package gozer

import (
	"fmt"
	"net/url"
	"reflect"
	"sort"
	"strconv"
	"strings"
)

type segKind int

const (
	segCatchAll segKind = iota
	segParam
	segTyped
	segLit
)

type segment struct {
	kind     segKind
	lit      string // literal text or parameter name
	typ      string // int, float, bool for segTyped
	optional bool
}

type route struct {
	pattern string
	segs    []segment
	mk      func() Component
	typ     reflect.Type
	layout  func() Component
}

var routes []*route

// RegisterPage adds a routable page (@page). layout may be nil.
// Patterns: /users/{id}, /users/{id:int}, /docs/{*path}, /list/{page?}.
func RegisterPage(pattern string, mk func() Component, layout func() Component) {
	r := &route{pattern: pattern, mk: mk, typ: reflect.TypeOf(mk()).Elem(), layout: layout}
	for _, s := range strings.Split(strings.Trim(pattern, "/"), "/") {
		if s == "" {
			continue
		}
		if !strings.HasPrefix(s, "{") {
			r.segs = append(r.segs, segment{kind: segLit, lit: strings.ToLower(s)})
			continue
		}
		s = strings.TrimSuffix(strings.TrimPrefix(s, "{"), "}")
		seg := segment{kind: segParam}
		if strings.HasPrefix(s, "*") {
			seg.kind, s = segCatchAll, s[1:]
		}
		if strings.HasSuffix(s, "?") {
			seg.optional, s = true, strings.TrimSuffix(s, "?")
		}
		if name, typ, ok := strings.Cut(s, ":"); ok {
			if typ != "int" && typ != "float" && typ != "bool" {
				panic(fmt.Sprintf("gozer: route %q: unknown constraint %q", pattern, typ))
			}
			seg.kind, seg.typ, s = segTyped, typ, name
		}
		seg.lit = s
		r.segs = append(r.segs, seg)
	}
	routes = append(routes, r)
	// most specific first: compare segment kinds left to right, then length
	sort.SliceStable(routes, func(i, j int) bool {
		a, b := routes[i].segs, routes[j].segs
		for k := 0; k < len(a) && k < len(b); k++ {
			if a[k].kind != b[k].kind {
				return a[k].kind > b[k].kind
			}
		}
		return len(a) > len(b)
	})
}

func matchRoute(path string) (*route, map[string]string) {
	var parts []string
	for _, p := range strings.Split(strings.Trim(path, "/"), "/") {
		if p != "" {
			parts = append(parts, p)
		}
	}
	for _, r := range routes {
		if params, ok := r.match(parts); ok {
			return r, params
		}
	}
	return nil, nil
}

func (r *route) match(parts []string) (map[string]string, bool) {
	params := map[string]string{}
	for i, s := range r.segs {
		if s.kind == segCatchAll {
			params[s.lit] = strings.Join(parts[min(i, len(parts)):], "/")
			return params, true
		}
		if i >= len(parts) {
			if s.optional {
				continue
			}
			return nil, false
		}
		p, err := url.PathUnescape(parts[i])
		if err != nil {
			return nil, false
		}
		switch s.kind {
		case segLit:
			if !strings.EqualFold(p, s.lit) {
				return nil, false
			}
		case segTyped:
			if !typeOK(s.typ, p) {
				return nil, false
			}
			params[s.lit] = p
		case segParam:
			params[s.lit] = p
		}
	}
	return params, len(parts) <= len(r.segs)
}

func typeOK(typ, s string) bool {
	var err error
	switch typ {
	case "int":
		_, err = strconv.ParseInt(s, 10, 64)
	case "float":
		_, err = strconv.ParseFloat(s, 64)
	case "bool":
		_, err = strconv.ParseBool(s)
	}
	return err == nil
}

// setRouteParams assigns route values to fields by name (case-insensitive) or
// `route:"name"` tag, and query values to fields tagged `query:"name"`.
func setRouteParams(c Component, params map[string]string, q url.Values) {
	v := reflect.ValueOf(c).Elem()
	t := v.Type()
	for i := range t.NumField() {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		if name, ok := f.Tag.Lookup("query"); ok {
			if s := q.Get(name); s != "" {
				_ = setString(v.Field(i), s) // shortcut: bad query values leave the field unchanged
			} else {
				v.Field(i).SetZero()
			}
			continue
		}
		name := f.Tag.Get("route")
		for k, s := range params {
			if strings.EqualFold(k, name) || (name == "" && strings.EqualFold(k, f.Name)) {
				if err := setString(v.Field(i), s); err != nil {
					panic(fmt.Sprintf("gozer: route value %q for %s.%s: %v", s, t.Name(), f.Name, err))
				}
			}
		}
	}
}

