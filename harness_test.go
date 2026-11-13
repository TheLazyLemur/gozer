package gozer

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"
)

// fnode/fakeDOM mirror client/gozer.js so tests can check that the ops the
// server emits rebuild exactly the tree the server thinks is mounted.
type fnode struct {
	id     int
	tag    string
	text   *string
	raw    *string
	attrs  map[string]string
	props  map[string]any
	events map[string]int
	kids   []*fnode
	parent *fnode
}

type fakeDOM struct {
	nodes map[int]*fnode
	root  *fnode
}

func newFakeDOM() *fakeDOM {
	r := &fnode{id: rootID, tag: "div"}
	return &fakeDOM{nodes: map[int]*fnode{rootID: r}, root: r}
}

func (d *fakeDOM) build(s spec) *fnode {
	n := &fnode{id: s.I, tag: s.T, text: s.X, raw: s.R, attrs: map[string]string{}, props: map[string]any{}, events: map[string]int{}}
	for k, v := range s.A {
		n.attrs[k] = v
	}
	for k, v := range s.P {
		n.props[k] = v
	}
	for k, v := range s.E {
		n.events[k] = v
	}
	for _, c := range s.C {
		k := d.build(c)
		k.parent = n
		n.kids = append(n.kids, k)
	}
	d.nodes[s.I] = n
	return n
}

func (d *fakeDOM) forget(n *fnode) {
	delete(d.nodes, n.id)
	for _, k := range n.kids {
		d.forget(k)
	}
}

func (d *fakeDOM) detach(n *fnode) {
	if p := n.parent; p != nil {
		p.kids = slices.DeleteFunc(p.kids, func(k *fnode) bool { return k == n })
		n.parent = nil
	}
}

func (d *fakeDOM) insertBefore(parent, n *fnode, before int) {
	d.detach(n)
	n.parent = parent
	if before == 0 {
		parent.kids = append(parent.kids, n)
		return
	}
	b := d.nodes[before]
	i := slices.Index(parent.kids, b)
	if i < 0 {
		panic(fmt.Sprintf("anchor %d not a child of %d", before, parent.id))
	}
	parent.kids = slices.Insert(parent.kids, i, n)
}

func (d *fakeDOM) apply(data []byte) {
	var ops [][]json.RawMessage
	if err := json.Unmarshal(data, &ops); err != nil {
		panic(err)
	}
	num := func(r json.RawMessage) int {
		var n int
		must0(json.Unmarshal(r, &n))
		return n
	}
	str := func(r json.RawMessage) *string {
		var s *string
		must0(json.Unmarshal(r, &s))
		return s
	}
	for _, op := range ops {
		switch num(op[0]) {
		case opInsert:
			var ss []spec
			must0(json.Unmarshal(op[3], &ss))
			parent := d.nodes[num(op[1])]
			for _, s := range ss {
				d.insertBefore(parent, d.build(s), num(op[2]))
			}
		case opRemove:
			n := d.nodes[num(op[1])]
			d.detach(n)
			d.forget(n)
		case opMove:
			d.insertBefore(d.nodes[num(op[1])], d.nodes[num(op[2])], num(op[3]))
		case opAttr:
			n := d.nodes[num(op[1])]
			if v := str(op[3]); v == nil {
				delete(n.attrs, *str(op[2]))
			} else {
				n.attrs[*str(op[2])] = *v
			}
		case opProp:
			n := d.nodes[num(op[1])]
			var v any
			must0(json.Unmarshal(op[3], &v))
			if v == nil {
				delete(n.props, *str(op[2]))
			} else {
				n.props[*str(op[2])] = v
			}
		case opText:
			d.nodes[num(op[1])].text = str(op[2])
		case opRaw:
			d.nodes[num(op[1])].raw = str(op[2])
		case opListen:
			n := d.nodes[num(op[1])]
			if f := num(op[3]); f < 0 {
				delete(n.events, *str(op[2]))
			} else {
				n.events[*str(op[2])] = f
			}
		case opNav, opTitle, opHead:
		default:
			panic(fmt.Sprintf("unknown op %s", op[0]))
		}
	}
}

func must0(err error) {
	if err != nil {
		panic(err)
	}
}

func dumpMap[V any](b *strings.Builder, prefix string, m map[string]V) {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(b, " %s%s=%v", prefix, k, m[k])
	}
}

