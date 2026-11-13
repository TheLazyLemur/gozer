package main

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// lspClient plays the editor against a proxy wired to a real gopls.
type lspClient struct {
	t     *testing.T
	in    *wire
	mu    sync.Mutex
	next  int
	resps map[string]chan *msg
	diags chan *msg
	init  json.RawMessage // initialize result
}

func startLSP(t *testing.T, root string) *lspClient {
	t.Helper()
	gopls, err := exec.LookPath("gopls")
	if err != nil {
		t.Skip("gopls not installed")
	}
	cmd := exec.Command(gopls)
	sin, _ := cmd.StdinPipe()
	sout, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sin.Close(); cmd.Process.Kill(); cmd.Wait() })
	editorR, proxyW := io.Pipe()
	proxyR, editorW := io.Pipe()
	p := newProxy(proxyW, sin, "github.com/TheLazyLemur/gozer")
	go p.serveServer(sout)
	go p.serveClient(proxyR)
	c := &lspClient{t: t, in: &wire{w: editorW}, resps: map[string]chan *msg{}, diags: make(chan *msg, 100)}
	go func() {
		br := bufio.NewReader(editorR)
		for {
			raw, err := readMsg(br)
			if err != nil {
				return
			}
			var m msg
			json.Unmarshal(raw, &m)
			switch {
			case m.Method == "textDocument/publishDiagnostics":
				c.diags <- &m
			case m.Method != "" && m.ID != nil: // gopls asking the editor something
				c.in.send(&msg{ID: m.ID, Result: json.RawMessage("null")})
			case m.Method == "":
				c.mu.Lock()
				ch := c.resps[string(m.ID)]
				c.mu.Unlock()
				if ch != nil {
					ch <- &m
				}
			}
		}
	}()
	c.init = c.call("initialize", map[string]any{
		"rootUri":               pathToURI(root),
		"initializationOptions": map[string]any{"semanticTokens": true},
		"capabilities": map[string]any{"textDocument": map[string]any{"semanticTokens": map[string]any{
			"requests":       map[string]any{"full": true},
			"tokenTypes":     strings.Fields("namespace type typeParameter parameter variable function method keyword comment string number operator"),
			"tokenModifiers": []string{},
			"formats":        []string{"relative"},
		}}},
	})
	c.notify("initialized", map[string]any{})
	return c
}

func (c *lspClient) notify(method string, params any) {
	c.in.send(&msg{Method: method, Params: mustJSON(params)})
}

func (c *lspClient) call(method string, params any) json.RawMessage {
	c.t.Helper()
	c.mu.Lock()
	c.next++
	id := itoa(c.next)
	ch := make(chan *msg, 1)
	c.resps[id] = ch
	c.mu.Unlock()
	c.in.send(&msg{ID: json.RawMessage(id), Method: method, Params: mustJSON(params)})
	select {
	case r := <-ch:
		if r.Error != nil {
			c.t.Fatalf("%s: %s", method, r.Error)
		}
		return r.Result
	case <-time.After(60 * time.Second):
		c.t.Fatalf("%s timed out", method)
	}
	return nil
}

// at is the LSP position of the first occurrence of marker in text, plus skip bytes.
func at(text, marker string, skip int) position {
	off := strings.Index(text, marker) + skip
	return newLineIndex(text).position(off, "utf-16")
}

func labels(raw json.RawMessage) string {
	v, _ := decode(raw)
	var out []string
	for _, it := range completionItems(v) {
		out = append(out, it.(map[string]any)["label"].(string))
	}
	return " " + strings.Join(out, " ") + " "
}

