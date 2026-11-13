package gozer

import (
	"fmt"
	"math"
	"reflect"
	"sort"
)

type kind uint8

const (
	kElem kind = iota
	kText
	kRaw
	kComment
	kComp // virtual: owns no DOM node, its DOM is the flattened DOM of its children
)

// mnode is the server's copy of what is mounted in the browser.
type mnode struct {
	kind     kind
	id       int // DOM node id; 0 for components
	tag      string
	text     string // text or raw html
	attrs    map[string]string
	props    map[string]any
	events   map[string]*eventNode
	key      any
	children []*mnode
	parent   *mnode
	owner    *instance // component whose render produced this node
	inst     *instance // kComp only
	ctyp     reflect.Type
}

// Ops sent to the client; see client/gozer.js.
const (
	opInsert = iota // parent, before, specs
	opRemove        // id
	opMove          // parent, id, before
	opAttr          // id, name, value|null
	opProp          // id, name, value|null
	opText          // id, text
	opRaw           // id, html
	opListen        // id, event, flags (-1 removes)
	opNav           // url, replace, force
	opTitle         // title
	opHead          // html for <head>, replacing earlier HeadContent output
)

type spec struct {
	I int               `json:"i"`
	T string            `json:"t,omitempty"`
	X *string           `json:"x,omitempty"`
	R *string           `json:"r,omitempty"`
	A map[string]string `json:"a,omitempty"`
	P map[string]any    `json:"p,omitempty"`
	E map[string]int    `json:"e,omitempty"`
	C []spec            `json:"c,omitempty"`
}

type elemParts struct {
	attrs  map[string]string
	props  map[string]any
	events map[string]*eventNode
	key    any
	ref    *ElementRef
	kids   []vnode
}

// vnode is a freshly rendered node, validated and pre-split before any mutation,
// so framework panics (bad keys, stray attributes) happen before the tree changes.
type vnode struct {
	n     Node
	parts *elemParts
	key   any
}

func flatten(n Node, out []Node) []Node {
	switch v := n.(type) {
	case nil:
	case fragment:
		for _, c := range v {
			out = flatten(c, out)
		}
	case attrNode, *eventNode, keyNode, refNode:
		panic(fmt.Sprintf("gozer: %T outside an element", v))
	default:
		out = append(out, v)
	}
	return out
}

func prepare(nodes []Node) []vnode {
	out := make([]vnode, len(nodes))
	var seen map[any]bool
	for i, n := range nodes {
		v := vnode{n: n}
		switch t := n.(type) {
		case element:
			p := splitElement(t)
			v.parts, v.key = &p, p.key
		case *compNode:
			v.key = t.key
		}
		if v.key != nil {
			if seen == nil {
				seen = map[any]bool{}
			}
			if seen[v.key] {
				panic(fmt.Sprintf("gozer: duplicate key %v", v.key))
			}
			seen[v.key] = true
		}
		out[i] = v
	}
	return out
}

func isProp(tag, name string) bool {
	return (name == "value" && (tag == "input" || tag == "textarea" || tag == "select")) ||
		(name == "checked" && tag == "input")
}

func splitElement(e element) elemParts {
	var p elemParts
	var kids []Node
	var walk func([]Node)
	walk = func(items []Node) {
		for _, it := range items {
			switch v := it.(type) {
			case nil:
			case fragment:
				walk(v)
			case attrNode:
				p.setAttr(e.tag, v)
			case *eventNode:
				if p.events == nil {
					p.events = map[string]*eventNode{}
				}
				p.events[v.name] = v
			case keyNode:
				p.key = v.key
			case refNode:
				p.ref = v.ref
			default:
				kids = append(kids, v)
			}
		}
	}
	walk(e.items)
	p.kids = prepare(kids)
	return p
}

