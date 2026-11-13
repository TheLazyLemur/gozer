package gozer

import (
	"embed"
	"html"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

//go:embed client/gozer.js
var clientFS embed.FS

// App is the HTTP server for a gozer application.
type App struct {
	// Root builds the root component. Default: a Router with no layout.
	Root func() Component
	// Title is the default document title.
	Title string
	// Head is extra trusted HTML for <head> (stylesheets, meta tags, scripts).
	Head string
	// Lang is the <html lang> attribute.
	Lang string
	// Prerender renders the first page as HTML before the WebSocket connects.
	Prerender bool
	// Retention is how long a disconnected circuit waits for a reconnect.
	Retention time.Duration
	// Authenticate resolves the user from the request that opens a circuit.
	Authenticate func(*http.Request) *User
	// WebSocketOptions configures the WebSocket upgrade (e.g. OriginPatterns).
	WebSocketOptions *websocket.AcceptOptions

	mux        *http.ServeMux
	rootType   reflect.Type
	singletons map[reflect.Type]reflect.Value
	scoped     map[reflect.Type]reflect.Value
	circuits   sync.Map
	initOnce   sync.Once
}

// New creates an App with prerendering on and a 3 minute reconnect window.
func New() *App {
	a := &App{
		Prerender:  true,
		Retention:  3 * time.Minute,
		Lang:       "en",
		mux:        http.NewServeMux(),
		singletons: map[reflect.Type]reflect.Value{},
		scoped:     map[reflect.Type]reflect.Value{},
	}
	a.mux.Handle("GET /_gozer/gozer.js", http.StripPrefix("/_gozer/", http.FileServerFS(must(fs.Sub(clientFS, "client")))))
	a.mux.HandleFunc("GET /_gozer/ws", a.serveWS)
	a.mux.HandleFunc("GET /", a.servePage)
	return a
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

// Provide registers a singleton service for fields tagged `inject:""` of type T.
func Provide[T any](a *App, v T) { a.singletons[reflect.TypeFor[T]()] = reflect.ValueOf(&v).Elem() }

// ProvideScoped registers a per-circuit service built on first use.
func ProvideScoped[T any](a *App, f func(*Circuit) T) {
	a.scoped[reflect.TypeFor[T]()] = reflect.ValueOf(f)
}

// Handle registers an extra HTTP handler (APIs, downloads). More specific
// patterns win over the catch-all page handler.
func (a *App) Handle(pattern string, h http.Handler) { a.mux.Handle(pattern, h) }

// HandleFunc is Handle for functions.
func (a *App) HandleFunc(pattern string, h http.HandlerFunc) { a.mux.HandleFunc(pattern, h) }

// Static serves files from fsys under prefix, e.g. Static("/static/", assets).
func (a *App) Static(prefix string, fsys fs.FS) {
	prefix = "/" + strings.Trim(prefix, "/") + "/"
	a.mux.Handle("GET "+prefix, http.StripPrefix(prefix, http.FileServerFS(fsys)))
}

func (a *App) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	a.initOnce.Do(func() {
		if a.Root == nil {
			a.Root = Factory[Router]()
		}
		a.rootType = reflect.TypeOf(a.Root()).Elem()
	})
	a.mux.ServeHTTP(w, r)
}

// ListenAndServe serves the app on addr.
func (a *App) ListenAndServe(addr string) error {
	log.Printf("gozer: listening on %s", addr)
	return http.ListenAndServe(addr, a)
}

func requestURL(r *http.Request, raw string) *url.URL {
	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	u := &url.URL{Scheme: scheme, Host: r.Host, Path: "/"}
	if ref, err := url.Parse(raw); err == nil {
		u.Path, u.RawQuery = ref.Path, ref.RawQuery
		if u.Path == "" {
			u.Path = "/"
		}
	}
	return u
}

func (a *App) servePage(w http.ResponseWriter, r *http.Request) {
	id, body, title, head, status, failed := "", "", a.Title, "", http.StatusOK, false
	if a.Prerender {
		c := newCircuit(a, r)
		a.circuits.Store(c.ID, c)
		ok := c.sync(func() {
			c.start(requestURL(r, r.URL.RequestURI()))
			c.flush()
			if failed = c.fatal != nil; failed {
				return
			}
			var b strings.Builder
			writeHTML(&b, c.root, true)
			body = b.String()
			if c.title != "" {
				title = c.title
			}
			head = c.head
			c.mu.Lock()
			if c.notFnd {
				status = http.StatusNotFound
			}
			c.scheduleExpiry()
			c.mu.Unlock()
		})
		if !ok || failed {
			http.Error(w, "render failed", http.StatusInternalServerError)
			return
		}
		id = c.ID
	} else if rt, _ := matchRoute(r.URL.Path); rt == nil && len(routes) > 0 {
		status = http.StatusNotFound
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	var b strings.Builder
	b.WriteString(`<!DOCTYPE html><html lang="` + html.EscapeString(a.Lang) + `"><head><meta charset="utf-8">`)
	b.WriteString(`<meta name="viewport" content="width=device-width, initial-scale=1"><title>` + html.EscapeString(title) + `</title>`)
	b.WriteString(defaultStyle + a.Head + head + `</head><body><div id="gozer-app">` + body + `</div>`)
	b.WriteString(`<div id="gozer-reconnect" hidden><div>Reconnecting to the server…</div></div>`)
	b.WriteString(`<div id="gozer-error" hidden>An unhandled error has occurred. <a href="">Reload</a></div>`)
	b.WriteString(`<script src="/_gozer/gozer.js" data-circuit="` + id + `"></script></body></html>`)
	w.Write([]byte(b.String()))
}

const defaultStyle = `<style>
#gozer-reconnect{position:fixed;inset:0;z-index:9999;display:flex;align-items:center;justify-content:center;background:rgba(0,0,0,.35)}
#gozer-reconnect[hidden],#gozer-error[hidden]{display:none}
#gozer-reconnect>div{background:#fff;color:#222;padding:1rem 1.5rem;border-radius:.5rem;font:15px system-ui,sans-serif}
#gozer-error{position:fixed;left:0;right:0;bottom:0;z-index:9999;background:#ffe9a8;color:#222;padding:.6rem 1rem;font:15px system-ui,sans-serif;box-shadow:0 -1px 4px rgba(0,0,0,.2)}
</style>`

func (a *App) serveWS(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, a.WebSocketOptions)
	if err != nil {
		return
	}
	conn.SetReadLimit(1 << 20)
	q := r.URL.Query()
	seq, _ := strconv.Atoi(q.Get("s"))
	var c *Circuit
	if id := q.Get("c"); id != "" {
		v, ok := a.circuits.Load(id)
		if !ok {
			conn.Close(closeExpired, "circuit expired")
			return
		}
		c = v.(*Circuit)
	} else {
		c = newCircuit(a, r)
		a.circuits.Store(c.ID, c)
		u := requestURL(r, q.Get("u"))
		c.post(func() { c.start(u) })
	}
	if !c.attach(conn, seq) {
		conn.Close(closeExpired, "circuit cannot be resumed")
		return
	}
	c.readLoop(conn)
}

var voidTags = map[string]bool{"area": true, "base": true, "br": true, "col": true, "embed": true,
	"hr": true, "img": true, "input": true, "link": true, "meta": true, "source": true, "track": true, "wbr": true}

// writeHTML serialises the mounted tree; props are written as attributes.
func writeHTML(b *strings.Builder, m *mnode, inner bool) {
	switch m.kind {
	case kComp:
		for _, ch := range m.children {
			writeHTML(b, ch, false)
		}
		return
	case kText:
		b.WriteString(html.EscapeString(m.text))
		return
	case kRaw:
		b.WriteString(m.text)
		return
	case kComment:
		b.WriteString("<!--gz-->")
		return
	}
	if !inner {
		b.WriteString("<" + m.tag)
		keys := make([]string, 0, len(m.attrs))
		for k := range m.attrs {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			b.WriteString(" " + k)
			if v := m.attrs[k]; v != "" {
				b.WriteString(`="` + html.EscapeString(v) + `"`)
			}
		}
		if v, ok := m.props["value"].(string); ok && m.tag != "textarea" && m.tag != "select" {
			b.WriteString(` value="` + html.EscapeString(v) + `"`)
		}
		if m.props["checked"] == true {
			b.WriteString(" checked")
		}
		b.WriteString(">")
		if voidTags[m.tag] {
			return
		}
		if v, ok := m.props["value"].(string); ok && m.tag == "textarea" {
			b.WriteString(html.EscapeString(v))
		}
	}
	for _, ch := range m.children {
		writeHTML(b, ch, false)
	}
	if !inner {
		b.WriteString("</" + m.tag + ">")
	}
}