func TestLanguageServerBridgesTemplatesAndGo(t *testing.T) {
	root, _ := filepath.Abs("../..")
	demo := filepath.Join(root, "examples", "demo")
	c := startLSP(t, root)
	counterPath := filepath.Join(demo, "Counter.gozer")
	orig, err := os.ReadFile(counterPath)
	if err != nil {
		t.Fatal(err)
	}
	text := string(orig) + "<p>@c.</p>\n<Counter  />\n<p>@c.nope</p>\n"
	uri := pathToURI(counterPath)
	c.notify("textDocument/didOpen", map[string]any{"textDocument": map[string]any{
		"uri": uri, "languageId": "github.com/TheLazyLemur/gozer", "version": 1, "text": text}})

	t.Run("Go completion after a dot in markup", func(t *testing.T) {
		// given
		// ... the cursor right after "@c."
		pos := at(text, "<p>@c.</p>", len("<p>@c."))

		// when
		// ... completion is requested
		res := c.call("textDocument/completion", map[string]any{"textDocument": map[string]any{"uri": uri}, "position": pos})

		// then
		// ... the component's fields and methods are offered
		if got := labels(res); !strings.Contains(got, " count ") || !strings.Contains(got, " increment ") {
			t.Fatalf("got %s", got)
		}
	})

	t.Run("component parameters in a tag", func(t *testing.T) {
		// given
		// ... the cursor in the blank space of "<Counter  />"
		pos := at(text, "<Counter  />", len("<Counter "))

		// when
		// ... completion is requested
		res := c.call("textDocument/completion", map[string]any{"textDocument": map[string]any{"uri": uri}, "position": pos})

		// then
		// ... exported fields are offered as attributes, unexported state is not
		got := labels(res)
		if !strings.Contains(got, " Start ") || !strings.Contains(got, " IncrementAmount ") || strings.Contains(got, " count ") {
			t.Fatalf("got %s", got)
		}
	})

	t.Run("type errors land on the template line", func(t *testing.T) {
		// given
		// ... a template that uses c.nope
		want := at(text, "@c.nope", 0).Line

		// when
		// ... gopls reports diagnostics for the generated file
		var found string
		deadline := time.After(60 * time.Second)
		for found == "" {
			select {
			case m := <-c.diags:
				var params struct {
					URI         string
					Diagnostics []struct {
						Range   lspRange
						Message string
					}
				}
				json.Unmarshal(m.Params, &params)
				for _, d := range params.Diagnostics {
					if params.URI == uri && strings.Contains(d.Message, "nope") && d.Range.Start.Line == want {
						found = d.Message
					}
				}
			case <-deadline:
				t.Fatal("no diagnostic for c.nope on its template line")
			}
		}

		// then
		// ... the message is gopls's
		if !strings.Contains(found, "undefined") {
			t.Fatalf("got %q", found)
		}
	})

	t.Run("hover in an event handler", func(t *testing.T) {
		// given
		// ... the cursor on c.increment inside @onclick
		pos := at(text, `@onclick="c.increment"`, len(`@onclick="c.inc`))

		// when
		// ... hover is requested
		res := c.call("textDocument/hover", map[string]any{"textDocument": map[string]any{"uri": uri}, "position": pos})

		// then
		// ... gopls describes the method
		if !strings.Contains(string(res), "func (c *Counter) increment()") {
			t.Fatalf("got %s", res)
		}
	})

	t.Run("definition from Go lands in the template", func(t *testing.T) {
		// given
		// ... main.go's reference to the App component, declared only by App.gozer
		mainPath := filepath.Join(demo, "main.go")
		mainText, _ := os.ReadFile(mainPath)
		pos := at(string(mainText), "Factory[App]", len("Factory["))

		// when
		// ... go to definition is requested in the Go file
		res := c.call("textDocument/definition", map[string]any{"textDocument": map[string]any{"uri": pathToURI(mainPath)}, "position": pos})

		// then
		// ... the location is the .gozer file, not its generated Go
		if !strings.Contains(string(res), "App.gozer\"") {
			t.Fatalf("got %s", res)
		}
	})
}

func TestLanguageServerColoursTemplates(t *testing.T) {
	// given
	// ... an open template with an event handler and a block keyword
	root, _ := filepath.Abs("../..")
	c := startLSP(t, root)
	path := filepath.Join(root, "examples", "demo", "Counter.gozer")
	orig, _ := os.ReadFile(path)
	text := string(orig) + "@if c.count > 1 {\n<b>big</b>\n}\n"
	uri := pathToURI(path)
	c.notify("textDocument/didOpen", map[string]any{"textDocument": map[string]any{
		"uri": uri, "languageId": "github.com/TheLazyLemur/gozer", "version": 1, "text": text}})
	var init struct {
		Capabilities struct {
			SemanticTokensProvider struct {
				Legend struct{ TokenTypes []string }
			}
		}
	}
	json.Unmarshal(c.init, &init)
	legend := init.Capabilities.SemanticTokensProvider.Legend.TokenTypes

	// when
	// ... semantic tokens are requested
	raw := c.call("textDocument/semanticTokens/full", map[string]any{"textDocument": map[string]any{"uri": uri}})

	// then
	// ... Go code gets gopls's token types and template syntax is a keyword
	var res struct{ Data []int }
	json.Unmarshal(raw, &res)
	got := map[string]string{}
	lines := strings.Split(text, "\n")
	line, char := 0, 0
	for i := 0; i+4 < len(res.Data); i += 5 {
		if res.Data[i] > 0 {
			line, char = line+res.Data[i], res.Data[i+1]
		} else {
			char += res.Data[i+1]
		}
		got[lines[line][char:char+res.Data[i+2]]] = legend[res.Data[i+3]]
	}
	for word, typ := range map[string]string{"@page": "keyword", "increment": "method", "@if": "keyword", "count": "property"} {
		if got[word] != typ {
			t.Errorf("%s: got %q, want %q (all: %v)", word, got[word], typ, got)
		}
	}
}