// Layout is a component that wraps pages. Embed LayoutBase.
type Layout interface {
	Component
	SetBody(Node)
}

// LayoutBase is embedded by layouts; render Body where the page goes.
type LayoutBase struct {
	Base
	Body Node
}

func (l *LayoutBase) SetBody(n Node) { l.Body = n }

// Authorizer pages decide who may see them ([Authorize]).
type Authorizer interface{ Authorize(u *User) bool }

// Router renders the page that matches the current URL.
type Router struct {
	Base
	DefaultLayout func() Component
	NotFound      Node
	NotAuthorized Node
	unsub         func()
}

func (r *Router) OnInit() { r.unsub = r.Circuit().Nav.OnChange(r.StateHasChanged) }

func (r *Router) Dispose() {
	if r.unsub != nil {
		r.unsub()
	}
}

func (r *Router) Render() Node {
	c := r.Circuit()
	u := c.Nav.uri
	rt, params := matchRoute(u.Path)
	layout := r.DefaultLayout
	var body Node
	switch {
	case rt == nil && !c.Nav.initial:
		// unknown in-app link: let the server handle it (static file, other handler)
		c.Nav.NavigateTo(u.RequestURI(), ForceLoad, Replace)
		return nil
	case rt == nil:
		c.mu.Lock()
		c.notFnd = true
		c.mu.Unlock()
		body = r.NotFound
		if body == nil {
			body = El("p", Text("Sorry, there's nothing at this address."))
		}
	default:
		if rt.layout != nil {
			layout = rt.layout
		}
		if a, ok := rt.mk().(Authorizer); ok && !a.Authorize(c.user) {
			body = r.NotAuthorized
			if body == nil {
				body = El("p", Text("You are not authorized to access this resource."))
			}
			break
		}
		q := u.Query()
		body = &compNode{typ: rt.typ, mk: rt.mk, set: func(p Component) { setRouteParams(p, params, q) }}
	}
	if layout == nil {
		return body
	}
	return &compNode{typ: reflect.TypeOf(layout()).Elem(), mk: layout, set: func(l Component) {
		lay, ok := l.(Layout)
		if !ok {
			panic(fmt.Sprintf("gozer: layout %T must embed gozer.LayoutBase", l))
		}
		lay.SetBody(body)
	}}
}

// NavLinkMatch selects how NavLink decides it is active.
type NavLinkMatch int

const (
	MatchPrefix NavLinkMatch = iota
	MatchAll
)

// NavLink is an <a> that gets ActiveClass when it matches the current URL.
type NavLink struct {
	Base
	Href         string
	Class        string
	ActiveClass  string
	Match        NavLinkMatch
	Attrs        map[string]any
	ChildContent Node
	unsub        func()
}

func (n *NavLink) OnInit() { n.unsub = n.Circuit().Nav.OnChange(n.StateHasChanged) }

func (n *NavLink) Dispose() {
	if n.unsub != nil {
		n.unsub()
	}
}

func (n *NavLink) Render() Node {
	nav := n.Circuit().Nav
	target, err := url.Parse(n.Href)
	active := false
	if err == nil {
		want := strings.TrimSuffix(nav.uri.ResolveReference(target).Path, "/")
		cur := strings.TrimSuffix(nav.uri.Path, "/")
		active = strings.EqualFold(cur, want) ||
			(n.Match == MatchPrefix && strings.HasPrefix(strings.ToLower(cur), strings.ToLower(want)+"/"))
	}
	class := n.Class
	if active {
		ac := n.ActiveClass
		if ac == "" {
			ac = "active"
		}
		class = strings.TrimSpace(class + " " + ac)
	}
	var cls Node
	if class != "" {
		cls = Attr("class", class)
	}
	return El("a", Attrs(n.Attrs), Attr("href", n.Href), cls, n.ChildContent)
}

