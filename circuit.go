package gozer

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"reflect"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// Circuit is one live user session: a component tree, its dispatcher goroutine
// and the browser connection. All component code runs on the circuit goroutine.
type Circuit struct {
	ID  string
	Nav *Nav
	JS  *JSRuntime

	app    *App
	req    *http.Request
	user   *User
	ctx    context.Context
	cancel context.CancelFunc
	kick   chan struct{}
	done   chan struct{}
	once   sync.Once

	mu      sync.Mutex // guards the fields below
	queue   []func()
	dirtyq  []*instance
	conn    *websocket.Conn
	expiry  *time.Timer
	closed  bool
	scoped  map[reflect.Type]reflect.Value
	notFnd  bool
	started bool

	// owned by the circuit goroutine
	nodes        map[int]*mnode
	building     int
	nextID       int
	root         *mnode
	ops          []any
	rendered     []*instance
	pendingAfter []*instance
	unacked      []batch
	seq, sent    int
	acked        int
	fatal        error
	title        string
	titles       []headPart // live PageTitles in mount order; the last one wins
	heads        []headPart // live HeadContents in mount order
	head         string     // HeadContent HTML last sent
}

type batch struct {
	seq int
	ops []any
}

const rootID = 1

func newCircuit(a *App, r *http.Request) *Circuit {
	b := make([]byte, 16)
	rand.Read(b)
	ctx, cancel := context.WithCancel(context.Background())
	c := &Circuit{
		ID:     hex.EncodeToString(b),
		app:    a,
		req:    r.Clone(context.Background()),
		ctx:    ctx,
		cancel: cancel,
		kick:   make(chan struct{}, 1),
		done:   make(chan struct{}),
		nodes:  map[int]*mnode{},
		nextID: rootID,
		scoped: map[reflect.Type]reflect.Value{},
	}
	c.req.Body = http.NoBody
	c.root = &mnode{kind: kElem, id: rootID, tag: "div"}
	c.nodes[rootID] = c.root
	c.JS = &JSRuntime{c: c, pending: map[int]chan jsResult{}, exposed: map[string]func([]json.RawMessage) (any, error){}}
	if a.Authenticate != nil {
		c.user = a.Authenticate(r)
	}
	go c.loop()
	return c
}

// Request is the HTTP request that opened the circuit (headers and cookies; no body).
func (c *Circuit) Request() *http.Request { return c.req }

// User is the authenticated user, nil when anonymous.
func (c *Circuit) User() *User { return c.user }

// Context is cancelled when the circuit ends.
func (c *Circuit) Context() context.Context { return c.ctx }

// start mounts the root component for the given location. Circuit goroutine only.
func (c *Circuit) start(u *url.URL) {
	c.Nav = &Nav{c: c, uri: u, initial: true, subs: map[int]func(){}}
	c.reconcile(c.root, prepare([]Node{&compNode{typ: c.app.rootType, mk: c.app.Root, set: func(Component) {}}}), nil)
}

func (c *Circuit) post(f func()) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.queue = append(c.queue, f)
	c.mu.Unlock()
	c.wake()
}

func (c *Circuit) wake() {
	select {
	case c.kick <- struct{}{}:
	default:
	}
}

// sync runs f on the circuit goroutine and waits. Returns false if the circuit is gone.
func (c *Circuit) sync(f func()) bool {
	done := make(chan struct{})
	c.post(func() { f(); close(done) })
	select {
	case <-done:
		return true
	case <-c.done:
		return false
	}
}

func (c *Circuit) enqueueDirty(i *instance) {
	c.mu.Lock()
	c.dirtyq = append(c.dirtyq, i)
	c.mu.Unlock()
	c.wake()
}

func (c *Circuit) loop() {
	for {
		select {
		case <-c.kick:
		case <-c.done:
			c.teardown()
			return
		}
		for {
			c.mu.Lock()
			var f func()
			if len(c.queue) > 0 {
				f = c.queue[0]
				c.queue = c.queue[1:]
			}
			c.mu.Unlock()
			if f != nil {
				f()
			}
			c.flush()
			if c.fatal != nil {
				c.logf("circuit terminated: %v", c.fatal)
				c.send(map[string]any{"t": "x", "m": "An unhandled error has occurred."})
				c.Close()
				c.teardown()
				return
			}
			if f == nil {
				break
			}
		}
	}
}