func (p *elemParts) setAttr(tag string, a attrNode) {
	if isProp(tag, a.name) {
		if p.props == nil {
			p.props = map[string]any{}
		}
		if a.name == "checked" {
			b, _ := a.val.(bool)
			p.props["checked"] = b
		} else {
			p.props["value"] = FormatValue(a.val, "")
		}
		return
	}
	if p.attrs == nil {
		p.attrs = map[string]string{}
	}
	switch v := a.val.(type) {
	case nil:
		delete(p.attrs, a.name)
	case bool:
		if v {
			p.attrs[a.name] = ""
		} else {
			delete(p.attrs, a.name)
		}
	case string:
		p.attrs[a.name] = v
	default:
		p.attrs[a.name] = fmt.Sprint(v)
	}
}

func matches(m *mnode, n Node) bool {
	switch v := n.(type) {
	case textNode:
		return m.kind == kText
	case rawNode:
		return m.kind == kRaw
	case commentNode:
		return m.kind == kComment
	case element:
		return m.kind == kElem && m.tag == v.tag
	case *compNode:
		return m.kind == kComp && m.ctyp == v.typ
	}
	panic(fmt.Sprintf("gozer: unexpected node %T", n))
}

func (c *Circuit) emit(op ...any) { c.ops = append(c.ops, op) }

func (c *Circuit) assign(m *mnode) {
	c.nextID++
	m.id = c.nextID
	c.nodes[m.id] = m
}

// create builds a detached subtree; its DOM goes out as one insert op by the caller.
func (c *Circuit) create(v vnode, parent *mnode, owner *instance) *mnode {
	c.building++
	defer func() { c.building-- }()
	m := &mnode{parent: parent, owner: owner, key: v.key}
	switch n := v.n.(type) {
	case textNode:
		m.kind, m.text = kText, string(n)
		c.assign(m)
	case rawNode:
		m.kind, m.text = kRaw, string(n)
		c.assign(m)
	case commentNode:
		m.kind = kComment
		c.assign(m)
	case element:
		m.kind, m.tag = kElem, n.tag
		c.assign(m)
		p := v.parts
		m.attrs, m.props, m.events = p.attrs, p.props, p.events
		if p.ref != nil {
			p.ref.id, p.ref.circ = m.id, c
		}
		for _, k := range p.kids {
			m.children = append(m.children, c.create(k, m, owner))
		}
	case *compNode:
		m.kind, m.ctyp = kComp, n.typ
		c.mount(m, n, owner)
	}
	return m
}

func (c *Circuit) mount(m *mnode, n *compNode, parent *instance) {
	comp := n.mk()
	inst := &instance{c: comp, node: m, parent: parent, circ: c}
	if parent != nil {
		inst.depth = parent.depth + 1
	}
	m.inst = inst
	if b, ok := comp.(baser); ok {
		b.gozerBase().inst = inst
	}
	c.guard(inst, func() {
		c.inject(inst)
		n.set(comp)
		c.cascade(inst)
		if i, ok := comp.(Initer); ok {
			i.OnInit()
		}
		if p, ok := comp.(ParamsSetter); ok {
			p.OnParametersSet()
		}
	})
	c.renderInstance(inst)
}

func (c *Circuit) renderInstance(inst *instance) {
	inst.dirty.Store(false)
	var vs []vnode
	if !inst.failed {
		c.guard(inst, func() { vs = prepare(flatten(inst.c.Render(), nil)) })
	}
	if len(vs) == 0 || inst.failed {
		// a component always owns at least one DOM node so siblings have an anchor
		vs = []vnode{{n: commentNode{}}}
	}
	c.reconcile(inst.node, vs, inst)
	c.rendered = append(c.rendered, inst)
}

func (c *Circuit) shouldRender(inst *instance) bool {
	g, ok := inst.c.(RenderGate)
	if !ok {
		return true
	}
	r := true
	c.guard(inst, func() { r = g.ShouldRender() })
	return r
}

