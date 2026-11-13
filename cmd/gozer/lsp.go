package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// runLSP serves LSP on stdio. Requests about .gozer files are answered here,
// mapping into generated Go where needed; everything else goes to a gopls child
// that sees an in-memory generated file for every .gozer file in the workspace.
func runLSP(args []string) error {
	fl := flag.NewFlagSet("gozer lsp", flag.ExitOnError)
	goplsPath := fl.String("gopls", "gopls", "gopls binary")
	runtime := fl.String("runtime", "github.com/TheLazyLemur/gozer", "import path of the gozer runtime package")
	fl.Parse(args)
	log.SetPrefix("gozer lsp: ")
	cmd := exec.Command(*goplsPath)
	cmd.Stderr = os.Stderr
	in, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting gopls: %w", err)
	}
	p := newProxy(os.Stdout, in, *runtime)
	go func() {
		p.serveServer(out)
		os.Exit(0) // gopls is gone; the editor will see the server exit
	}()
	err = p.serveClient(os.Stdin)
	in.Close()
	done := make(chan struct{})
	go func() { cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		cmd.Process.Kill()
	}
	return err
}

type msg struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   json.RawMessage `json:"error,omitempty"`
}

type wire struct {
	mu sync.Mutex
	w  io.Writer
}

func (w *wire) write(data []byte) {
	w.mu.Lock()
	defer w.mu.Unlock()
	fmt.Fprintf(w.w, "Content-Length: %d\r\n\r\n", len(data))
	w.w.Write(data)
}

func (w *wire) send(m *msg) {
	m.JSONRPC = "2.0"
	if m.ID != nil && m.Method == "" && m.Result == nil && m.Error == nil {
		m.Result = json.RawMessage("null")
	}
	data, err := json.Marshal(m)
	if err != nil {
		panic(fmt.Sprintf("encoding %s: %v", m.Method, err))
	}
	w.write(data)
}

func readMsg(r *bufio.Reader) ([]byte, error) {
	n := -1
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimSpace(line)
		if line == "" {
			break
		}
		if k, v, ok := strings.Cut(line, ":"); ok && strings.EqualFold(k, "Content-Length") {
			n, _ = strconv.Atoi(strings.TrimSpace(v))
		}
	}
	if n < 0 {
		return nil, errors.New("message without Content-Length")
	}
	buf := make([]byte, n)
	_, err := io.ReadFull(r, buf)
	return buf, err
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

type proxy struct {
	client, server *wire
	runtime        string

	mu      sync.Mutex
	enc     string   // negotiated position encoding
	legend  []string // gopls's semantic token types
	roots   []string
	docs    map[string]*doc       // by .gozer path
	pending map[string]func(*msg) // forwarded client requests awaiting gopls; nil means the generic rewrite
	own     map[string]chan *msg  // the proxy's own requests to gopls
	seq     int
}

func newProxy(client, server io.Writer, runtime string) *proxy {
	return &proxy{
		client:  &wire{w: client},
		server:  &wire{w: server},
		runtime: runtime,
		enc:     "utf-16",
		docs:    map[string]*doc{},
		pending: map[string]func(*msg){},
		own:     map[string]chan *msg{},
	}
}

type clientMsg struct {
	m   *msg
	raw []byte
}

// serveClient reads the editor. Answers to gopls go straight through; everything
// else runs in order on one worker, which may wait on gopls without starving
// those answers (gopls asks the editor for settings while loading).
func (p *proxy) serveClient(r io.Reader) error {
	work := make(chan clientMsg, 1024)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for c := range work {
			p.fromClient(c.m, c.raw)
		}
	}()
	defer func() { close(work); <-done }()
	br := bufio.NewReader(r)
	for {
		raw, err := readMsg(br)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		var m msg
		if err := json.Unmarshal(raw, &m); err != nil {
			log.Printf("bad message from editor: %v", err)
			continue
		}
		switch m.Method {
		case "exit":
			p.server.write(raw)
			return nil
		case "":
			p.server.write(raw)
		default:
			work <- clientMsg{&m, raw}
		}
	}
}

