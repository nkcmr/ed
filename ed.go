package ed

import (
	"context"
	"reflect"
	"sync"
	"sync/atomic"

	"golang.org/x/sync/errgroup"
)

type typeFuncs struct {
	wrappers, handlers []int
}

func (t *typeFuncs) get(kind fnKind) []int {
	switch kind {
	case fnKindHandler:
		return t.handlers
	case fnKindWrapper:
		return t.wrappers
	}
	panic("unreachable")
}

func (t *typeFuncs) push(kind fnKind, i ...int) {
	switch kind {
	case fnKindHandler:
		t.handlers = append(t.handlers, i...)
		return
	case fnKindWrapper:
		t.wrappers = append(t.wrappers, i...)
		return
	}
	panic("unreachable")
}

var idseq atomic.Int64

// Dispatcher is the object that represents how events of particular types are
// routed to registered [Handler] and [Wrapper]. The zero value is ready to
// use.
//
// A Dispatcher is safe for concurrent use; registrations and dispatches may
// come from any goroutine. There is one caveat: [Dispatcher.Dispatch] holds a
// read lock for the whole call, including while handlers and wrappers run. A
// handler or wrapper must therefore never call [Dispatcher.Register] or
// [Dispatcher.Wrap] on the Dispatcher that is currently dispatching to it,
// since that will deadlock. Dispatching a further event from inside a handler
// relies on recursive read-locking, and can deadlock as well if a registration
// arrives concurrently. The simplest way to avoid both: finish registering
// handlers and wrappers before the first event is dispatched.
type Dispatcher struct {
	l        sync.RWMutex
	fns      map[int]reflect.Value
	ifaces   map[reflect.Type]*typeFuncs
	concrete map[reflect.Type]*typeFuncs
}

var globalRouter = new(Dispatcher)

// Register binds a [Handler] to a particular event by it's event type (E). The
// registered event type may also be an interface, to allow for capturing
// multiple types in one handler.
func Register[E any](handler Handler[E]) {
	globalRouter.Register(handler)
}

// Dispatch will send the provided event to all registered handlers and
// wrappers and allow them to return an error if necessary.
//
// Events are routed by their dynamic type, so dispatching a value held in an
// interface variable still reaches handlers registered for the concrete type
// underneath. For that reason the event must not be a nil interface value:
// there is no dynamic type to route on, and Dispatch will panic.
func Dispatch[E any](ctx context.Context, event E) error {
	return globalRouter.Dispatch(ctx, event)
}

// Wrap will allow a [Wrapper] function to be called before any [Handler] of a
// matching event. The use-case for wrapping tends to be things like
// observability (logging, metrics, tracing, etc.). Wrapper functions that match
// a particular [Dispatch] will all be called serially, but the order they are
// called in is not guaranteed and may change between releases.
func Wrap[E any](wrapper Wrapper[E]) {
	globalRouter.Wrap(wrapper)
}

// Wrapper is a function that will be called before an Handler is called for a
// particular [Dispatch] call. With each [Dispatch] call, zero or many Wrapper
// functions might be called, but they will all be guaranteed to be called
// serially. Once all wrapper functions have invoked their next(), the actual
// [Handler] functions will be invoked.
//
// The relative order in which matching Wrapper functions are nested is not
// guaranteed and may change between releases. Do not write a Wrapper that
// depends on running inside or outside any other particular Wrapper.
type Wrapper[E any] func(ctx context.Context, event E, next func(context.Context) error) error

// Wrap does the equivalent of [Wrap] on an explicit [Dispatcher].
func (d *Dispatcher) Wrap[E any](wrapper Wrapper[E]) {
	d.bindFunc[E](fnKindWrapper, reflect.ValueOf(wrapper))
}

func (d *Dispatcher) init() {
	if d.fns == nil {
		d.fns = map[int]reflect.Value{}
	}
	if d.concrete == nil {
		d.concrete = map[reflect.Type]*typeFuncs{}
	}
	if d.ifaces == nil {
		d.ifaces = map[reflect.Type]*typeFuncs{}
	}
}