func (c *Circuit) update(m *mnode, v vnode, owner *instance) {
	m.owner = owner
	switch n := v.n.(type) {
	case textNode:
		if m.text != string(n) {
			m.text = string(n)
			c.emit(opText, m.id, m.text)
		}
	case rawNode:
		if m.text != string(n) {
			m.text = string(n)
			c.emit(opRaw, m.id, m.text)
		}
	case commentNode:
	case element:
		c.diffParts(m, v.parts)
		c.reconcile(m, v.parts.kids, owner)
	case *compNode:
		inst := m.inst
		if inst.failed {
			return
		}
		ok := c.guard(inst, func() {
			n.set(inst.c)
			c.cascade(inst)
			if p, ok := inst.c.(ParamsSetter); ok {
				p.OnParametersSet()
			}
		})
		if ok && c.shouldRender(inst) {
			c.renderInstance(inst)
		} else {
			inst.dirty.Store(false)
		}
	}
}

func evFlags(e *eventNode) int {
	f := 0
	if e.prevent {
		f |= 1
	}
	if e.stop {
		f |= 2
	}
	return f
}

func (c *Circuit) diffParts(m *mnode, p *elemParts) {
	for k, v := range p.attrs {
		if old, ok := m.attrs[k]; !ok || old != v {
			c.emit(opAttr, m.id, k, v)
		}
	}
	for k := range m.attrs {
		if _, ok := p.attrs[k]; !ok {
			c.emit(opAttr, m.id, k, nil)
		}
	}
	for k, v := range p.props {
		if old, ok := m.props[k]; !ok || old != v {
			c.emit(opProp, m.id, k, v)
		}
	}
	for k := range m.props {
		if _, ok := p.props[k]; !ok {
			c.emit(opProp, m.id, k, nil)
		}
	}
	for k, e := range p.events {
		if old, ok := m.events[k]; !ok || evFlags(old) != evFlags(e) {
			c.emit(opListen, m.id, k, evFlags(e))
		}
	}
	for k := range m.events {
		if _, ok := p.events[k]; !ok {
			c.emit(opListen, m.id, k, -1)
		}
	}
	if p.ref != nil {
		p.ref.id, p.ref.circ = m.id, c
	}
	m.attrs, m.props, m.events = p.attrs, p.props, p.events
}

// reconcile turns container's children into vs, emitting DOM ops.
// Matching: keyed nodes by key, unkeyed nodes in order by type.
// Placement walks right to left so every insert/move has a final-position anchor.
func (c *Circuit) reconcile(container *mnode, vs []vnode, owner *instance) {
	old := container.children
	if c.building > 0 {
		if len(old) > 0 {
			panic("gozer: re-render inside a subtree that is still being built")
		}
		for _, v := range vs {
			container.children = append(container.children, c.create(v, container, owner))
		}
		return
	}
	used := make([]bool, len(old))
	var keyed map[any]int
	for i, o := range old {
		if o.key != nil {
			if keyed == nil {
				keyed = map[any]int{}
			}
			keyed[o.key] = i
		}
	}
	match := make([]int, len(vs))
	cursor := 0
	for i, v := range vs {
		match[i] = -1
		if v.key != nil {
			if j, ok := keyed[v.key]; ok && !used[j] && matches(old[j], v.n) {
				used[j], match[i] = true, j
			}
			continue
		}
		for j := cursor; j < len(old); j++ {
			if !used[j] && old[j].key == nil && matches(old[j], v.n) {
				used[j], match[i], cursor = true, j, j+1
				break
			}
		}
	}
	for j, o := range old {
		if !used[j] {
			c.remove(o)
		}
	}

	kids := make([]*mnode, len(vs))
	for i, j := range match {
		if j >= 0 {
			kids[i] = old[j]
		}
	}
	container.children = kids
	parentDOM := domParent(container)
	anchor := c.endAnchor(container)
	lastOld := math.MaxInt
	for i := len(vs) - 1; i >= 0; i-- {
		if j := match[i]; j >= 0 {
			m := kids[i]
			c.update(m, vs[i], owner)
			if j > lastOld {
				for _, d := range domNodes(m, nil) {
					c.emit(opMove, parentDOM, d.id, anchor)
				}
			} else {
				lastOld = j
			}
		} else {
			m := c.create(vs[i], container, owner)
			kids[i] = m
			c.emit(opInsert, parentDOM, anchor, specs(m, nil))
		}
		anchor = firstDOM(kids[i]).id
	}
}