func paramURI(params json.RawMessage) string {
	var v struct {
		TextDocument struct {
			URI string `json:"uri"`
		} `json:"textDocument"`
	}
	json.Unmarshal(params, &v)
	return v.TextDocument.URI
}

func (p *proxy) fromClient(m *msg, raw []byte) {
	uri := paramURI(m.Params)
	isGozer := strings.HasSuffix(uri, ".gozer")
	switch {
	case m.Method == "initialize":
		p.setRoots(m.Params)
		p.forward(m, raw, p.initResult)
	case m.Method == "initialized":
		p.server.write(raw)
		p.openAll()
	case strings.HasPrefix(m.Method, "textDocument/did") && isGozer:
		p.syncDoc(m)
	case strings.HasPrefix(m.Method, "textDocument/did") && strings.HasSuffix(uri, ".gozer.go") && p.mirrored(uri):
		// gopls holds our generated version of this file; the editor's copy would clash
	case m.Method == "workspace/didChangeWatchedFiles":
		p.server.write(raw)
		p.refreshClosed()
	case isGozer && m.ID != nil:
		p.gozerRequest(m)
	case isGozer:
	default:
		p.forward(m, raw, nil)
	}
}

func (p *proxy) forward(m *msg, raw []byte, rewrite func(*msg)) {
	if m.ID != nil {
		p.mu.Lock()
		p.pending[string(m.ID)] = rewrite
		p.mu.Unlock()
	}
	p.server.write(raw)
}

func (p *proxy) serveServer(r io.Reader) {
	br := bufio.NewReader(r)
	for {
		raw, err := readMsg(br)
		if err != nil {
			if !errors.Is(err, io.EOF) {
				log.Printf("reading gopls: %v", err)
			}
			return
		}
		var m msg
		if err := json.Unmarshal(raw, &m); err != nil {
			log.Printf("bad message from gopls: %v", err)
			continue
		}
		switch {
		case m.Method == "":
			id := string(m.ID)
			p.mu.Lock()
			ch, own := p.own[id]
			rewrite, fwd := p.pending[id]
			delete(p.own, id)
			delete(p.pending, id)
			p.mu.Unlock()
			switch {
			case own:
				ch <- &m
			case fwd && rewrite != nil:
				rewrite(&m)
				p.client.send(&m)
			case fwd:
				p.fixResult(&m)
				p.client.send(&m)
			default:
				// the proxy gave up waiting on its own request; the editor never asked for this
				log.Printf("dropping late gopls response %s", id)
			}
		case m.Method == "textDocument/publishDiagnostics":
			p.goDiagnostics(&m)
		case m.Method == "workspace/applyEdit":
			v, err := decode(m.Params)
			if err == nil && p.fixValue(v, map[string]*mapper{}) == nil {
				m.Params = mustJSON(v)
			}
			p.client.send(&m)
		default:
			p.client.write(raw)
		}
	}
}

// call sends the proxy's own request to gopls and waits for the result.
func (p *proxy) call(method string, params any) (json.RawMessage, error) {
	p.mu.Lock()
	p.seq++
	id := fmt.Sprintf(`"gozer-%d"`, p.seq)
	ch := make(chan *msg, 1)
	p.own[id] = ch
	p.mu.Unlock()
	p.server.send(&msg{ID: json.RawMessage(id), Method: method, Params: mustJSON(params)})
	select {
	case r := <-ch:
		if r.Error != nil {
			return nil, fmt.Errorf("gopls: %s", r.Error)
		}
		return r.Result, nil
	case <-time.After(10 * time.Second):
		p.mu.Lock()
		delete(p.own, id)
		p.mu.Unlock()
		return nil, fmt.Errorf("gopls: %s timed out", method)
	}
}