// Close ends the circuit and disposes every component.
func (c *Circuit) Close() {
	c.once.Do(func() {
		c.mu.Lock()
		c.closed = true
		c.mu.Unlock()
		close(c.done)
	})
}

func (c *Circuit) teardown() {
	c.app.circuits.Delete(c.ID)
	c.dispose(c.root)
	c.cancel()
	c.mu.Lock()
	conn := c.conn
	c.conn = nil
	if c.expiry != nil {
		c.expiry.Stop()
	}
	c.mu.Unlock()
	if conn != nil {
		code := websocket.StatusNormalClosure
		if c.fatal != nil {
			code = closeFatal
		}
		conn.Close(code, "circuit closed")
	}
}

func (c *Circuit) logf(format string, args ...any) {
	log.Printf("gozer[%s]: %s", c.ID[:8], fmt.Sprintf(format, args...))
}

// guard runs user code for inst; a panic goes to the nearest ErrorBoundary
// above inst, or terminates the circuit if there is none.
func (c *Circuit) guard(inst *instance, f func()) (ok bool) {
	defer func() {
		if r := recover(); r != nil {
			ok = false
			c.fail(inst, fmt.Errorf("%v", r), debug.Stack())
		}
	}()
	f()
	return true
}

func (c *Circuit) fail(inst *instance, err error, stack []byte) {
	c.logf("unhandled error in %s: %v\n%s", typeName(inst), err, stack)
	if inst == nil {
		c.fatal = err
		return
	}
	inst.failed = true
	for p := inst.parent; p != nil; p = p.parent {
		if b, ok := p.c.(*ErrorBoundary); ok && !p.failed && !p.disposed && b.err == nil {
			b.err = err
			p.invalidate()
			return
		}
	}
	c.fatal = err
}

func typeName(inst *instance) string {
	if inst == nil {
		return "circuit"
	}
	return fmt.Sprintf("%T", inst.c)
}

// flush renders dirty components, sends pending batches and runs OnAfterRender.
func (c *Circuit) flush() {
	for range 100 {
		c.renderDirty()
		if len(c.ops) > 0 {
			c.seq++
			c.unacked = append(c.unacked, batch{c.seq, c.ops})
			c.ops = nil
		}
		c.pendingAfter = append(c.pendingAfter, c.rendered...)
		c.rendered = nil
		if !c.transmit() || len(c.pendingAfter) == 0 {
			return
		}
		after := c.pendingAfter
		c.pendingAfter = nil
		seen := map[*instance]bool{}
		for _, inst := range after {
			if seen[inst] || inst.disposed {
				continue
			}
			seen[inst] = true
			first := !inst.afterDone
			inst.afterDone = true
			if a, ok := inst.c.(AfterRenderer); ok {
				c.guard(inst, func() { a.OnAfterRender(first) })
			}
		}
		c.mu.Lock()
		more := len(c.dirtyq) > 0
		c.mu.Unlock()
		if !more && len(c.ops) == 0 {
			return
		}
	}
}

// transmit sends unsent batches; false when there is no connection.
func (c *Circuit) transmit() bool {
	c.mu.Lock()
	conn := c.conn
	c.mu.Unlock()
	if conn == nil {
		return false
	}
	for _, b := range c.unacked {
		if b.seq <= c.sent {
			continue
		}
		if err := c.write(conn, map[string]any{"t": "b", "n": b.seq, "o": b.ops}); err != nil {
			conn.CloseNow()
			return false
		}
		c.sent = b.seq
	}
	return true
}

func (c *Circuit) write(conn *websocket.Conn, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("gozer: cannot encode message: %v", err))
	}
	ctx, cancel := context.WithTimeout(c.ctx, 10*time.Second)
	defer cancel()
	return conn.Write(ctx, websocket.MessageText, data)
}

func (c *Circuit) send(v any) error {
	c.mu.Lock()
	conn := c.conn
	c.mu.Unlock()
	if conn == nil {
		return ErrDisconnected
	}
	return c.write(conn, v)
}