type fnKind int

const (
	_ = fnKind(iota)
	fnKindHandler
	fnKindWrapper
)

// Register does the equivalent of [Register] on an explicit [Dispatcher].
func (d *Dispatcher) Register[E any](handler Handler[E]) {
	d.bindFunc[E](fnKindHandler, reflect.ValueOf(handler))
}

func (d *Dispatcher) bindFunc[E any](kind fnKind, fn reflect.Value) {
	d.l.Lock()
	defer d.l.Unlock()
	d.init()

	eventType := reflect.TypeFor[E]()
	fid := int(idseq.Add(1))
	d.fns[fid] = fn

	if eventType.Kind() == reflect.Interface {
		ifm, ok := d.ifaces[eventType]
		if !ok {
			ifm = new(typeFuncs)
			d.ifaces[eventType] = ifm
		}
		ifm.push(kind, fid)
		for ct, tf := range d.concrete {
			if ct.Implements(eventType) {
				tf.push(kind, fid)
			}
		}
		return
	}

	ctm, ok := d.concrete[eventType]
	if !ok {
		ctm = new(typeFuncs)
		d.concrete[eventType] = ctm
		for iface, tf := range d.ifaces {
			if eventType.Implements(iface) {
				ctm.push(fnKindHandler, tf.get(fnKindHandler)...)
				ctm.push(fnKindWrapper, tf.get(fnKindWrapper)...)
			}
		}
	}
	ctm.push(kind, fid)
}

// Handler is a function responsible for handling an event. Returning an error
// from a Handler function will cause the [Dispatch] operation that triggered
// it to return that error. Sibling handlers are not canceled: every matching
// Handler for a [Dispatch] runs to completion, and if more than one of them
// fails, one of their errors is returned.
//
// All matching Handler functions for a single [Dispatch] are invoked
// concurrently, each on its own goroutine, and the order they run in is not
// guaranteed and may change between releases. A Handler must therefore be safe
// to run alongside every other Handler that matches the same event.
type Handler[E any] func(ctx context.Context, event E) error

// Dispatch does the equivalent of [Dispatch] on an explicit [Dispatcher].
func (d *Dispatcher) Dispatch[E any](ctx context.Context, event E) error {
	d.l.RLock()
	defer d.l.RUnlock()

	if d.ifaces == nil && d.concrete == nil {
		return nil
	}

	eventValue := reflect.ValueOf(event)
	var handlers, wrappers []int
	tf, ok := d.concrete[eventValue.Type()]
	if !ok {
		for iface, tf := range d.ifaces {
			// todo, memoize? store in Dispatcher.concrete
			if eventValue.Type().Implements(iface) {
				handlers = append(handlers, tf.handlers...)
				wrappers = append(wrappers, tf.wrappers...)
			}
		}
	} else {
		handlers = tf.handlers
		wrappers = tf.wrappers
	}

	if len(handlers) == 0 {
		return nil
	}

	top := func(ctx context.Context) error {
		var g errgroup.Group
		in := []reflect.Value{
			reflect.ValueOf(ctx),
			eventValue,
		}
		for _, handlerID := range handlers {
			hid := handlerID
			g.Go(func() error {
				out := d.fns[hid].Call(in)
				outv := out[0]
				if outv.IsNil() {
					return nil
				}
				return outv.Interface().(error)
			})
		}
		return g.Wait()
	}

	for _, w := range wrappers {
		thisnext := top
		wfn := d.fns[w]
		top = func(ctx context.Context) error {
			in := []reflect.Value{
				reflect.ValueOf(ctx),
				eventValue,
				reflect.ValueOf(thisnext),
			}
			out := wfn.Call(in)
			outv := out[0]
			if outv.IsNil() {
				return nil
			}
			return outv.Interface().(error)
		}
	}

	return top(ctx)
}