func (p *proxy) reply(id json.RawMessage, v any) {
	p.client.send(&msg{ID: id, Result: mustJSON(v)})
}

func (p *proxy) replyErr(id json.RawMessage, err error) {
	p.client.send(&msg{ID: id, Error: mustJSON(map[string]any{"code": -32603, "message": err.Error()})})
}

func (p *proxy) setRoots(params json.RawMessage) {
	var v struct {
		RootURI          string `json:"rootUri"`
		WorkspaceFolders []struct {
			URI string `json:"uri"`
		} `json:"workspaceFolders"`
	}
	json.Unmarshal(params, &v)
	for _, f := range v.WorkspaceFolders {
		p.roots = append(p.roots, uriToPath(f.URI))
	}
	if len(p.roots) == 0 && v.RootURI != "" {
		p.roots = append(p.roots, uriToPath(v.RootURI))
	}
}

// initResult records the position encoding and adjusts what the editor is told we support.
func (p *proxy) initResult(m *msg) {
	v, err := decode(m.Result)
	if err != nil || m.Error != nil {
		return
	}
	caps, _ := v.(map[string]any)["capabilities"].(map[string]any)
	if caps == nil {
		return
	}
	p.mu.Lock()
	if enc, ok := caps["positionEncoding"].(string); ok {
		p.enc = enc
	}
	if st, ok := caps["semanticTokensProvider"].(map[string]any); ok {
		legend, _ := st["legend"].(map[string]any)
		types, _ := legend["tokenTypes"].([]any)
		for _, t := range types {
			p.legend = append(p.legend, t.(string))
		}
	}
	p.mu.Unlock()
	delete(caps, "diagnosticProvider") // .gozer diagnostics are pushed; keep one model for both
	if cp, ok := caps["completionProvider"].(map[string]any); ok {
		tc, _ := cp["triggerCharacters"].([]any)
		cp["triggerCharacters"] = append(tc, "<", "@")
	}
	m.Result = mustJSON(v)
}

func (p *proxy) mirrored(virtualURI string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.docs[uriToPath(strings.TrimSuffix(virtualURI, ".go"))] != nil
}

// openAll mirrors every .gozer file under the workspace roots, so gopls
// type-checks packages with their current generated code.
func (p *proxy) openAll() {
	for _, root := range p.roots {
		filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			name := d.Name()
			if d.IsDir() && path != root && (strings.HasPrefix(name, ".") || name == "vendor" || name == "node_modules" || name == "testdata") {
				return filepath.SkipDir
			}
			if d.IsDir() || !strings.HasSuffix(name, ".gozer") {
				return nil
			}
			p.mu.Lock()
			_, seen := p.docs[path]
			p.mu.Unlock()
			if text, err := readFile(path); err == nil && !seen {
				d := &doc{path: path, text: text}
				p.mu.Lock()
				p.docs[path] = d
				p.mu.Unlock()
				p.update(d)
			}
			return nil
		})
	}
}

// refreshClosed re-reads .gozer files the editor does not have open (git checkout, etc.).
func (p *proxy) refreshClosed() {
	p.mu.Lock()
	var closed []*doc
	for _, d := range p.docs {
		if !d.open {
			closed = append(closed, d)
		}
	}
	p.mu.Unlock()
	for _, d := range closed {
		if text, err := readFile(d.path); err == nil && text != d.text {
			p.mu.Lock()
			d.text = text
			p.mu.Unlock()
			p.update(d)
		}
	}
}