func (c *Circuit) ack(n int) {
	i := 0
	for i < len(c.unacked) && c.unacked[i].seq <= n {
		i++
	}
	c.unacked = c.unacked[i:]
	c.acked = max(c.acked, n)
}

const (
	closeExpired = websocket.StatusCode(4001)
	closeFatal   = websocket.StatusCode(4002)
)

// attach makes conn the circuit's connection; the client has applied batches up to seq.
func (c *Circuit) attach(conn *websocket.Conn, seq int) bool {
	var ok bool
	if !c.sync(func() {
		// a second tab replaying an old page cannot be resumed: its batches are gone
		if seq < c.acked || seq > c.seq {
			return
		}
		ok = true
		c.mu.Lock()
		old := c.conn
		c.conn = conn
		if c.expiry != nil {
			c.expiry.Stop()
		}
		c.mu.Unlock()
		if old != nil {
			old.Close(closeExpired, "replaced by a newer connection")
		}
		c.ack(seq)
		c.sent = seq
		c.write(conn, map[string]any{"t": "h", "c": c.ID})
	}) {
		return false
	}
	return ok
}

func (c *Circuit) detach(conn *websocket.Conn) {
	c.post(func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.conn != conn {
			return
		}
		c.conn = nil
		c.scheduleExpiry()
	})
	c.JS.failAll(ErrDisconnected)
}

// scheduleExpiry closes the circuit if nobody connects in time. Caller holds mu.
func (c *Circuit) scheduleExpiry() {
	if c.expiry != nil {
		c.expiry.Stop()
	}
	c.expiry = time.AfterFunc(c.app.Retention, func() {
		c.post(func() {
			c.mu.Lock()
			idle := c.conn == nil
			c.mu.Unlock()
			if idle {
				c.Close()
			}
		})
	})
}

type inMsg struct {
	T string            `json:"t"`
	I int               `json:"i"`
	N string            `json:"n"`
	S int               `json:"s"`
	D *Event            `json:"d"`
	U string            `json:"u"`
	F string            `json:"f"`
	A []json.RawMessage `json:"a"`
	V json.RawMessage   `json:"v"`
	E string            `json:"e"`
}

func (c *Circuit) readLoop(conn *websocket.Conn) {
	defer c.detach(conn)
	go func() { // keepalive so dead peers are noticed
		t := time.NewTicker(20 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				ctx, cancel := context.WithTimeout(c.ctx, 15*time.Second)
				err := conn.Ping(ctx)
				cancel()
				if err != nil {
					conn.CloseNow()
					return
				}
			case <-c.ctx.Done():
				return
			}
		}
	}()
	for {
		_, data, err := conn.Read(c.ctx)
		if err != nil {
			return
		}
		var m inMsg
		if err := json.Unmarshal(data, &m); err != nil {
			c.logf("bad message: %v", err)
			conn.Close(websocket.StatusPolicyViolation, "bad message")
			return
		}
		switch m.T {
		case "r":
			c.JS.resolve(m.I, m.V, m.E)
		case "a":
			c.post(func() { c.ack(m.S) })
		case "e":
			ev := Event{}
			if m.D != nil {
				ev = *m.D
			}
			c.post(func() { c.dispatchEvent(m.I, m.N, ev) })
		case "n":
			c.post(func() { c.Nav.clientNavigated(m.U) })
		case "c":
			c.post(func() { c.JS.callGo(m.I, m.F, m.A) })
		}
	}
}

func (c *Circuit) dispatchEvent(id int, name string, ev Event) {
	m := c.nodes[id]
	if m == nil || m.kind != kElem {
		return // stale event for a node that is gone
	}
	h := m.events[name]
	if h == nil {
		return
	}
	// the browser already shows these values; record them so the diff does not
	// echo stale ones back over newer keystrokes (Blazor does the same)
	if _, ok := m.props["value"]; ok {
		m.props["value"] = ev.Value
	}
	if _, ok := m.props["checked"]; ok {
		m.props["checked"] = ev.Checked
	}
	inst := m.owner
	c.guard(inst, func() {
		switch f := h.fn.(type) {
		case func():
			f()
		case func(Event):
			f(ev)
		}
	})
	if inst != nil && !inst.disposed {
		inst.invalidate()
	}
	if h.owner != nil {
		h.owner.StateHasChanged()
	}
}