// PageTitle sets document.title to its text content.
type PageTitle struct {
	Base
	ChildContent Node
}

func (p *PageTitle) Render() Node {
	p.Circuit().setTitle(p, textOf(p.ChildContent), false)
	return nil
}

func (p *PageTitle) Dispose() { p.Circuit().setTitle(p, "", true) }

// HeadContent puts elements in the document <head> while it is rendered
// (meta description, canonical link, Open Graph tags). Prerendered pages carry
// them in their HTML, so crawlers see them.
type HeadContent struct {
	Base
	ChildContent Node
}

func (h *HeadContent) Render() Node {
	var b strings.Builder
	for _, v := range prepare(flatten(h.ChildContent, nil)) {
		if _, ok := v.n.(textNode); ok {
			continue // whitespace between tags
		}
		if _, ok := v.n.(element); !ok {
			panic(fmt.Sprintf("gozer: HeadContent takes elements, got %T", v.n))
		}
		m := staticTree(v)
		m.attrs["data-gz-head"] = "" // lets the client find and replace them on navigation
		writeHTML(&b, m, false)
	}
	h.Circuit().setHead(h, b.String(), false)
	return nil
}

func (h *HeadContent) Dispose() { h.Circuit().setHead(h, "", true) }

// staticTree turns rendered nodes into an unmounted tree for writeHTML.
func staticTree(v vnode) *mnode {
	switch n := v.n.(type) {
	case textNode:
		return &mnode{kind: kText, text: string(n)}
	case rawNode:
		return &mnode{kind: kRaw, text: string(n)}
	case element:
		m := &mnode{kind: kElem, tag: n.tag, attrs: map[string]string{}, props: v.parts.props}
		for k, a := range v.parts.attrs {
			m.attrs[k] = a
		}
		for _, k := range v.parts.kids {
			m.children = append(m.children, staticTree(k))
		}
		return m
	}
	panic(fmt.Sprintf("gozer: HeadContent cannot render %T", v.n))
}

func textOf(n Node) string {
	var b strings.Builder
	var walk func(Node)
	walk = func(n Node) {
		switch v := n.(type) {
		case textNode:
			b.WriteString(string(v))
		case fragment:
			for _, c := range v {
				walk(c)
			}
		case element:
			for _, c := range v.items {
				walk(c)
			}
		}
	}
	walk(n)
	return strings.TrimSpace(b.String())
}

// CascadingValue makes Value available to descendants with a `cascade` field tag.
type CascadingValue struct {
	Base
	Value        any
	Name         string
	ChildContent Node
}

func (c *CascadingValue) Render() Node { return c.ChildContent }

// ErrorBoundary shows ErrorContent instead of its children after they panic.
type ErrorBoundary struct {
	Base
	ChildContent Node
	ErrorContent func(err error) Node
	err          error
}

// Err is the captured error, nil while healthy.
func (e *ErrorBoundary) Err() error { return e.err }

// Recover clears the error and renders the children again.
func (e *ErrorBoundary) Recover() {
	e.err = nil
	e.StateHasChanged()
}

func (e *ErrorBoundary) Render() Node {
	if e.err == nil {
		return e.ChildContent
	}
	if e.ErrorContent != nil {
		return e.ErrorContent(e.err)
	}
	return El("div", Attr("class", "gozer-error-boundary"), Text("An error has occurred."))
}

// User is the authenticated principal; nil means anonymous.
type User struct {
	Name   string
	Roles  []string
	Claims map[string]string
}

// InRole reports whether the user has any of roles.
func (u *User) InRole(roles ...string) bool {
	if u == nil {
		return false
	}
	for _, r := range roles {
		for _, have := range u.Roles {
			if r == have {
				return true
			}
		}
	}
	return false
}

// AuthorizeView renders Authorized (or ChildContent) for signed-in users
// matching Roles, otherwise NotAuthorized.
type AuthorizeView struct {
	Base
	Roles         []string
	Authorized    Node
	ChildContent  Node
	NotAuthorized Node
}

func (a *AuthorizeView) Render() Node {
	u := a.Circuit().user
	if u == nil || (len(a.Roles) > 0 && !u.InRole(a.Roles...)) {
		return a.NotAuthorized
	}
	if a.Authorized != nil {
		return a.Authorized
	}
	return a.ChildContent
}