func (p *proxy) syncDoc(m *msg) {
	var v struct {
		TextDocument struct {
			URI  string `json:"uri"`
			Text string `json:"text"`
		} `json:"textDocument"`
		ContentChanges []struct {
			Range *lspRange `json:"range"`
			Text  string    `json:"text"`
		} `json:"contentChanges"`
	}
	if err := json.Unmarshal(m.Params, &v); err != nil {
		log.Printf("%s: %v", m.Method, err)
		return
	}
	path := uriToPath(v.TextDocument.URI)
	p.mu.Lock()
	d := p.docs[path]
	if d == nil {
		d = &doc{path: path}
		p.docs[path] = d
	}
	enc := p.enc
	p.mu.Unlock()
	text := d.text
	switch m.Method {
	case "textDocument/didOpen":
		text = v.TextDocument.Text
		d.open = true
	case "textDocument/didChange":
		for _, c := range v.ContentChanges {
			if c.Range == nil {
				text = c.Text
				continue
			}
			x := newLineIndex(text)
			text = text[:x.offset(c.Range.Start, enc)] + c.Text + text[x.offset(c.Range.End, enc):]
		}
	case "textDocument/didClose":
		d.open = false
		if disk, err := readFile(path); err == nil {
			text = disk
		}
	case "textDocument/didSave":
		// keep the generated file on disk in step, so go build matches the editor
		if err := compileFile(path, p.runtime); err != nil {
			log.Printf("%v", err)
		}
		return
	default:
		return
	}
	p.mu.Lock()
	d.text = text
	p.mu.Unlock()
	p.update(d)
}

// regen parses and generates text for the editor; gs is nil when it cannot.
func (p *proxy) regen(path, text string, dotAt int) (*genState, []any) {
	name := strings.TrimSuffix(filepath.Base(path), ".gozer")
	idx := newLineIndex(text)
	p.mu.Lock()
	enc := p.enc
	p.mu.Unlock()
	var diags []any
	diag := func(start, end int, message string) {
		diags = append(diags, map[string]any{
			"range":    lspRange{idx.position(start, enc), idx.position(end, enc)},
			"severity": 1, "source": "github.com/TheLazyLemur/gozer", "message": message,
		})
	}
	f, errs := parseTolerant(path, name, text, dotAt)
	for _, e := range errs {
		diag(e.pos, min(e.pos+1, len(text)), e.msg)
	}
	if f == nil {
		return nil, diags
	}
	dir := filepath.Dir(path)
	out, err := generate(path, packageName(dir), p.runtime, localComponents(dir), f, true, dotAt)
	if err != nil {
		message, line := strings.TrimPrefix(err.Error(), path+":"), 1
		var ge *genError
		if errors.As(err, &ge) {
			line = ge.line
			message = strings.TrimPrefix(message, strconv.Itoa(ge.line)+":")
		}
		start := idx.starts[min(line-1, len(idx.starts)-1)]
		diag(start, start, strings.TrimSpace(message))
		return nil, diags
	}
	return &genState{src: text, out: out, gen: newLineIndex(out.gen)}, diags
}

func (p *proxy) update(d *doc) {
	gs, diags := p.regen(d.path, d.text, -1)
	p.mu.Lock()
	d.parseDiags = diags
	if gs != nil {
		d.gs = gs
	}
	p.mu.Unlock()
	if gs != nil {
		p.sendVirtual(d, gs.out.gen)
	}
	p.publish(d)
}

// sendVirtual gives gopls new contents for d's generated file.
func (p *proxy) sendVirtual(d *doc, text string) {
	p.mu.Lock()
	d.version++
	v := d.version
	p.mu.Unlock()
	uri := pathToURI(d.path + ".go")
	if v == 1 {
		p.server.send(&msg{Method: "textDocument/didOpen", Params: mustJSON(map[string]any{
			"textDocument": map[string]any{"uri": uri, "languageId": "go", "version": v, "text": text}})})
		return
	}
	p.server.send(&msg{Method: "textDocument/didChange", Params: mustJSON(map[string]any{
		"textDocument":   map[string]any{"uri": uri, "version": v},
		"contentChanges": []any{map[string]any{"text": text}}})})
}