type headPart struct {
	owner Component
	text  string
}

// putPart sets owner's entry, or removes it when remove is true.
func putPart(parts []headPart, owner Component, text string, remove bool) []headPart {
	i := slices.IndexFunc(parts, func(h headPart) bool { return h.owner == owner })
	switch {
	case remove && i >= 0:
		return slices.Delete(parts, i, i+1)
	case remove:
		return parts
	case i >= 0:
		parts[i].text = text
		return parts
	}
	return append(parts, headPart{owner, text})
}

func (c *Circuit) setTitle(owner Component, t string, remove bool) {
	c.titles = putPart(c.titles, owner, t, remove)
	t = c.app.Title
	if n := len(c.titles); n > 0 {
		t = c.titles[n-1].text
	}
	if t != c.title {
		c.title = t
		c.emit(opTitle, t)
	}
}

func (c *Circuit) setHead(owner Component, html string, remove bool) {
	c.heads = putPart(c.heads, owner, html, remove)
	var b strings.Builder
	for _, h := range c.heads {
		b.WriteString(h.text)
	}
	if s := b.String(); s != c.head {
		c.head = s
		c.emit(opHead, s)
	}
}

// inject fills fields tagged `inject:""` from the app's services.
func (c *Circuit) inject(inst *instance) {
	v := reflect.ValueOf(inst.c)
	if v.Kind() != reflect.Pointer || v.Elem().Kind() != reflect.Struct {
		return
	}
	v = v.Elem()
	t := v.Type()
	for i := range t.NumField() {
		f := t.Field(i)
		if _, ok := f.Tag.Lookup("inject"); !ok {
			continue
		}
		v.Field(i).Set(c.service(f.Type))
	}
}

func (c *Circuit) service(t reflect.Type) reflect.Value {
	switch t {
	case reflect.TypeFor[*Circuit]():
		return reflect.ValueOf(c)
	case reflect.TypeFor[*Nav]():
		return reflect.ValueOf(c.Nav)
	case reflect.TypeFor[*JSRuntime]():
		return reflect.ValueOf(c.JS)
	case reflect.TypeFor[*User]():
		return reflect.ValueOf(c.user)
	}
	if v, ok := c.app.singletons[t]; ok {
		return v
	}
	if f, ok := c.app.scoped[t]; ok {
		c.mu.Lock()
		v, ok := c.scoped[t]
		c.mu.Unlock()
		if !ok {
			v = f.Call([]reflect.Value{reflect.ValueOf(c)})[0]
			c.mu.Lock()
			c.scoped[t] = v
			c.mu.Unlock()
		}
		return v
	}
	panic(fmt.Sprintf("gozer: no service registered for %s", t))
}

// Service returns the service of type T for this circuit.
func Service[T any](c *Circuit) T {
	return c.service(reflect.TypeFor[T]()).Interface().(T)
}

// cascade fills fields tagged `cascade:""` (by type) or `cascade:"Name"` (by name)
// from the nearest enclosing CascadingValue.
func (c *Circuit) cascade(inst *instance) {
	v := reflect.ValueOf(inst.c)
	if v.Kind() != reflect.Pointer || v.Elem().Kind() != reflect.Struct {
		return
	}
	v = v.Elem()
	t := v.Type()
	for i := range t.NumField() {
		f := t.Field(i)
		name, ok := f.Tag.Lookup("cascade")
		if !ok {
			continue
		}
		for p := inst.parent; p != nil; p = p.parent {
			cv, ok := p.c.(*CascadingValue)
			if !ok || cv.Value == nil || cv.Name != name {
				continue
			}
			if val := reflect.ValueOf(cv.Value); val.Type().AssignableTo(f.Type) {
				v.Field(i).Set(val)
				break
			}
		}
	}
}

// ErrDisconnected is returned by JS interop while no browser is connected
// (for example during prerendering).
var ErrDisconnected = errors.New("gozer: browser not connected")