func dumpF(b *strings.Builder, n *fnode) {
	switch {
	case n.text != nil:
		fmt.Fprintf(b, "%q", *n.text)
	case n.raw != nil:
		fmt.Fprintf(b, "raw(%s)", *n.raw)
	case n.tag == "":
		b.WriteString("<!>")
	default:
		b.WriteString("<" + n.tag)
		dumpMap(b, "", n.attrs)
		dumpMap(b, ".", n.props)
		dumpMap(b, "@", n.events)
		b.WriteString(">")
		for _, k := range n.kids {
			dumpF(b, k)
		}
		b.WriteString("</>")
	}
}

func dumpM(b *strings.Builder, m *mnode) {
	switch m.kind {
	case kComp:
		for _, c := range m.children {
			dumpM(b, c)
		}
	case kText:
		fmt.Fprintf(b, "%q", m.text)
	case kRaw:
		fmt.Fprintf(b, "raw(%s)", m.text)
	case kComment:
		b.WriteString("<!>")
	default:
		b.WriteString("<" + m.tag)
		dumpMap(b, "", m.attrs)
		props := map[string]any{}
		for k, v := range m.props {
			props[k] = v
		}
		dumpMap(b, ".", props)
		ev := map[string]int{}
		for k, e := range m.events {
			ev[k] = evFlags(e)
		}
		dumpMap(b, "@", ev)
		b.WriteString(">")
		for _, c := range m.children {
			dumpM(b, c)
		}
		b.WriteString("</>")
	}
}

type harness struct {
	t       *testing.T
	c       *Circuit
	dom     *fakeDOM
	lastOps []byte // JSON of the ops applied by the last do
}

func newHarness(t *testing.T, path string, root func() Component) *harness {
	t.Helper()
	a := New()
	a.Root = root
	a.rootType = reflect.TypeOf(root()).Elem()
	req := httptest.NewRequest("GET", path, nil)
	h := &harness{t: t, c: newCircuit(a, req), dom: newFakeDOM()}
	t.Cleanup(h.c.Close)
	h.do(func() { h.c.start(requestURL(req, path)) })
	return h
}

// do runs f on the circuit goroutine, then applies the resulting batches to the fake DOM.
func (h *harness) do(f func()) {
	h.t.Helper()
	var bs []batch
	var want, got strings.Builder
	var fatal error
	h.c.sync(f)
	if !h.c.sync(func() {
		bs = h.c.unacked
		if len(bs) > 0 {
			h.c.ack(bs[len(bs)-1].seq)
		}
		dumpM(&want, h.c.root)
		fatal = h.c.fatal
	}) {
		h.t.Fatalf("circuit closed: %v", h.c.fatal)
	}
	if fatal != nil {
		h.t.Fatalf("circuit failed: %v", fatal)
	}
	h.lastOps = nil
	for _, b := range bs {
		data, err := json.Marshal(b.ops)
		if err != nil {
			h.t.Fatal(err)
		}
		h.lastOps = append(h.lastOps, data...)
		h.dom.apply(data)
	}
	dumpF(&got, h.dom.root)
	if want.String() != got.String() {
		h.t.Fatalf("DOM diverged from server tree\nserver: %s\nclient: %s", want.String(), got.String())
	}
}

// html is the client DOM as a compact string.
func (h *harness) html() string {
	var b strings.Builder
	dumpF(&b, h.dom.root)
	return b.String()
}

// find returns the first element whose subtree text contains s.
func (h *harness) find(tag, s string) *fnode {
	var hit *fnode
	var walk func(n *fnode)
	walk = func(n *fnode) {
		if hit != nil {
			return
		}
		if n.tag == tag && strings.Contains(textF(n), s) {
			hit = n
			return
		}
		for _, k := range n.kids {
			walk(k)
		}
	}
	walk(h.dom.root)
	if hit == nil {
		h.t.Fatalf("no <%s> containing %q in %s", tag, s, h.html())
	}
	return hit
}

func textF(n *fnode) string {
	if n.text != nil {
		return *n.text
	}
	var b strings.Builder
	for _, k := range n.kids {
		b.WriteString(textF(k))
	}
	return b.String()
}

func (h *harness) fire(n *fnode, name string, ev Event) {
	h.t.Helper()
	if _, ok := n.events[name]; !ok {
		h.t.Fatalf("element %d has no %s listener", n.id, name)
	}
	// like a browser, the element already shows what the user typed
	if _, ok := n.props["value"]; ok {
		n.props["value"] = ev.Value
	}
	if _, ok := n.props["checked"]; ok {
		n.props["checked"] = ev.Checked
	}
	h.do(func() { h.c.dispatchEvent(n.id, name, ev) })
}
