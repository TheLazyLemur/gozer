package gozer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sync"
	"time"
)

// JSRuntime calls JavaScript in the browser and exposes Go functions to it.
type JSRuntime struct {
	c       *Circuit
	mu      sync.Mutex
	next    int
	pending map[int]chan jsResult
	exposed map[string]func([]json.RawMessage) (any, error) // circuit goroutine only
}

type jsResult struct {
	v   json.RawMessage
	err error
}

// Invoke calls the global function at path fn (e.g. "localStorage.getItem")
// and waits for its (awaited) result. Arguments are JSON-encoded; ElementRefs
// arrive as DOM elements. Without a deadline on ctx it times out after a minute.
//
// It blocks the caller; on the circuit goroutine that pauses this circuit's
// rendering and event handling until the browser replies, so JS must not call
// back into Go synchronously from inside an invoked function.
func (j *JSRuntime) Invoke(ctx context.Context, fn string, args ...any) (json.RawMessage, error) {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Minute)
		defer cancel()
	}
	if args == nil {
		args = []any{}
	}
	ch := make(chan jsResult, 1)
	j.mu.Lock()
	j.next++
	id := j.next
	j.pending[id] = ch
	j.mu.Unlock()
	defer func() {
		j.mu.Lock()
		delete(j.pending, id)
		j.mu.Unlock()
	}()
	if err := j.c.send(map[string]any{"t": "j", "i": id, "f": fn, "a": args}); err != nil {
		return nil, err
	}
	select {
	case r := <-ch:
		return r.v, r.err
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-j.c.done:
		return nil, ErrDisconnected
	}
}

// InvokeVoid is Invoke ignoring the result.
func (j *JSRuntime) InvokeVoid(ctx context.Context, fn string, args ...any) error {
	_, err := j.Invoke(ctx, fn, args...)
	return err
}

// InvokeInto calls fn and decodes the result into out.
func (j *JSRuntime) InvokeInto(ctx context.Context, out any, fn string, args ...any) error {
	raw, err := j.Invoke(ctx, fn, args...)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, out)
}

func (j *JSRuntime) resolve(id int, v json.RawMessage, errMsg string) {
	j.mu.Lock()
	ch := j.pending[id]
	j.mu.Unlock()
	if ch == nil {
		return
	}
	r := jsResult{v: v}
	if errMsg != "" {
		r.err = &JSError{Message: errMsg}
	}
	ch <- r
}

func (j *JSRuntime) failAll(err error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	for id, ch := range j.pending {
		select {
		case ch <- jsResult{err: err}:
		default:
		}
		delete(j.pending, id)
	}
}

// JSError is an exception thrown by invoked JavaScript.
type JSError struct{ Message string }

func (e *JSError) Error() string { return "gozer: js: " + e.Message }

// Expose makes fn callable from the browser as `await gozer.invoke(name, ...args)`.
// fn runs on the circuit goroutine. Call from component code (e.g. OnInit), and
// Unexpose in Dispose.
func (j *JSRuntime) Expose(name string, fn func(args []json.RawMessage) (any, error)) {
	j.exposed[name] = fn
}

// Unexpose removes a function added with Expose.
func (j *JSRuntime) Unexpose(name string) { delete(j.exposed, name) }

func (j *JSRuntime) callGo(id int, name string, args []json.RawMessage) {
	reply := map[string]any{"t": "r", "i": id}
	fn := j.exposed[name]
	if fn == nil {
		reply["e"] = fmt.Sprintf("no Go function %q", name)
		j.c.send(reply)
		return
	}
	var v any
	var err error
	func() {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("panic: %v", r)
				j.c.logf("panic in exposed %q: %v", name, r)
			}
		}()
		v, err = fn(args)
	}()
	if err != nil {
		reply["e"] = err.Error()
	} else {
		reply["v"] = v
	}
	j.c.send(reply)
}

// Focus focuses the element.
func (r ElementRef) Focus(ctx context.Context) error {
	if r.circ == nil {
		return errors.New("gozer: focus on an element that was never rendered")
	}
	return r.circ.JS.InvokeVoid(ctx, "gozer.focus", r)
}

// Nav is Blazor's NavigationManager: the current location and programmatic navigation.
type Nav struct {
	c       *Circuit
	uri     *url.URL
	initial bool
	next    int
	subs    map[int]func()
}

// URL returns a copy of the current location.
func (n *Nav) URL() *url.URL {
	u := *n.uri
	return &u
}

// Path is the current URL path.
func (n *Nav) Path() string { return n.uri.Path }

// Query returns the current query parameters.
func (n *Nav) Query() url.Values { return n.uri.Query() }

// NavOption changes how NavigateTo behaves.
type NavOption int

const (
	// ForceLoad does a full page load instead of client-side routing.
	ForceLoad NavOption = 1 << iota
	// Replace replaces the current history entry.
	Replace
)

// NavigateTo navigates to target, resolved against the current URL.
// Call it on the circuit goroutine (event handlers, lifecycle, InvokeAsync).
func (n *Nav) NavigateTo(target string, opts ...NavOption) {
	var o NavOption
	for _, x := range opts {
		o |= x
	}
	ref, err := url.Parse(target)
	if err != nil {
		panic(fmt.Sprintf("gozer: bad navigation target %q: %v", target, err))
	}
	u := n.uri.ResolveReference(ref)
	external := u.Host != n.uri.Host || u.Scheme != n.uri.Scheme
	if external || o&ForceLoad != 0 {
		n.c.emit(opNav, u.String(), o&Replace != 0, true)
		return
	}
	n.c.emit(opNav, u.RequestURI(), o&Replace != 0, false)
	n.set(u)
}

// OnChange subscribes to location changes; call the returned func to unsubscribe.
func (n *Nav) OnChange(f func()) (unsubscribe func()) {
	n.next++
	id := n.next
	n.subs[id] = f
	return func() { delete(n.subs, id) }
}

func (n *Nav) set(u *url.URL) {
	n.initial = false
	n.uri = u
	for _, f := range n.subs {
		f()
	}
}

// clientNavigated handles a link click or back/forward in the browser.
func (n *Nav) clientNavigated(raw string) {
	ref, err := url.Parse(raw)
	if err != nil {
		return
	}
	// only path and query are taken from the client; host stays ours
	n.set(n.uri.ResolveReference(&url.URL{Path: ref.Path, RawQuery: ref.RawQuery}))
}
