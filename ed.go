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
// routed to registered [Handler] and [Wrapper].
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
	Using[E](globalRouter).Register(handler)
}

// Dispatch will send the provided event to all registered handlers and
// wrappers and allow them to return an error if necessary.
func Dispatch[E any](ctx context.Context, event E) error {
	return Using[E](globalRouter).Dispatch(ctx, event)
}

// Wrap will allow a [Wrapper] function to be called before any [Handler] of a
// matching event. The use-case for wrapping tends to be things like
// observability (logging, metrics, tracing, etc.). Wrapper functions that match
// a particular [Dispatch] will all be called serially in the order they were
// setup.
func Wrap[E any](wrapper Wrapper[E]) {
	Using[E](globalRouter).Wrap(wrapper)
}

type typedDispatch[E any] struct {
	d *Dispatcher
}

// Wrapper is a function that will be called before an Handler is called for a
// particular [Dispatch] call. With each [Dispatch] call, zero or many Wrapper
// functions might be called, but they will all be guaranteed to be called
// serially. Once all wrapper functions have invoked their next(), the actual
// [Handler] functions will be invoked.
type Wrapper[E any] func(ctx context.Context, event E, next func(context.Context) error) error

func (t *typedDispatch[E]) Wrap(wrapper Wrapper[E]) {
	t.bindFunc(fnKindWrapper, reflect.ValueOf(wrapper))
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

func (t *typedDispatch[E]) bindFunc(kind fnKind, fn reflect.Value) {
	t.d.l.Lock()
	defer t.d.l.Unlock()
	t.d.init()

	eventType := reflect.TypeFor[E]()
	fid := int(idseq.Add(1))
	t.d.fns[fid] = fn

	if eventType.Kind() == reflect.Interface {
		ifm, ok := t.d.ifaces[eventType]
		if !ok {
			ifm = new(typeFuncs)
			t.d.ifaces[eventType] = ifm
		}
		ifm.push(kind, fid)
		for ct, tf := range t.d.concrete {
			if ct.Implements(eventType) {
				tf.push(kind, fid)
			}
		}
		return
	}

	ctm, ok := t.d.concrete[eventType]
	if !ok {
		ctm = new(typeFuncs)
		t.d.concrete[eventType] = ctm
		for iface, tf := range t.d.ifaces {
			if eventType.Implements(iface) {
				ctm.push(fnKindHandler, tf.get(fnKindHandler)...)
				ctm.push(fnKindWrapper, tf.get(fnKindWrapper)...)
			}
		}
	}
	ctm.push(kind, fid)
}

// Handler is a function responsible for handling an event. Returning an error
// from a Handler function will cause the entire dispatch operation for a
// Dispatch() to be canceled and will return the error.
type Handler[E any] func(ctx context.Context, event E) error

func (t *typedDispatch[E]) Register(handler Handler[E]) {
	t.bindFunc(fnKindHandler, reflect.ValueOf(handler))
}

func (t *typedDispatch[E]) Dispatch(ctx context.Context, event E) error {
	t.d.l.RLock()
	defer t.d.l.RUnlock()

	if t.d.ifaces == nil && t.d.concrete == nil {
		return nil
	}

	eventValue := reflect.ValueOf(event)
	var handlers, wrappers []int
	tf, ok := t.d.concrete[eventValue.Type()]
	if !ok {
		for iface, tf := range t.d.ifaces {
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
				out := t.d.fns[hid].Call(in)
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
		wfn := t.d.fns[w]
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

// Using allows an instance of [Dispatcher] to be used with a specific type. The
// returned value of this is not meant to be "long-lived" or stored anywhere;
// rather used ephemerally and called as a chain.
func Using[E any](r *Dispatcher) interface {
	// Wrap does the equivalent of [Wrap] on an explicit [Dispatcher].
	Wrap(wrapper Wrapper[E])

	// Register does the equivalent of [Register] on an explicit [Dispatcher].
	Register(handler Handler[E])

	// Dispatch does the equivalent of [Dispatch] on an explict [Dispatcher].
	Dispatch(ctx context.Context, event E) error
} {
	return &typedDispatch[E]{d: r}
}