func (p *proxy) publish(d *doc) {
	p.mu.Lock()
	all := append(append([]any{}, d.parseDiags...), d.goDiags...)
	p.mu.Unlock()
	p.client.send(&msg{Method: "textDocument/publishDiagnostics", Params: mustJSON(map[string]any{
		"uri": pathToURI(d.path), "diagnostics": all})})
}

// goDiagnostics moves gopls diagnostics for generated files onto their templates.
func (p *proxy) goDiagnostics(m *msg) {
	v, err := decode(m.Params)
	if err != nil {
		return
	}
	params := v.(map[string]any)
	uri, _ := params["uri"].(string)
	cache := map[string]*mapper{}
	mp := p.mapperFor(uri, cache)
	diags, _ := params["diagnostics"].([]any)
	p.mu.Lock()
	d := p.docs[uriToPath(strings.TrimSuffix(uri, ".go"))]
	stale := d != nil && params["version"] != nil && asInt(params["version"]) != d.version
	p.mu.Unlock()
	if stale {
		return // e.g. the temporary copy used to complete "@c."
	}
	if mp == nil {
		p.fixValue(v, cache) // related information may point into templates
		m.Params = mustJSON(v)
		p.client.send(m)
		return
	}
	for _, dg := range diags {
		mp.rangeInPlace(dg.(map[string]any)["range"], true)
		p.fixValue(dg, cache)
	}
	p.mu.Lock()
	d.goDiags = diags
	p.mu.Unlock()
	p.publish(d)
}

// mapperFor returns a mapper when uri is a generated file the proxy mirrors.
func (p *proxy) mapperFor(uri string, cache map[string]*mapper) *mapper {
	if !strings.HasSuffix(uri, ".gozer.go") {
		return nil
	}
	if m, ok := cache[uri]; ok {
		return m
	}
	p.mu.Lock()
	d := p.docs[uriToPath(strings.TrimSuffix(uri, ".go"))]
	var m *mapper
	if d != nil && d.gs != nil {
		m = &mapper{cur: d.text, gs: d.gs, src: newLineIndex(d.text), enc: p.enc}
	}
	p.mu.Unlock()
	cache[uri] = m
	return m
}

// fixResult rewrites locations and edits in a forwarded response.
func (p *proxy) fixResult(m *msg) {
	if m.Error != nil || !strings.Contains(string(m.Result), ".gozer.go") {
		return
	}
	v, err := decode(m.Result)
	if err != nil {
		return
	}
	if err := p.fixValue(v, map[string]*mapper{}); err != nil {
		m.Result = nil
		m.Error = mustJSON(map[string]any{"code": -32603, "message": err.Error()})
		return
	}
	m.Result = mustJSON(v)
}

// fixValue walks any LSP value and moves locations and edits that point into
// generated files onto their templates. Edits that land in generated-only code fail.
func (p *proxy) fixValue(v any, cache map[string]*mapper) error {
	switch t := v.(type) {
	case []any:
		for _, x := range t {
			if err := p.fixValue(x, cache); err != nil {
				return err
			}
		}
	case map[string]any:
		if changes, ok := t["changes"].(map[string]any); ok {
			for uri, edits := range changes {
				mp := p.mapperFor(uri, cache)
				if mp == nil {
					continue
				}
				for _, e := range edits.([]any) {
					if !mp.rangeInPlace(e.(map[string]any)["range"], false) {
						return errUnmappable
					}
				}
				target := strings.TrimSuffix(uri, ".go")
				prev, _ := changes[target].([]any)
				changes[target] = append(prev, edits.([]any)...)
				delete(changes, uri)
			}
		}
		if td, ok := t["textDocument"].(map[string]any); ok && t["edits"] != nil {
			uri, _ := td["uri"].(string)
			if mp := p.mapperFor(uri, cache); mp != nil {
				for _, e := range t["edits"].([]any) {
					if !mp.rangeInPlace(e.(map[string]any)["range"], false) {
						return errUnmappable
					}
				}
				td["uri"], td["version"] = strings.TrimSuffix(uri, ".go"), nil
			}
		}
		for _, k := range []string{"uri", "targetUri"} {
			uri, _ := t[k].(string)
			if mp := p.mapperFor(uri, cache); mp != nil {
				for _, rk := range []string{"range", "targetRange", "targetSelectionRange"} {
					if t[rk] != nil {
						mp.rangeInPlace(t[rk], true)
					}
				}
				t[k] = strings.TrimSuffix(uri, ".go")
			}
		}
		for _, x := range t {
			if err := p.fixValue(x, cache); err != nil {
				return err
			}
		}
	}
	return nil
}

