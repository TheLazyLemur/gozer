package gozer

import (
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Node is one piece of a render tree: an element, text, markup, a component,
// or an attribute/event that belongs to the enclosing element.
type Node interface{ isNode() }

type element struct {
	tag   string
	items []Node
}

type (
	textNode    string
	rawNode     string
	commentNode struct{}
	fragment    []Node
	keyNode     struct{ key any }
	refNode     struct{ ref *ElementRef }
	attrNode    struct {
		name string
		val  any
	}
)

type eventNode struct {
	name    string
	fn      any // func() or func(Event)
	prevent bool
	stop    bool
	owner   StateHaser
	format  string // bind only
}

type compNode struct {
	typ reflect.Type
	mk  func() Component
	set func(Component)
	key any
}

func (element) isNode()     {}
func (textNode) isNode()    {}
func (rawNode) isNode()     {}
func (commentNode) isNode() {}
func (fragment) isNode()    {}
func (keyNode) isNode()     {}
func (refNode) isNode()     {}
func (attrNode) isNode()    {}
func (*eventNode) isNode()  {}
func (*compNode) isNode()   {}

// El builds an element. Items may be children, attributes, events, keys or refs.
func El(tag string, items ...Node) Node { return element{tag, items} }

// Text is an escaped text node.
func Text(s string) Node { return textNode(s) }

// Raw inserts trusted HTML without escaping (Blazor's MarkupString).
func Raw(html string) Node { return rawNode(html) }

// Frag groups nodes without a wrapper element.
func Frag(nodes ...Node) Node { return fragment(nodes) }

// Attr sets an attribute. true renders a bare attribute, false or nil omits it.
func Attr(name string, val any) Node { return attrNode{name, val} }

// Attrs splats a map of attributes (Blazor's @attributes).
func Attrs(m map[string]any) Node {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make(fragment, len(keys))
	for i, k := range keys {
		out[i] = attrNode{k, m[k]}
	}
	return out
}

// Key gives an element a stable identity across renders (@key).
func Key(k any) Node { return keyNode{k} }

// Ref captures a reference to the rendered DOM element (@ref).
func Ref(r *ElementRef) Node { return refNode{r} }

// Keyed attaches a key to an element or component node.
func Keyed(key any, n Node) Node {
	switch v := n.(type) {
	case *compNode:
		cp := *v
		cp.key = key
		return &cp
	case element:
		return element{v.tag, append(v.items[:len(v.items):len(v.items)], keyNode{key})}
	}
	panic(fmt.Sprintf("gozer: Keyed needs an element or component, got %T", n))
}

// X renders an arbitrary value: Nodes pass through, everything else becomes text.
func X(v any) Node {
	switch v := v.(type) {
	case nil:
		return nil
	case Node:
		return v
	case string:
		return textNode(v)
	case []Node:
		return fragment(v)
	case error:
		return textNode(v.Error())
	case fmt.Stringer:
		return textNode(v.String())
	}
	return textNode(fmt.Sprint(v))
}

// S concatenates values as text; used for interpolated attribute values.
func S(parts ...any) string {
	var b strings.Builder
	for _, p := range parts {
		fmt.Fprint(&b, p)
	}
	return b.String()
}

// Opt configures an event handler or binding.
type Opt func(*eventNode)

var (
	// Prevent calls preventDefault on the DOM event (@onx:preventDefault).
	Prevent Opt = func(e *eventNode) { e.prevent = true }
	// Stop calls stopPropagation on the DOM event (@onx:stopPropagation).
	Stop Opt = func(e *eventNode) { e.stop = true }
)

// OwnedBy re-renders owner after the handler runs, like Blazor's EventCallback target.
func OwnedBy(owner StateHaser) Opt { return func(e *eventNode) { e.owner = owner } }

// BindEvent picks the DOM event a binding listens to (default "change").
func BindEvent(name string) Opt {
	return func(e *eventNode) { e.name = strings.TrimPrefix(name, "on") }
}

// BindFormat sets the time layout used by a binding (@bind:format).
func BindFormat(layout string) Opt { return func(e *eventNode) { e.format = layout } }

// On attaches an event handler. fn is func() or func(Event).
func On(name string, fn any, opts ...Opt) Node {
	switch f := fn.(type) {
	case nil:
		return nil
	case func():
		if f == nil {
			return nil
		}
	case func(Event):
		if f == nil {
			return nil
		}
	default:
		panic(fmt.Sprintf("gozer: handler for %q must be func() or func(gozer.Event), got %T", name, fn))
	}
	e := &eventNode{name: strings.TrimPrefix(name, "on"), fn: fn}
	for _, o := range opts {
		o(e)
	}
	return e
}

// Bind two-way binds an input to *ptr (@bind). Bools bind to "checked".
func Bind[T any](ptr *T, opts ...Opt) Node {
	e := &eventNode{name: "change"}
	for _, o := range opts {
		o(e)
	}
	if b, ok := any(ptr).(*bool); ok {
		e.fn = func(ev Event) { *b = ev.Checked }
		return fragment{attrNode{"checked", *b}, e}
	}
	e.fn = func(ev Event) {
		// unparsable input keeps the old value; the re-render reverts the DOM
		_ = ParseValue(ptr, ev.Value, e.format)
	}
	return fragment{attrNode{"value", FormatValue(*ptr, e.format)}, e}
}

// FormatValue renders a bound value as an input string.
func FormatValue(v any, layout string) string {
	switch v := v.(type) {
	case string:
		return v
	case time.Time:
		if v.IsZero() {
			return ""
		}
		if layout == "" {
			layout = time.DateOnly
		}
		return v.Format(layout)
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	case float32:
		return strconv.FormatFloat(float64(v), 'f', -1, 32)
	}
	return fmt.Sprint(v)
}

// ParseValue parses s into the value ptr points to.
func ParseValue(ptr any, s, layout string) error {
	if t, ok := ptr.(*time.Time); ok {
		if layout == "" {
			layout = time.DateOnly
		}
		if s == "" {
			*t = time.Time{}
			return nil
		}
		v, err := time.Parse(layout, s)
		if err == nil {
			*t = v
		}
		return err
	}
	return setString(reflect.ValueOf(ptr).Elem(), s)
}

func setString(v reflect.Value, s string) error {
	switch v.Kind() {
	case reflect.String:
		v.SetString(s)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		n, err := strconv.ParseInt(strings.TrimSpace(s), 10, v.Type().Bits())
		if err != nil {
			return err
		}
		v.SetInt(n)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		n, err := strconv.ParseUint(strings.TrimSpace(s), 10, v.Type().Bits())
		if err != nil {
			return err
		}
		v.SetUint(n)
	case reflect.Float32, reflect.Float64:
		n, err := strconv.ParseFloat(strings.TrimSpace(s), v.Type().Bits())
		if err != nil {
			return err
		}
		v.SetFloat(n)
	case reflect.Bool:
		b, err := strconv.ParseBool(s)
		if err != nil {
			return err
		}
		v.SetBool(b)
	default:
		return fmt.Errorf("gozer: cannot bind to %s", v.Type())
	}
	return nil
}

// C renders a component of type T. set assigns its parameters; it runs on a fresh
// instance the first time and on the live instance on every later parent render,
// so only the parameters you assign are touched (Blazor's SetParameters).
func C[T any, PT interface {
	*T
	Component
}](set func(PT)) Node {
	return &compNode{
		typ: reflect.TypeFor[T](),
		mk:  func() Component { return PT(new(T)) },
		set: func(c Component) {
			if set != nil {
				set(c.(PT))
			}
		},
	}
}

// Factory returns a constructor for T, e.g. for Router.DefaultLayout or App.Root.
func Factory[T any, PT interface {
	*T
	Component
}]() func() Component {
	return func() Component { return PT(new(T)) }
}

// Event carries DOM event data to handlers.
type Event struct {
	Type     string              `json:"type"`
	Value    string              `json:"value"`
	Checked  bool                `json:"checked"`
	Key      string              `json:"key"`
	Code     string              `json:"code"`
	Repeat   bool                `json:"repeat"`
	CtrlKey  bool                `json:"ctrlKey"`
	ShiftKey bool                `json:"shiftKey"`
	AltKey   bool                `json:"altKey"`
	MetaKey  bool                `json:"metaKey"`
	ClientX  float64             `json:"clientX"`
	ClientY  float64             `json:"clientY"`
	OffsetX  float64             `json:"offsetX"`
	OffsetY  float64             `json:"offsetY"`
	Button   int                 `json:"button"`
	Buttons  int                 `json:"buttons"`
	Detail   int                 `json:"detail"`
	DeltaX   float64             `json:"deltaX"`
	DeltaY   float64             `json:"deltaY"`
	Form     map[string][]string `json:"form"`
}

// ElementRef points at a rendered DOM element; pass it to JS interop calls.
type ElementRef struct {
	id   int
	circ *Circuit
}

func (r ElementRef) MarshalJSON() ([]byte, error) {
	return []byte(`{"__gzref":` + strconv.Itoa(r.id) + `}`), nil
}

// Valid reports whether the element has been rendered.
func (r ElementRef) Valid() bool { return r.circ != nil }
