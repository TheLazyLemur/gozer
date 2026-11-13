package main

import (
	"go/format"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func compile(t *testing.T, src string) string {
	t.Helper()
	f, err := parse("T.gozer", "T", src)
	if err != nil {
		t.Fatal(err)
	}
	out, err := generate("T.gozer", "pages", "github.com/TheLazyLemur/gozer", map[string]bool{}, f, false, -1)
	if err != nil {
		t.Fatal(err)
	}
	formatted, err := format.Source([]byte(out.gen))
	if err != nil {
		t.Fatalf("%v\n%s", err, out.gen)
	}
	return string(formatted)
}

func TestTemplateCompilesToExpectedGo(t *testing.T) {
	cases := []struct {
		name, src string
		want      []string
	}{
		{"text, literals and implicit expressions",
			`<p>mail a@b.com, @@handle, @c.Name.Upper()[0]!</p>`,
			[]string{`gozer.Text("mail a@b.com, ")`, `gozer.Text("@")`, `gozer.X(c.Name.Upper()[0])`, `gozer.Text("!")`}},
		{"control flow",
			"@if c.N > 1 {\n<b>many</b>\n} else if c.N == 1 {\n<b>one</b>\n} else {\nnone\n}\n@for _, x := range c.Xs {\n<i @key=\"x\">@x</i>\n}",
			[]string{"if c.N > 1 {", "} else if c.N == 1 {", "} else {", "for _, x := range c.Xs {", "gozer.Key(x)"}},
		{"element directives",
			`<input class="a @c.Cls" disabled @bind="c.V" @bind:event="oninput" @onkeydown="c.Key" @onkeydown:preventDefault @ref="c.in" />`,
			[]string{`gozer.Attr("class", gozer.S("a ", c.Cls))`, `gozer.Attr("disabled", true)`,
				`gozer.Bind(&(c.V), gozer.OwnedBy(c), gozer.BindEvent("oninput"))`,
				`gozer.On("keydown", c.Key, gozer.OwnedBy(c), gozer.Prevent)`, `gozer.Ref(&(c.in))`}},
		{"components, callbacks, binding and fragments",
			"<List[Item] Title=\"Hi @c.Who\" Count=@c.N OnPick=@c.Pick @bind-Sel=\"c.Sel\" @key=\"c.ID\" @ref=\"c.list\">\n<:Row args=\"it Item\"><td>@it.Name</td></:Row>\n<p>body</p>\n</List[Item]><PageTitle>x</PageTitle>",
			[]string{`gozer.Keyed(c.ID, gozer.C(func(_p1 *List[Item]) {`, `_p1.Title = gozer.S("Hi ", c.Who)`,
				`_p1.Count = c.N`, `_p1.OnPick = gozer.Callback(c, c.Pick)`, `_p1.Sel = gozer.BindTo(&(c.Sel), c)`,
				`c.list = _p1`, `_p1.Row = func(it Item) gozer.Node {`, `_p1.ChildContent = gozer.Frag(`, `*gozer.PageTitle`}},
		{"page, layout, params and generated struct",
			"@page \"/x/{id:int}\"\n@layout Main\n@param ID int\n@inject Svc *Service\n<style>@media x { a{} }</style>",
			[]string{"type T struct {", "gozer.Base", "ID  int", "Svc *Service `inject:\"\"`",
				`gozer.RegisterPage("/x/{id:int}", func() gozer.Component { return &T{} }, func() gozer.Component { return &Main{} })`,
				`gozer.Raw("@media x { a{} }")`}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// given
			// ... a template
			src := tc.src

			// when
			// ... it is compiled
			out := compile(t, src)

			// then
			// ... the Go output contains the expected constructs
			for _, w := range tc.want {
				if !strings.Contains(out, w) {
					t.Errorf("missing %s in:\n%s", w, out)
				}
			}
		})
	}
}

func TestTemplateErrorsPointAtTheSource(t *testing.T) {
	// given
	// ... a template with a mismatched closing tag on line 3
	src := "<div>\n  <p>hi\n</div>"

	// when
	// ... it is parsed
	_, err := parse("T.gozer", "T", src)

	// then
	// ... the error names the file, line and both tags
	if err == nil || !strings.Contains(err.Error(), "T.gozer:3:1: expected </p> (opened on line 2), found </div>") {
		t.Fatalf("got %v", err)
	}
}

func editorOutput(t *testing.T, path, src string, dotAt int) *output {
	t.Helper()
	name := strings.TrimSuffix(filepath.Base(path), ".gozer")
	f, errs := parseTolerant(path, name, src, dotAt)
	if f == nil {
		t.Fatalf("%s: %v", path, errs)
	}
	out, err := generate(path, "demo", "github.com/TheLazyLemur/gozer", map[string]bool{}, f, true, dotAt)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestEditorSpansCopyTemplateCodeVerbatim(t *testing.T) {
	files, _ := filepath.Glob("../../examples/demo/*.gozer")
	if len(files) == 0 {
		t.Fatal("no demo templates found")
	}
	for _, path := range files {
		t.Run(filepath.Base(path), func(t *testing.T) {
			// given
			// ... a valid demo template
			src, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}

			// when
			// ... it is generated in editor mode
			out := editorOutput(t, path, string(src), -1)

			// then
			// ... every span is a verbatim copy and the Go parses
			for _, s := range out.spans {
				if g, w := out.gen[s.gen:s.gen+s.n], string(src[s.src:s.src+s.n]); g != w {
					t.Errorf("span %+v: gen %q, template %q", s, g, w)
				}
			}
			if _, err := parser.ParseFile(token.NewFileSet(), "x.go", out.gen, 0); err != nil {
				t.Fatalf("%v\n%s", err, out.gen)
			}
		})
	}
}

func TestEditorModeRecoversFromHalfTypedMarkup(t *testing.T) {
	// given
	// ... a component tag still being typed, an unclosed div and a trailing "@c."
	src := "<div>\n<Counter Inc\n<p>@c.</p>"

	// when
	// ... it is parsed for the editor with the dot marked as a completion point
	out := editorOutput(t, "T.gozer", src, strings.Index(src, "."))

	// then
	// ... the attribute and the dotted expression are both mapped
	var mapped []string
	for _, s := range out.spans {
		mapped = append(mapped, src[s.src:s.src+s.n])
	}
	if got := strings.Join(mapped, ","); got != "Counter,Inc,c." {
		t.Fatalf("mapped %q\n%s", got, out.gen)
	}
}
