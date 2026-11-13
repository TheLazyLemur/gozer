package gozer

import (
	"bytes"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type fuzzRoot struct {
	Base
	gen func() Node
}

func (r *fuzzRoot) Render() Node { return r.gen() }

type fuzzChild struct {
	Base
	Seed  int
	Body  Node
	ticks int
}

func (f *fuzzChild) Render() Node {
	if f.Seed%4 == 0 {
		return nil // renders nothing: exercises the placeholder anchor
	}
	return Frag(Text(fmt.Sprintf("c%d/%d", f.Seed, f.ticks)), f.Body)
}

type treeGen struct {
	r        *rand.Rand
	children []*fuzzChild
	lists    int
}

type listKey struct{ list, id int }

func (g *treeGen) nodes(depth int) []Node {
	n := g.r.IntN(5)
	if g.r.IntN(4) == 0 {
		return g.keyed(depth)
	}
	out := make([]Node, 0, n)
	for range n {
		out = append(out, g.node(depth))
	}
	return out
}

func (g *treeGen) keyed(depth int) []Node {
	ids := g.r.Perm(8)[:g.r.IntN(8)]
	out := make([]Node, 0, len(ids))
	g.lists++
	for _, n := range ids {
		id := listKey{g.lists, n}
		if g.r.IntN(2) == 0 {
			out = append(out, El("li", Key(id), Text(fmt.Sprint("k", n)), g.maybeKids(depth)))
		} else {
			out = append(out, Keyed(id, g.comp(depth)))
		}
	}
	return out
}

func (g *treeGen) maybeKids(depth int) Node {
	if depth > 3 {
		return nil
	}
	return Frag(g.nodes(depth + 1)...)
}

func (g *treeGen) comp(depth int) Node {
	seed := g.r.IntN(6)
	body := g.maybeKids(depth)
	return C(func(p *fuzzChild) {
		p.Seed, p.Body = seed, body
		g.children = append(g.children, p)
	})
}

func (g *treeGen) node(depth int) Node {
	switch g.r.IntN(7) {
	case 0:
		return Text(fmt.Sprint("t", g.r.IntN(3)))
	case 1:
		return Raw(fmt.Sprintf("<b>%d</b>", g.r.IntN(2)))
	case 2:
		return g.comp(depth)
	case 3:
		return Frag(g.nodes(depth + 1)...)
	default:
		tags := []string{"div", "span", "p"}
		items := []Node{Attr("class", fmt.Sprint("c", g.r.IntN(3)))}
		if g.r.IntN(2) == 0 {
			opts := []Opt{}
			if g.r.IntN(2) == 0 {
				opts = append(opts, Prevent)
			}
			items = append(items, On("click", func() {}, opts...))
		}
		if g.r.IntN(3) == 0 {
			items = append(items, El("input", Attr("value", fmt.Sprint(g.r.IntN(3)))))
		}
		items = append(items, g.maybeKids(depth))
		return El(tags[g.r.IntN(len(tags))], items...)
	}
}

// The ops sent to the browser must rebuild exactly the server's tree,
// across random inserts, removals, keyed reorders and isolated child renders.
func TestRandomRendersKeepClientInSync(t *testing.T) {
	for seed := range 300 {
		t.Run(fmt.Sprint(seed), func(t *testing.T) {
			// given
			// ... a root that renders a random tree each step
			g := &treeGen{r: rand.New(rand.NewPCG(uint64(seed), 7))}
			root := &fuzzRoot{gen: func() Node { g.lists = 0; return Frag(g.nodes(0)...) }}
			h := newHarness(t, "/", func() Component { return root })

			for range 25 {
				// when
				// ... the root or a random live child re-renders
				h.do(func() {
					if len(g.children) > 0 && g.r.IntN(3) == 0 {
						ch := g.children[g.r.IntN(len(g.children))]
						ch.ticks++
						ch.StateHasChanged()
						return
					}
					root.StateHasChanged()
				})
				// then
				// ... harness.do has compared the client DOM to the server tree
			}
		})
	}
}

type counter struct {
	Base
	Start int
	n     int
}

func (c *counter) OnInit() { c.n = c.Start }
func (c *counter) Render() Node {
	return El("button", On("click", func() { c.n++ }), Text(fmt.Sprint("count ", c.n)))
}

type counterHost struct {
	Base
	order []int
}

func (h *counterHost) Render() Node {
	var out []Node
	for _, k := range h.order {
		out = append(out, Keyed(k, C(func(c *counter) { c.Start = k * 10 })))
	}
	return Frag(out...)
}

func TestChildStateSurvivesParentRenderAndKeyedReorder(t *testing.T) {
	// given
	// ... two keyed counters, the first clicked once
	host := &counterHost{order: []int{1, 2}}
	h := newHarness(t, "/", func() Component { return host })
	h.fire(h.find("button", "count 10"), "click", Event{})

	// when
	// ... the parent re-renders with the order swapped
	h.do(func() { host.order = []int{2, 1}; host.StateHasChanged() })

	// then
	// ... each counter kept its own count and they swapped places
	if got := textF(h.dom.root); got != "count 20count 11" {
		t.Fatalf("got %q", got)
	}
}

type boom struct{ Base }

func (b *boom) Render() Node {
	return El("button", On("click", func() { panic("kaboom") }), Text("explode"))
}

type boundaryHost struct{ Base }

func (b *boundaryHost) Render() Node {
	return Frag(
		El("p", Text("outside")),
		C(func(e *ErrorBoundary) {
			e.ChildContent = C(func(*boom) {})
			e.ErrorContent = func(err error) Node { return El("p", Text("caught: "+err.Error())) }
		}),
	)
}

func TestErrorBoundaryCatchesHandlerPanic(t *testing.T) {
	// given
	// ... a component that panics on click inside an ErrorBoundary
	h := newHarness(t, "/", func() Component { return &boundaryHost{} })

	// when
	// ... it is clicked
	h.fire(h.find("button", "explode"), "click", Event{})

	// then
	// ... the boundary shows the error and the rest of the page survives
	if got := textF(h.dom.root); got != "outsidecaught: kaboom" {
		t.Fatalf("got %q", got)
	}
}

type theme struct{ Color string }

type themed struct {
	Base
	T *theme `cascade:""`
}

func (t *themed) Render() Node { return Text("theme " + t.T.Color) }

type themeHost struct {
	Base
	t theme
}

func (h *themeHost) Render() Node {
	return C(func(cv *CascadingValue) {
		cv.Value = &h.t
		cv.ChildContent = El("div", C(func(*themed) {}))
	})
}

func TestCascadingValueReachesDescendants(t *testing.T) {
	// given
	// ... a CascadingValue holding a theme
	host := &themeHost{t: theme{"red"}}
	h := newHarness(t, "/", func() Component { return host })

	// when
	// ... the theme changes and the host re-renders
	h.do(func() { host.t.Color = "blue"; host.StateHasChanged() })

	// then
	// ... the nested consumer shows the new value
	if got := textF(h.dom.root); got != "theme blue" {
		t.Fatalf("got %q", got)
	}
}

type bindForm struct {
	Base
	Name  string
	Age   int
	Agree bool
}

func (f *bindForm) Render() Node {
	return Frag(
		El("input", Bind(&f.Name, BindEvent("input"))),
		El("input", Attr("type", "number"), Bind(&f.Age)),
		El("input", Attr("type", "checkbox"), Bind(&f.Agree)),
		El("p", Text(fmt.Sprintf("%s/%d/%v", f.Name, f.Age, f.Agree))),
	)
}

func TestBindWritesBackAndRerenders(t *testing.T) {
	// given
	// ... text, number and checkbox inputs bound to fields
	f := &bindForm{}
	h := newHarness(t, "/", func() Component { return f })
	inputs := h.dom.root.kids

	// when
	// ... the user edits each one
	h.fire(inputs[0], "input", Event{Value: "Ann"})
	h.fire(inputs[1], "change", Event{Value: "42"})
	h.fire(inputs[2], "change", Event{Checked: true})

	// then
	// ... the fields hold the values and the page shows them
	if got := textF(h.find("p", "")); got != "Ann/42/true" {
		t.Fatalf("got %q", got)
	}
	if inputs[0].props["value"] != "Ann" {
		t.Fatalf("value prop not updated: %v", inputs[0].props)
	}
}

type userPage struct {
	Base
	ID   int
	Tab  string `query:"tab"`
	life *[]string
}

func (u *userPage) OnInit()  { *u.life = append(*u.life, "init") }
func (u *userPage) Dispose() { *u.life = append(*u.life, "dispose") }
func (u *userPage) Render() Node {
	return El("h1", Text(fmt.Sprintf("user %d %s", u.ID, u.Tab)))
}

type shell struct{ LayoutBase }

func (s *shell) Render() Node { return El("main", s.Body) }

var lifecycle []string

func init() {
	RegisterPage("/users/{id:int}", func() Component { return &userPage{life: &lifecycle} }, func() Component { return &shell{} })
}

func TestRouterParamsLayoutAndNotFound(t *testing.T) {
	// given
	// ... the app opened on a user page
	lifecycle = nil
	h := newHarness(t, "/users/7?tab=posts", Factory[Router]())
	first := h.html()

	// when
	// ... the browser navigates to another user, then to an unknown /users/x
	h.do(func() { h.c.Nav.clientNavigated("/users/8") })
	second := h.html()
	h.do(func() { h.c.Nav.NavigateTo("/users/x") })

	// then
	// ... params bind, the page instance is reused, and leaving the routes disposes it
	if !strings.Contains(first, `<main><h1>"user 7 posts"`) || !strings.Contains(second, `"user 8 "`) {
		t.Fatalf("got %s then %s", first, second)
	}
	if strings.Join(lifecycle, ",") != "init,dispose" {
		t.Fatalf("lifecycle %v", lifecycle)
	}
}

type asyncLoader struct {
	Base
	data string
}

func (a *asyncLoader) OnInit() {
	go func() {
		a.InvokeAsync(func() { a.data = "loaded" })
	}()
}

func (a *asyncLoader) Render() Node {
	if a.data == "" {
		return Text("loading")
	}
	return Text(a.data)
}

func TestInvokeAsyncRerenders(t *testing.T) {
	// given
	// ... a component that loads data on another goroutine
	h := newHarness(t, "/", func() Component { return &asyncLoader{} })

	// when
	// ... the circuit drains the posted work
	for range 100 {
		if textF(h.dom.root) == "loaded" {
			break
		}
		h.do(func() {})
	}

	// then
	// ... the result is rendered
	if got := textF(h.dom.root); got != "loaded" {
		t.Fatalf("got %q", got)
	}
}

func TestBoundInputIsNotEchoedButBadInputIsReverted(t *testing.T) {
	// given
	// ... text and number inputs bound to fields
	f := &bindForm{Age: 42}
	h := newHarness(t, "/", func() Component { return f })
	inputs := h.dom.root.kids

	// when
	// ... the user types valid text, then non-numeric text into the number box
	h.fire(inputs[0], "input", Event{Value: "Ann"})
	echoed := strings.Contains(string(h.lastOps), `"value"`)
	h.fire(inputs[1], "change", Event{Value: "12x"})

	// then
	// ... the typed text is not sent back, and the bad number is reverted to 42
	if echoed {
		t.Fatalf("value echoed back: %s", h.lastOps)
	}
	if inputs[1].props["value"] != "42" || f.Age != 42 {
		t.Fatalf("not reverted: %v, Age=%d", inputs[1].props, f.Age)
	}
}

type seoPage struct {
	Base
	show bool
}

func (s *seoPage) Render() Node {
	if !s.show {
		return Text("plain")
	}
	return Frag(
		C(func(p *PageTitle) { p.ChildContent = Text("About") }),
		C(func(h *HeadContent) {
			h.ChildContent = El("meta", Attr("name", "description"), Attr("content", `Fish & "chips"`))
		}),
	)
}

func TestHeadContentIsPrerenderedAndClearedWhenItLeaves(t *testing.T) {
	// given
	// ... a page with a title and a meta description, served over HTTP
	app := New()
	app.Title = "Site"
	page := &seoPage{show: true}
	app.Root = func() Component { return page }
	srv := httptest.NewServer(app)
	defer srv.Close()
	res, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	head := string(body[:bytes.Index(body, []byte("</head>"))])

	// when
	// ... a live circuit stops rendering them
	h := newHarness(t, "/", func() Component { return &seoPage{show: true} })
	live := h.c.root.children[0].inst.c.(*seoPage)
	h.do(func() { live.show = false; live.StateHasChanged() })

	// then
	// ... crawlers get both in <head>, escaped, and the browser is told to drop them
	// ... (the title falls back to the test app's empty default)
	if !strings.Contains(head, "<title>About</title>") ||
		!strings.Contains(head, `<meta content="Fish &amp; &#34;chips&#34;" data-gz-head name="description">`) {
		t.Fatalf("head: %s", head)
	}
	if ops := string(h.lastOps); !strings.Contains(ops, `[9,""]`) || !strings.Contains(ops, `[10,""]`) {
		t.Fatalf("ops: %s", ops)
	}
}
