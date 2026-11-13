package gozer

import (
	"reflect"
	"sync/atomic"
)

// Component is anything that renders. Embed Base to get state and lifecycle helpers.
type Component interface{ Render() Node }

// Lifecycle hooks; implement the ones you need.
type (
	// Initer runs once, after parameters are first set (OnInitialized).
	Initer interface{ OnInit() }
	// ParamsSetter runs after every parameter assignment (OnParametersSet).
	ParamsSetter interface{ OnParametersSet() }
	// AfterRenderer runs after a render batch reached the browser (OnAfterRender).
	AfterRenderer interface{ OnAfterRender(first bool) }
	// Disposer runs when the component leaves the tree or the circuit ends.
	Disposer interface{ Dispose() }
	// RenderGate can veto re-renders (ShouldRender). The first render always happens.
	RenderGate interface{ ShouldRender() bool }
)

// StateHaser is anything that can be told its state changed.
type StateHaser interface{ StateHasChanged() }

type baser interface{ gozerBase() *Base }

// Base is embedded in components. Its zero value is ready to use.
type Base struct{ inst *instance }

func (b *Base) gozerBase() *Base { return b }

// StateHasChanged schedules a re-render. Safe to call from any goroutine.
func (b *Base) StateHasChanged() {
	if b.inst != nil {
		b.inst.invalidate()
	}
}

// InvokeAsync runs f on the circuit's goroutine and then re-renders the component.
// Use it to apply results computed on other goroutines.
func (b *Base) InvokeAsync(f func()) {
	inst := b.inst
	if inst == nil {
		panic("gozer: InvokeAsync on a component that was never mounted")
	}
	inst.circ.post(func() {
		if inst.disposed {
			return
		}
		inst.circ.guard(inst, f)
		inst.invalidate()
	})
}

// Circuit is the user session this component lives in.
func (b *Base) Circuit() *Circuit {
	if b.inst == nil {
		panic("gozer: Circuit() on a component that was never mounted")
	}
	return b.inst.circ
}

// Disposed reports whether the component has left the tree.
func (b *Base) Disposed() bool { return b.inst != nil && b.inst.disposed }

type instance struct {
	c         Component
	node      *mnode
	parent    *instance
	circ      *Circuit
	depth     int
	dirty     atomic.Bool
	disposed  bool
	failed    bool
	afterDone bool
}

func (i *instance) invalidate() {
	if i.dirty.CompareAndSwap(false, true) {
		i.circ.enqueueDirty(i)
	}
}

// Callback wraps fn so that owner re-renders after it runs, like Blazor's
// EventCallback. Use it for func-typed parameters passed to child components.
func Callback[F any](owner StateHaser, fn F) F {
	v := reflect.ValueOf(fn)
	if owner == nil || v.Kind() != reflect.Func || v.IsNil() {
		return fn
	}
	return reflect.MakeFunc(v.Type(), func(args []reflect.Value) []reflect.Value {
		defer owner.StateHasChanged()
		return v.Call(args)
	}).Interface().(F)
}

// Binding is a two-way bound parameter (@bind-Value): a pointer into the owner's
// state plus the owner to re-render when the child writes through it.
type Binding[T any] struct {
	ptr   *T
	owner StateHaser
}

// BindTo creates a Binding to *ptr owned by owner.
func BindTo[T any](ptr *T, owner StateHaser) Binding[T] { return Binding[T]{ptr, owner} }

// Get returns the bound value, or the zero value if unbound.
func (b Binding[T]) Get() T {
	if b.ptr == nil {
		var z T
		return z
	}
	return *b.ptr
}

// Set writes the value and re-renders the owner.
func (b Binding[T]) Set(v T) {
	if b.ptr == nil {
		panic("gozer: Set on an unbound Binding")
	}
	*b.ptr = v
	if b.owner != nil {
		b.owner.StateHasChanged()
	}
}

// Ptr returns the bound pointer (nil if unbound); forms use it as the field identity.
func (b Binding[T]) Ptr() *T { return b.ptr }