func (c *Circuit) remove(m *mnode) {
	for _, d := range domNodes(m, nil) {
		c.emit(opRemove, d.id)
	}
	c.dispose(m)
}

func (c *Circuit) dispose(m *mnode) {
	for _, ch := range m.children {
		c.dispose(ch)
	}
	if m.id != 0 {
		delete(c.nodes, m.id)
	}
	if m.kind == kComp {
		inst := m.inst
		inst.disposed = true
		if d, ok := inst.c.(Disposer); ok {
			func() {
				defer func() {
					if r := recover(); r != nil {
						c.logf("panic in Dispose of %T: %v", inst.c, r)
					}
				}()
				d.Dispose()
			}()
		}
	}
}

func domNodes(m *mnode, out []*mnode) []*mnode {
	if m.kind != kComp {
		return append(out, m)
	}
	for _, ch := range m.children {
		out = domNodes(ch, out)
	}
	return out
}

func firstDOM(m *mnode) *mnode {
	for m.kind == kComp {
		if len(m.children) == 0 {
			panic("gozer: component without DOM nodes")
		}
		m = m.children[0]
	}
	return m
}

func domParent(m *mnode) int {
	for m.kind == kComp {
		m = m.parent
	}
	return m.id
}

// endAnchor is the DOM id that container's range must stay in front of (0 = append).
func (c *Circuit) endAnchor(container *mnode) int {
	m := container
	for m.kind == kComp {
		p := m.parent
		idx := -1
		for i, ch := range p.children {
			if ch == m {
				idx = i
				break
			}
		}
		if idx < 0 {
			panic("gozer: node not found in its parent")
		}
		// shortcut: linear scan for the sibling index, fine for normal fan-out.
		for _, sib := range p.children[idx+1:] {
			if sib == nil {
				panic("gozer: unplaced sibling during reconcile")
			}
			return firstDOM(sib).id
		}
		m = p
	}
	return 0
}

func specs(m *mnode, out []spec) []spec {
	switch m.kind {
	case kComp:
		for _, ch := range m.children {
			out = specs(ch, out)
		}
		return out
	case kText:
		t := m.text
		return append(out, spec{I: m.id, X: &t})
	case kRaw:
		t := m.text
		return append(out, spec{I: m.id, R: &t})
	case kComment:
		return append(out, spec{I: m.id})
	}
	s := spec{I: m.id, T: m.tag, A: m.attrs, P: m.props}
	if len(m.events) > 0 {
		s.E = make(map[string]int, len(m.events))
		for k, e := range m.events {
			s.E[k] = evFlags(e)
		}
	}
	for _, ch := range m.children {
		s.C = specs(ch, s.C)
	}
	return append(out, s)
}

// renderDirty re-renders invalidated components, parents first so a child
// re-rendered by its parent is not rendered twice.
func (c *Circuit) renderDirty() {
	for iter := 0; ; iter++ {
		c.mu.Lock()
		q := c.dirtyq
		c.dirtyq = nil
		c.mu.Unlock()
		if len(q) == 0 {
			return
		}
		if iter > 100 {
			c.fatal = fmt.Errorf("gozer: render loop did not settle after %d passes", iter)
			return
		}
		sort.SliceStable(q, func(i, j int) bool { return q[i].depth < q[j].depth })
		for _, inst := range q {
			if inst.disposed || !inst.dirty.Load() {
				continue
			}
			if c.shouldRender(inst) {
				c.renderInstance(inst)
			} else {
				inst.dirty.Store(false)
			}
		}
	}
}