// gozerRequest answers a position request in a template, via gopls when the
// position is in Go code.
func (p *proxy) gozerRequest(m *msg) {
	v, err := decode(m.Params)
	params, _ := v.(map[string]any)
	if err != nil || params == nil {
		p.reply(m.ID, nil)
		return
	}
	path := uriToPath(paramURI(m.Params))
	p.mu.Lock()
	d := p.docs[path]
	var text string
	var gs *genState
	if d != nil {
		text, gs = d.text, d.gs
	}
	enc := p.enc
	p.mu.Unlock()
	if strings.HasPrefix(m.Method, "textDocument/semanticTokens/") && gs != nil {
		// full, full/delta and range all get the whole file: a valid answer to each
		p.reply(m.ID, p.semanticTokens(path, text, gs, newLineIndex(text)))
		return
	}
	pos, hasPos := posOf(params["position"])
	if gs == nil || !hasPos {
		p.reply(m.ID, nil)
		return
	}
	src := newLineIndex(text)
	off := src.offset(pos, enc)
	if m.Method == "textDocument/completion" {
		p.reply(m.ID, p.complete(d, text, gs, src, off, params))
		return
	}
	switch m.Method {
	case "textDocument/hover", "textDocument/definition", "textDocument/typeDefinition",
		"textDocument/implementation", "textDocument/references", "textDocument/signatureHelp",
		"textDocument/documentHighlight", "textDocument/prepareRename", "textDocument/rename":
	default:
		p.reply(m.ID, nil) // formatting, semantic tokens, symbols, ... are not offered for templates
		return
	}
	g, ok := gs.toGen(text, off)
	if !ok {
		p.reply(m.ID, nil)
		return
	}
	params["textDocument"] = map[string]any{"uri": pathToURI(path + ".go")}
	setPos(params["position"], gs.gen.position(g, enc))
	mp := &mapper{cur: text, gs: gs, src: src, enc: enc}
	method := m.Method
	p.mu.Lock()
	p.pending[string(m.ID)] = func(r *msg) {
		if r.Error != nil {
			return
		}
		res, err := decode(r.Result)
		if err != nil {
			return
		}
		switch method {
		case "textDocument/hover":
			if h, ok := res.(map[string]any); ok && h["range"] != nil && !mp.rangeInPlace(h["range"], false) {
				delete(h, "range")
			}
		case "textDocument/documentHighlight":
			list, _ := res.([]any)
			kept := []any{}
			for _, x := range list {
				if mp.rangeInPlace(x.(map[string]any)["range"], false) {
					kept = append(kept, x)
				}
			}
			res = kept
		case "textDocument/prepareRename":
			if r, ok := res.(map[string]any); ok {
				target := any(r)
				if r["range"] != nil {
					target = r["range"]
				}
				if r["start"] != nil || r["range"] != nil {
					if !mp.rangeInPlace(target, false) {
						res = nil
					}
				}
			}
		}
		if err := p.fixValue(res, map[string]*mapper{}); err != nil {
			r.Result, r.Error = nil, mustJSON(map[string]any{"code": -32603, "message": err.Error()})
			return
		}
		r.Result = mustJSON(res)
	}
	p.mu.Unlock()
	p.server.send(&msg{ID: m.ID, Method: m.Method, Params: mustJSON(params)})
}
