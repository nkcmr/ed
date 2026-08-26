package ed

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// shared fixtures
// ---------------------------------------------------------------------------

// fooer and barer are interfaces used to exercise interface-based routing.
type fooer interface{ Foo() }

type barer interface{ Bar() }

// noImplementers is deliberately satisfied by nothing in this package.
type noImplementers interface{ noOneShouldImplementThis() }

// concreteA implements fooer only.
type concreteA struct {
	onfoo func()
}

func (c concreteA) Foo() {
	if c.onfoo != nil {
		c.onfoo()
	}
}

// concreteB implements both fooer and barer.
type concreteB struct{ id string }

func (concreteB) Foo() {}
func (concreteB) Bar() {}

// concreteC implements neither fooer nor barer.
type concreteC struct{ id string }

// recorder collects ordered call markers. Handlers run on their own
// goroutines, so every access is guarded.
type recorder struct {
	mu   sync.Mutex
	seen []string
}

func (r *recorder) add(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seen = append(r.seen, fmt.Sprintf(format, args...))
}

func (r *recorder) calls() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.seen...)
}

// counter returns a Handler that only bumps n.
func counter[E any](n *atomic.Int64) Handler[E] {
	return func(ctx context.Context, event E) error {
		n.Add(1)
		return nil
	}
}

// recording returns a Handler that bumps n and notes name in r.
func recording[E any](r *recorder, name string, n *atomic.Int64) Handler[E] {
	return func(ctx context.Context, event E) error {
		n.Add(1)
		r.add("%s", name)
		return nil
	}
}

// ---------------------------------------------------------------------------
// core dispatch
// ---------------------------------------------------------------------------

func TestDispatch_ConcreteType(t *testing.T) {
	var d Dispatcher
	type simpleEvent struct{ foo string }

	var calls atomic.Int64
	d.Register(func(ctx context.Context, event simpleEvent) error {
		// handlers run on their own goroutine: assert, never require.
		assert.Equal(t, simpleEvent{foo: "cool! an event!"}, event)
		calls.Add(1)
		return nil
	})

	require.NoError(t, d.Dispatch(context.Background(), simpleEvent{foo: "cool! an event!"}))
	require.Equal(t, int64(1), calls.Load())
}

func TestDispatch_MultipleHandlersAllFire(t *testing.T) {
	var d Dispatcher
	type ev struct{}

	const n = 8
	var calls atomic.Int64
	for range n {
		d.Register(counter[ev](&calls))
	}

	require.NoError(t, d.Dispatch(context.Background(), ev{}))
	require.Equal(t, int64(n), calls.Load())

	// handlers are not consumed: a second dispatch runs all of them again.
	require.NoError(t, d.Dispatch(context.Background(), ev{}))
	require.Equal(t, int64(2*n), calls.Load())
}

func TestDispatch_HandlerCountPathsAgree(t *testing.T) {
	// Dispatch calls a lone handler inline and fans out to an errgroup for
	// two or more. Both paths must behave identically, so exercise the
	// boundary on either side of it.
	for _, n := range []int{1, 2, 3} {
		t.Run(fmt.Sprintf("handlers=%d", n), func(t *testing.T) {
			type ev struct{ payload string }

			t.Run("all fire with the right event", func(t *testing.T) {
				var d Dispatcher
				var calls atomic.Int64
				for range n {
					d.Register(func(ctx context.Context, event ev) error {
						assert.Equal(t, ev{payload: "p"}, event)
						calls.Add(1)
						return nil
					})
				}

				require.NoError(t, d.Dispatch(context.Background(), ev{payload: "p"}))
				require.Equal(t, int64(n), calls.Load())
			})

			t.Run("error propagates", func(t *testing.T) {
				var d Dispatcher
				var calls atomic.Int64
				d.Register(func(ctx context.Context, event ev) error {
					calls.Add(1)
					return errBoom
				})
				for range n - 1 {
					d.Register(counter[ev](&calls))
				}

				require.ErrorIs(t, d.Dispatch(context.Background(), ev{}), errBoom)
				require.Equal(t, int64(n), calls.Load())
			})

			t.Run("wrappers still apply", func(t *testing.T) {
				var d Dispatcher
				var calls, wrapped atomic.Int64
				d.Wrap(func(ctx context.Context, event ev, next func(context.Context) error) error {
					wrapped.Add(1)
					return next(ctx)
				})
				for range n {
					d.Register(counter[ev](&calls))
				}

				require.NoError(t, d.Dispatch(context.Background(), ev{}))
				require.Equal(t, int64(n), calls.Load())
				require.Equal(t, int64(1), wrapped.Load())
			})
		})
	}
}

func TestDispatch_SingleHandlerPanicReachesCaller(t *testing.T) {
	// A lone handler runs on the caller's goroutine, so its panic unwinds
	// through Dispatch and is recoverable. This is NOT true once there are
	// two or more handlers: those run on errgroup goroutines, where a panic
	// takes the process down. Documenting the asymmetry, not endorsing it.
	var d Dispatcher
	type ev struct{}

	d.Register(func(ctx context.Context, event ev) error {
		panic("handler blew up")
	})

	require.PanicsWithValue(t, "handler blew up", func() {
		_ = d.Dispatch(context.Background(), ev{})
	})
}

func TestDispatch_ZeroValueDispatcherIsUsable(t *testing.T) {
	// a Dispatcher that has never been registered against has nil maps; it
	// must still dispatch without panicking.
	var d Dispatcher
	type ev struct{}

	require.NoError(t, d.Dispatch(context.Background(), ev{}))
	require.NoError(t, d.Dispatch(context.Background(), 42))
	require.NoError(t, d.Dispatch(context.Background(), concreteA{}))
}

func TestDispatch_UnregisteredTypeIsNoop(t *testing.T) {
	var d Dispatcher
	type registered struct{}
	type unregistered struct{}

	var calls atomic.Int64
	d.Register(counter[registered](&calls))

	require.NoError(t, d.Dispatch(context.Background(), unregistered{}))
	require.Zero(t, calls.Load())

	// the unrelated dispatch did not disturb the registered type.
	require.NoError(t, d.Dispatch(context.Background(), registered{}))
	require.Equal(t, int64(1), calls.Load())
}

func TestDispatch_EventTypes(t *testing.T) {
	type named string
	type withFields struct {
		A int
		B string
		C []int
	}

	t.Run("int", func(t *testing.T) { roundTrip(t, 42) })
	t.Run("negative int", func(t *testing.T) { roundTrip(t, -1) })
	t.Run("string", func(t *testing.T) { roundTrip(t, "hello") })
	t.Run("named string", func(t *testing.T) { roundTrip(t, named("hello")) })
	t.Run("bool", func(t *testing.T) { roundTrip(t, true) })
	t.Run("struct", func(t *testing.T) { roundTrip(t, withFields{A: 1, B: "b", C: []int{1, 2}}) })
	t.Run("pointer", func(t *testing.T) { roundTrip(t, &withFields{A: 9}) })
	t.Run("slice", func(t *testing.T) { roundTrip(t, []string{"a", "b"}) })
	t.Run("map", func(t *testing.T) { roundTrip(t, map[string]int{"a": 1}) })
	t.Run("empty struct", func(t *testing.T) { roundTrip(t, struct{}{}) })
}

// roundTrip registers a handler for E, dispatches event and asserts the
// handler saw exactly that value, exactly once.
func roundTrip[E any](t *testing.T, event E) {
	t.Helper()

	var d Dispatcher
	var got E
	var calls atomic.Int64
	d.Register(func(ctx context.Context, e E) error {
		got = e
		calls.Add(1)
		return nil
	})

	require.NoError(t, d.Dispatch(context.Background(), event))
	require.Equal(t, int64(1), calls.Load())
	require.Equal(t, event, got)
}

func TestDispatch_PointerIdentityIsPreserved(t *testing.T) {
	type ev struct{ n int }

	var d Dispatcher
	var got *ev
	d.Register(func(ctx context.Context, e *ev) error {
		got = e
		return nil
	})

	want := &ev{n: 7}
	require.NoError(t, d.Dispatch(context.Background(), want))
	require.Same(t, want, got)
}

func TestDispatch_TypedNilPointerEvent(t *testing.T) {
	type ev struct{}

	var d Dispatcher
	var calls atomic.Int64
	var gotNil bool
	d.Register(func(ctx context.Context, e *ev) error {
		gotNil = e == nil
		calls.Add(1)
		return nil
	})

	require.NoError(t, d.Dispatch(context.Background(), (*ev)(nil)))
	require.Equal(t, int64(1), calls.Load())
	require.True(t, gotNil)
}

func TestDispatch_NilInterfaceEventPanics(t *testing.T) {
	// Routing is done off the event's *dynamic* type, which an untyped nil
	// does not have. This pins current behaviour rather than endorsing it: if
	// Dispatch ever learns to treat a nil event as a no-op, delete this test.
	var d Dispatcher
	d.Register(func(ctx context.Context, e any) error { return nil })

	require.Panics(t, func() {
		_ = d.Dispatch[any](context.Background(), nil)
	})
}

func TestDispatch_ContextIsPassedThrough(t *testing.T) {
	type ctxKey struct{}
	type ev struct{}

	var d Dispatcher
	var got any
	d.Register(func(ctx context.Context, e ev) error {
		got = ctx.Value(ctxKey{})
		return nil
	})

	ctx := context.WithValue(context.Background(), ctxKey{}, "carried")
	require.NoError(t, d.Dispatch(ctx, ev{}))
	require.Equal(t, "carried", got)
}

func TestDispatch_DispatchersAreIndependent(t *testing.T) {
	type ev struct{}

	var d1, d2 Dispatcher
	var c1, c2 atomic.Int64
	d1.Register(counter[ev](&c1))
	d2.Register(counter[ev](&c2))

	require.NoError(t, d1.Dispatch(context.Background(), ev{}))
	require.Equal(t, int64(1), c1.Load())
	require.Zero(t, c2.Load())

	require.NoError(t, d2.Dispatch(context.Background(), ev{}))
	require.Equal(t, int64(1), c1.Load())
	require.Equal(t, int64(1), c2.Load())
}

// ---------------------------------------------------------------------------
// interface routing
// ---------------------------------------------------------------------------

func TestInterface_HandlerReceivesConcreteEvent(t *testing.T) {
	// no handler is registered for the concrete type at all; the event must
	// still reach handlers registered against interfaces it satisfies.
	var d Dispatcher
	d.Register(func(ctx context.Context, event fooer) error {
		event.Foo()
		return nil
	})

	var calls atomic.Int64
	event := concreteA{onfoo: func() { calls.Add(1) }}
	require.NoError(t, d.Dispatch(context.Background(), event))
	require.Equal(t, int64(1), calls.Load())
}

func TestInterface_RegistrationOrderDoesNotMatter(t *testing.T) {
	// The concrete <-> interface index is built incrementally, so the order
	// registrations arrive in must not change which handlers fire.
	registerIface := func(d *Dispatcher, n *atomic.Int64) {
		d.Register(counter[fooer](n))
	}
	registerConcrete := func(d *Dispatcher, n *atomic.Int64) {
		d.Register(counter[concreteA](n))
	}

	t.Run("interface first", func(t *testing.T) {
		var d Dispatcher
		var iface, concrete atomic.Int64
		registerIface(&d, &iface)
		registerConcrete(&d, &concrete)

		require.NoError(t, d.Dispatch(context.Background(), concreteA{}))
		require.Equal(t, int64(1), iface.Load())
		require.Equal(t, int64(1), concrete.Load())
	})

	t.Run("concrete first", func(t *testing.T) {
		var d Dispatcher
		var iface, concrete atomic.Int64
		registerConcrete(&d, &concrete)
		registerIface(&d, &iface)

		require.NoError(t, d.Dispatch(context.Background(), concreteA{}))
		require.Equal(t, int64(1), iface.Load())
		require.Equal(t, int64(1), concrete.Load())
	})

	t.Run("interface after a dispatch", func(t *testing.T) {
		// the first dispatch materialises the concrete type's entry; a later
		// interface registration must still be back-filled into it.
		var d Dispatcher
		var iface, concrete atomic.Int64
		registerConcrete(&d, &concrete)

		require.NoError(t, d.Dispatch(context.Background(), concreteA{}))
		require.Zero(t, iface.Load())
		require.Equal(t, int64(1), concrete.Load())

		registerIface(&d, &iface)

		require.NoError(t, d.Dispatch(context.Background(), concreteA{}))
		require.Equal(t, int64(1), iface.Load())
		require.Equal(t, int64(2), concrete.Load())
	})
}

func TestInterface_AnyReceivesEveryEvent(t *testing.T) {
	var d Dispatcher
	var calls atomic.Int64
	d.Register(counter[any](&calls))

	require.NoError(t, d.Dispatch(context.Background(), 1))
	require.NoError(t, d.Dispatch(context.Background(), "two"))
	require.NoError(t, d.Dispatch(context.Background(), concreteA{}))
	require.NoError(t, d.Dispatch(context.Background(), concreteC{}))
	require.NoError(t, d.Dispatch(context.Background(), &concreteC{}))

	require.Equal(t, int64(5), calls.Load())
}

func TestInterface_NonImplementersDoNotMatch(t *testing.T) {
	var d Dispatcher
	var foo, never atomic.Int64
	d.Register(counter[fooer](&foo))
	d.Register(counter[noImplementers](&never))

	// concreteC implements neither interface.
	require.NoError(t, d.Dispatch(context.Background(), concreteC{}))
	require.Zero(t, foo.Load())
	require.Zero(t, never.Load())

	// concreteA implements fooer but not noImplementers.
	require.NoError(t, d.Dispatch(context.Background(), concreteA{}))
	require.Equal(t, int64(1), foo.Load())
	require.Zero(t, never.Load())
}

func TestInterface_MultipleInterfacesMatchExactlyOnce(t *testing.T) {
	// concreteB satisfies fooer, barer and any, and also has a handler
	// registered against its own concrete type. Every handler must run, and
	// none of them twice.
	var d Dispatcher
	var r recorder
	var n atomic.Int64

	d.Register(recording[fooer](&r, "fooer", &n))
	d.Register(recording[barer](&r, "barer", &n))
	d.Register(recording[any](&r, "any", &n))
	d.Register(recording[concreteB](&r, "concrete", &n))
	d.Register(recording[noImplementers](&r, "never", &n))

	require.NoError(t, d.Dispatch(context.Background(), concreteB{id: "x"}))

	// handlers run concurrently, so compare as a set.
	require.ElementsMatch(t, []string{"fooer", "barer", "any", "concrete"}, r.calls())
	require.Equal(t, int64(4), n.Load())
}

func TestInterface_MultipleMatchWithoutConcreteRegistration(t *testing.T) {
	// same as above, but nothing ever registers the concrete type, so
	// dispatch takes the "scan the interface index" path instead.
	var d Dispatcher
	var r recorder
	var n atomic.Int64

	d.Register(recording[fooer](&r, "fooer", &n))
	d.Register(recording[barer](&r, "barer", &n))
	d.Register(recording[any](&r, "any", &n))

	require.NoError(t, d.Dispatch(context.Background(), concreteB{}))
	require.ElementsMatch(t, []string{"fooer", "barer", "any"}, r.calls())
	require.Equal(t, int64(3), n.Load())
}

func TestInterface_DispatchThroughInterfaceStaticType(t *testing.T) {
	// The type parameter is inferred as fooer here, but routing is done off
	// the event's dynamic type, so the concreteA handler must fire.
	var d Dispatcher
	var concrete, iface atomic.Int64
	d.Register(counter[concreteA](&concrete))
	d.Register(counter[fooer](&iface))

	var event fooer = concreteA{}
	require.NoError(t, d.Dispatch(context.Background(), event))
	require.Equal(t, int64(1), concrete.Load())
	require.Equal(t, int64(1), iface.Load())
}

func TestInterface_ConcreteHandlersAreNotSharedAcrossImplementers(t *testing.T) {
	var d Dispatcher
	var onlyA, anyFooer atomic.Int64
	d.Register(counter[concreteA](&onlyA))
	d.Register(counter[fooer](&anyFooer))

	// concreteB is a fooer, but it is not a concreteA.
	require.NoError(t, d.Dispatch(context.Background(), concreteB{}))
	require.Zero(t, onlyA.Load())
	require.Equal(t, int64(1), anyFooer.Load())
}

func TestInterface_HandlerReceivesUsableInterfaceValue(t *testing.T) {
	// the reflect call must pass the concrete value into a parameter typed as
	// the interface, and methods on it must work.
	var d Dispatcher
	var seen []string
	d.Register(func(ctx context.Context, event fooer) error {
		seen = append(seen, fmt.Sprintf("%T", event))
		event.Foo()
		return nil
	})

	require.NoError(t, d.Dispatch(context.Background(), concreteA{}))
	require.NoError(t, d.Dispatch(context.Background(), concreteB{}))
	require.Equal(t, []string{"ed.concreteA", "ed.concreteB"}, seen)
}

// ---------------------------------------------------------------------------
// errors
// ---------------------------------------------------------------------------

var errBoom = errors.New("boom")

func TestError_HandlerErrorIsReturned(t *testing.T) {
	var d Dispatcher
	type ev struct{ foo string }

	d.Register(func(ctx context.Context, event ev) error { return nil })
	d.Register(func(ctx context.Context, event ev) error {
		return fmt.Errorf("handler failed: %w", errBoom)
	})

	err := d.Dispatch(context.Background(), ev{foo: "expecting error"})
	require.Error(t, err)
	require.ErrorIs(t, err, errBoom)
}

func TestError_AllHandlersRunEvenWhenOneFails(t *testing.T) {
	var d Dispatcher
	type ev struct{}

	var r recorder
	var n atomic.Int64
	d.Register(func(ctx context.Context, event ev) error {
		n.Add(1)
		r.add("failing")
		return errBoom
	})
	d.Register(recording[ev](&r, "ok1", &n))
	d.Register(recording[ev](&r, "ok2", &n))

	require.ErrorIs(t, d.Dispatch(context.Background(), ev{}), errBoom)
	require.Equal(t, int64(3), n.Load(), "a failing handler must not cancel its siblings")
	require.ElementsMatch(t, []string{"failing", "ok1", "ok2"}, r.calls())
}

func TestError_MultipleFailuresReturnOneOfThem(t *testing.T) {
	var d Dispatcher
	type ev struct{}

	errA := errors.New("a")
	errB := errors.New("b")
	d.Register(func(ctx context.Context, event ev) error { return errA })
	d.Register(func(ctx context.Context, event ev) error { return errB })

	err := d.Dispatch(context.Background(), ev{})
	require.Error(t, err)
	require.True(t, errors.Is(err, errA) || errors.Is(err, errB),
		"expected one of the handler errors, got %v", err)
}

func TestError_SuccessfulDispatchReturnsNil(t *testing.T) {
	var d Dispatcher
	type ev struct{}

	for range 3 {
		d.Register(func(ctx context.Context, event ev) error { return nil })
	}
	require.NoError(t, d.Dispatch(context.Background(), ev{}))
}

func TestError_TypedNilPointerErrorIsStillAnError(t *testing.T) {
	// The reflect plumbing must not "helpfully" flatten a typed nil into a
	// nil error: it should behave exactly like a plain Go call would.
	type ev struct{}
	var d Dispatcher
	d.Register(func(ctx context.Context, event ev) error {
		var e *customError
		return e // non-nil interface holding a nil pointer
	})

	require.Error(t, d.Dispatch(context.Background(), ev{}))
}

type customError struct{ msg string }

func (c *customError) Error() string {
	if c == nil {
		return "<nil *customError>"
	}
	return c.msg
}

func TestError_PropagatesThroughWrappers(t *testing.T) {
	var d Dispatcher
	type ev struct{}

	var sawErr error
	d.Wrap(func(ctx context.Context, event ev, next func(context.Context) error) error {
		sawErr = next(ctx)
		return sawErr
	})
	d.Register(func(ctx context.Context, event ev) error { return errBoom })

	err := d.Dispatch(context.Background(), ev{})
	require.ErrorIs(t, err, errBoom)
	require.ErrorIs(t, sawErr, errBoom, "the wrapper must be able to observe the handler error")
}

// ---------------------------------------------------------------------------
// wrappers
// ---------------------------------------------------------------------------

func TestWrap_NotInvokedWhenNoHandlersMatch(t *testing.T) {
	var d Dispatcher

	var x, y atomic.Int32
	d.Wrap(func(ctx context.Context, event any, next func(context.Context) error) error {
		x.Add(1)
		return next(ctx)
	})
	d.Wrap(func(ctx context.Context, event fooer, next func(context.Context) error) error {
		y.Add(1)
		return next(ctx)
	})

	// no handlers are registered, therefore no wrappers will fire
	require.NoError(t, d.Dispatch(context.Background(), concreteA{}))
	require.Equal(t, int32(0), x.Load())
	require.Equal(t, int32(0), y.Load())

	var z atomic.Int32
	d.Register(func(ctx context.Context, event concreteA) error {
		z.Add(1)
		return nil
	})

	require.NoError(t, d.Dispatch(context.Background(), concreteA{}))
	require.Equal(t, int32(1), x.Load())
	require.Equal(t, int32(1), y.Load())
	require.Equal(t, int32(1), z.Load())
}

func TestWrap_SurroundsHandlers(t *testing.T) {
	var d Dispatcher
	type ev struct{}

	var r recorder
	d.Wrap(func(ctx context.Context, event ev, next func(context.Context) error) error {
		r.add("before")
		err := next(ctx)
		r.add("after")
		return err
	})
	d.Register(func(ctx context.Context, event ev) error {
		r.add("handler")
		return nil
	})

	require.NoError(t, d.Dispatch(context.Background(), ev{}))
	require.Equal(t, []string{"before", "handler", "after"}, r.calls())
}

func TestWrap_NestingOrderCharacterization(t *testing.T) {
	// Characterization test, NOT a contract: the docs state that wrapper
	// nesting order is unspecified and may change between releases. Today,
	// wrappers registered against the same type compose LIFO — the last one
	// registered ends up outermost. This exists so that a change is a
	// deliberate act rather than a silent one; update the expectation freely
	// if the composition changes.
	//
	// Note there is deliberately no equivalent test for wrappers registered
	// against *different* interfaces: that order comes out of a map range and
	// genuinely varies run to run.
	var d Dispatcher
	type ev struct{}

	var r recorder
	for _, name := range []string{"w1", "w2", "w3"} {
		d.Wrap(func(ctx context.Context, event ev, next func(context.Context) error) error {
			r.add("%s:pre", name)
			err := next(ctx)
			r.add("%s:post", name)
			return err
		})
	}
	d.Register(func(ctx context.Context, event ev) error {
		r.add("handler")
		return nil
	})

	require.NoError(t, d.Dispatch(context.Background(), ev{}))
	require.Equal(t, []string{
		"w3:pre", "w2:pre", "w1:pre",
		"handler",
		"w1:post", "w2:post", "w3:post",
	}, r.calls())
}

func TestWrap_RunSeriallyAroundConcurrentHandlers(t *testing.T) {
	// however wrappers nest, they must all be entered before any handler runs
	// and left after every handler has finished.
	var d Dispatcher
	type ev struct{}

	var r recorder
	for range 3 {
		d.Wrap(func(ctx context.Context, event ev, next func(context.Context) error) error {
			r.add("wrapper:enter")
			err := next(ctx)
			r.add("wrapper:exit")
			return err
		})
	}
	for range 4 {
		d.Register(func(ctx context.Context, event ev) error {
			r.add("handler")
			return nil
		})
	}

	require.NoError(t, d.Dispatch(context.Background(), ev{}))

	got := r.calls()
	require.Len(t, got, 3+4+3)
	require.Equal(t, []string{"wrapper:enter", "wrapper:enter", "wrapper:enter"}, got[:3])
	require.Equal(t, []string{"handler", "handler", "handler", "handler"}, got[3:7])
	require.Equal(t, []string{"wrapper:exit", "wrapper:exit", "wrapper:exit"}, got[7:])
}

func TestWrap_ShortCircuit(t *testing.T) {
	t.Run("with error", func(t *testing.T) {
		var d Dispatcher
		type ev struct{}

		var handled atomic.Int64
		d.Wrap(func(ctx context.Context, event ev, next func(context.Context) error) error {
			return errBoom // never calls next
		})
		d.Register(counter[ev](&handled))

		require.ErrorIs(t, d.Dispatch(context.Background(), ev{}), errBoom)
		require.Zero(t, handled.Load())
	})

	t.Run("without error", func(t *testing.T) {
		var d Dispatcher
		type ev struct{}

		var handled atomic.Int64
		d.Wrap(func(ctx context.Context, event ev, next func(context.Context) error) error {
			return nil // swallow the event entirely
		})
		d.Register(counter[ev](&handled))

		require.NoError(t, d.Dispatch(context.Background(), ev{}))
		require.Zero(t, handled.Load())
	})

	t.Run("stops everything nested below it", func(t *testing.T) {
		var d Dispatcher
		type ev struct{}

		var r recorder
		var handled atomic.Int64
		d.Wrap(func(ctx context.Context, event ev, next func(context.Context) error) error {
			r.add("passthrough")
			return next(ctx)
		})
		d.Wrap(func(ctx context.Context, event ev, next func(context.Context) error) error {
			r.add("stops-here")
			return errBoom
		})
		d.Register(counter[ev](&handled))

		// Which of the two ends up outermost is unspecified, so assert the
		// part that is guaranteed: once a wrapper declines to call next,
		// nothing below it runs.
		require.ErrorIs(t, d.Dispatch(context.Background(), ev{}), errBoom)
		require.Zero(t, handled.Load())

		calls := r.calls()
		require.Equal(t, "stops-here", calls[len(calls)-1],
			"the short-circuiting wrapper must be the last thing that ran")
	})
}

func TestWrap_CanReplaceTheContext(t *testing.T) {
	type ctxKey struct{}
	type ev struct{}

	var d Dispatcher
	var got any
	d.Wrap(func(ctx context.Context, event ev, next func(context.Context) error) error {
		return next(context.WithValue(ctx, ctxKey{}, "from wrapper"))
	})
	d.Register(func(ctx context.Context, event ev) error {
		got = ctx.Value(ctxKey{})
		return nil
	})

	require.NoError(t, d.Dispatch(context.Background(), ev{}))
	require.Equal(t, "from wrapper", got)
}

func TestWrap_CanCancelTheContext(t *testing.T) {
	type ev struct{}

	var d Dispatcher
	var handlerErr error
	d.Wrap(func(ctx context.Context, event ev, next func(context.Context) error) error {
		ctx, cancel := context.WithCancel(ctx)
		cancel()
		return next(ctx)
	})
	d.Register(func(ctx context.Context, event ev) error {
		handlerErr = ctx.Err()
		return nil
	})

	require.NoError(t, d.Dispatch(context.Background(), ev{}))
	require.ErrorIs(t, handlerErr, context.Canceled)
}

func TestWrap_ReceivesTheEvent(t *testing.T) {
	var d Dispatcher

	var seenConcrete concreteB
	var seenIfaceType string
	d.Wrap(func(ctx context.Context, event concreteB, next func(context.Context) error) error {
		seenConcrete = event
		return next(ctx)
	})
	d.Wrap(func(ctx context.Context, event fooer, next func(context.Context) error) error {
		seenIfaceType = fmt.Sprintf("%T", event)
		return next(ctx)
	})
	d.Register(func(ctx context.Context, event concreteB) error { return nil })

	require.NoError(t, d.Dispatch(context.Background(), concreteB{id: "payload"}))
	require.Equal(t, concreteB{id: "payload"}, seenConcrete)
	require.Equal(t, "ed.concreteB", seenIfaceType)
}

func TestWrap_InterfaceWrapperAppliesToConcreteHandler(t *testing.T) {
	var d Dispatcher

	var wrapped, handled atomic.Int64
	d.Wrap(func(ctx context.Context, event fooer, next func(context.Context) error) error {
		wrapped.Add(1)
		return next(ctx)
	})
	d.Register(counter[concreteA](&handled))

	require.NoError(t, d.Dispatch(context.Background(), concreteA{}))
	require.Equal(t, int64(1), wrapped.Load())
	require.Equal(t, int64(1), handled.Load())
}

func TestWrap_ConcreteWrapperAppliesToInterfaceHandler(t *testing.T) {
	var d Dispatcher

	var wrapped, handled atomic.Int64
	d.Register(counter[fooer](&handled))
	d.Wrap(func(ctx context.Context, event concreteA, next func(context.Context) error) error {
		wrapped.Add(1)
		return next(ctx)
	})

	require.NoError(t, d.Dispatch(context.Background(), concreteA{}))
	require.Equal(t, int64(1), wrapped.Load())
	require.Equal(t, int64(1), handled.Load())

	// concreteB is a fooer too, but the wrapper is scoped to concreteA.
	require.NoError(t, d.Dispatch(context.Background(), concreteB{}))
	require.Equal(t, int64(1), wrapped.Load())
	require.Equal(t, int64(2), handled.Load())
}

func TestWrap_OnlyMatchingWrappersRun(t *testing.T) {
	var d Dispatcher

	var onA, onC, handled atomic.Int64
	d.Wrap(func(ctx context.Context, event concreteA, next func(context.Context) error) error {
		onA.Add(1)
		return next(ctx)
	})
	d.Wrap(func(ctx context.Context, event concreteC, next func(context.Context) error) error {
		onC.Add(1)
		return next(ctx)
	})
	d.Register(counter[any](&handled))

	require.NoError(t, d.Dispatch(context.Background(), concreteA{}))
	require.Equal(t, int64(1), onA.Load())
	require.Zero(t, onC.Load())
	require.Equal(t, int64(1), handled.Load())

	require.NoError(t, d.Dispatch(context.Background(), concreteC{}))
	require.Equal(t, int64(1), onA.Load())
	require.Equal(t, int64(1), onC.Load())
	require.Equal(t, int64(2), handled.Load())
}

func TestWrap_RegisteredAfterFirstDispatch(t *testing.T) {
	var d Dispatcher
	type ev struct{}

	var wrapped, handled atomic.Int64
	d.Register(counter[ev](&handled))

	require.NoError(t, d.Dispatch(context.Background(), ev{}))
	require.Zero(t, wrapped.Load())
	require.Equal(t, int64(1), handled.Load())

	d.Wrap(func(ctx context.Context, event ev, next func(context.Context) error) error {
		wrapped.Add(1)
		return next(ctx)
	})

	require.NoError(t, d.Dispatch(context.Background(), ev{}))
	require.Equal(t, int64(1), wrapped.Load())
	require.Equal(t, int64(2), handled.Load())
}

func TestWrap_WrapperMayInvokeNextMoreThanOnce(t *testing.T) {
	// nothing forbids it, and retry-style wrappers depend on it.
	var d Dispatcher
	type ev struct{}

	var handled atomic.Int64
	d.Wrap(func(ctx context.Context, event ev, next func(context.Context) error) error {
		if err := next(ctx); err != nil {
			return next(ctx) // retry once
		}
		return nil
	})

	var attempts atomic.Int64
	d.Register(func(ctx context.Context, event ev) error {
		handled.Add(1)
		if attempts.Add(1) == 1 {
			return errBoom
		}
		return nil
	})

	require.NoError(t, d.Dispatch(context.Background(), ev{}))
	require.Equal(t, int64(2), handled.Load())
}

// ---------------------------------------------------------------------------
// concurrency
// ---------------------------------------------------------------------------

func TestConcurrent_HandlersRunInParallel(t *testing.T) {
	// two handlers that can only both complete if they run at the same time.
	var d Dispatcher
	type ev struct{}

	a, b := make(chan struct{}), make(chan struct{})
	rendezvous := func(mine, theirs chan struct{}) Handler[ev] {
		return func(ctx context.Context, event ev) error {
			close(mine)
			select {
			case <-theirs:
				return nil
			case <-time.After(10 * time.Second):
				return errors.New("handlers did not run concurrently")
			}
		}
	}
	d.Register(rendezvous(a, b))
	d.Register(rendezvous(b, a))

	require.NoError(t, d.Dispatch(context.Background(), ev{}))
}

func TestConcurrent_Dispatch(t *testing.T) {
	var d Dispatcher
	type ev struct{ n int }

	var calls atomic.Int64
	d.Register(counter[ev](&calls))
	d.Register(counter[any](&calls))
	d.Wrap(func(ctx context.Context, event any, next func(context.Context) error) error {
		return next(ctx)
	})

	const goroutines, each = 16, 50
	var wg sync.WaitGroup
	errs := make(chan error, goroutines)
	for g := range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range each {
				if err := d.Dispatch(context.Background(), ev{n: g*each + i}); err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}

	require.Equal(t, int64(goroutines*each*2), calls.Load())
}

func TestConcurrent_RegisterWhileDispatching(t *testing.T) {
	// registration takes the write lock while dispatch holds the read lock;
	// interleaving the two must be safe (run this one under -race).
	var d Dispatcher
	type ev struct{}

	var calls atomic.Int64
	var wg sync.WaitGroup

	const registrations = 32
	wg.Add(1)
	go func() {
		defer wg.Done()
		for range registrations {
			d.Register(counter[ev](&calls))
			d.Wrap(func(ctx context.Context, event ev, next func(context.Context) error) error {
				return next(ctx)
			})
		}
	}()

	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 100 {
				assert.NoError(t, d.Dispatch(context.Background(), ev{}))
			}
		}()
	}
	wg.Wait()

	// once everything has settled, every registered handler fires.
	before := calls.Load()
	require.NoError(t, d.Dispatch(context.Background(), ev{}))
	require.Equal(t, int64(registrations), calls.Load()-before)
}

func TestDispatch_NestedDispatchOnSameDispatcher(t *testing.T) {
	// Dispatch only takes a read lock, so a handler may dispatch again on the
	// same Dispatcher. (Registering from inside a handler would deadlock —
	// that is a documented sharp edge, not something exercised here.)
	var d Dispatcher
	type outer struct{}
	type inner struct{}

	var innerCalls atomic.Int64
	d.Register(counter[inner](&innerCalls))
	d.Register(func(ctx context.Context, event outer) error {
		return d.Dispatch(ctx, inner{})
	})

	done := make(chan error, 1)
	go func() { done <- d.Dispatch(context.Background(), outer{}) }()

	select {
	case err := <-done:
		require.NoError(t, err)
		require.Equal(t, int64(1), innerCalls.Load())
	case <-time.After(10 * time.Second):
		t.Fatal("nested dispatch deadlocked")
	}
}

// ---------------------------------------------------------------------------
// package-level API (global dispatcher)
// ---------------------------------------------------------------------------

// globalEvent and friends are declared at package scope so the package-level
// tests do not accidentally share types with any other test.
type globalEvent struct{ payload string }

type globalUnhandledEvent struct{}

type globalIface interface{ isGlobalEvent() }

func (globalEvent) isGlobalEvent() {}

func TestGlobal_RegisterWrapDispatch(t *testing.T) {
	var r recorder
	var handled, wrappedConcrete, wrappedIface atomic.Int64

	Wrap(func(ctx context.Context, event globalEvent, next func(context.Context) error) error {
		wrappedConcrete.Add(1)
		r.add("wrap:concrete")
		return next(ctx)
	})
	Wrap(func(ctx context.Context, event globalIface, next func(context.Context) error) error {
		wrappedIface.Add(1)
		r.add("wrap:iface")
		return next(ctx)
	})
	Register(func(ctx context.Context, event globalEvent) error {
		assert.Equal(t, "hello", event.payload)
		handled.Add(1)
		r.add("handler")
		return nil
	})

	require.NoError(t, Dispatch(context.Background(), globalEvent{payload: "hello"}))
	require.Equal(t, int64(1), handled.Load())
	require.Equal(t, int64(1), wrappedConcrete.Load())
	require.Equal(t, int64(1), wrappedIface.Load())

	calls := r.calls()
	require.Equal(t, "handler", calls[len(calls)-1], "handler runs after every wrapper")
}

func TestGlobal_UnhandledTypeIsNoop(t *testing.T) {
	require.NoError(t, Dispatch(context.Background(), globalUnhandledEvent{}))
}

func TestGlobal_ErrorPropagates(t *testing.T) {
	type globalErrEvent struct{}
	Register(func(ctx context.Context, event globalErrEvent) error { return errBoom })
	require.ErrorIs(t, Dispatch(context.Background(), globalErrEvent{}), errBoom)
}

// ---------------------------------------------------------------------------
// API shape (generic methods)
// ---------------------------------------------------------------------------

func TestAPI_GenericMethodCallForms(t *testing.T) {
	type ev struct{ n int }

	var d Dispatcher
	var calls atomic.Int64

	// 1. inferred from a function literal
	d.Register(func(ctx context.Context, event ev) error { calls.Add(1); return nil })

	// 2. explicitly instantiated
	d.Register[ev](func(ctx context.Context, event ev) error { calls.Add(1); return nil })

	// 3. inferred from a value already typed as Handler[E]
	var h Handler[ev] = func(ctx context.Context, event ev) error { calls.Add(1); return nil }
	d.Register(h)

	// wrappers accept the same three forms
	var wrapped atomic.Int64
	d.Wrap(func(ctx context.Context, event ev, next func(context.Context) error) error {
		wrapped.Add(1)
		return next(ctx)
	})
	d.Wrap[ev](func(ctx context.Context, event ev, next func(context.Context) error) error {
		wrapped.Add(1)
		return next(ctx)
	})
	var w Wrapper[ev] = func(ctx context.Context, event ev, next func(context.Context) error) error {
		wrapped.Add(1)
		return next(ctx)
	}
	d.Wrap(w)

	require.NoError(t, d.Dispatch(context.Background(), ev{n: 1}))
	require.NoError(t, d.Dispatch[ev](context.Background(), ev{n: 2}))

	require.Equal(t, int64(6), calls.Load())
	require.Equal(t, int64(6), wrapped.Load())
}

func TestAPI_GenericMethodValues(t *testing.T) {
	// instantiated generic methods can be taken as values and passed around,
	// which is what replaced the old Using[E](d) helper.
	type ev struct{ n int }

	var d Dispatcher
	register := d.Register[ev]
	dispatch := d.Dispatch[ev]

	var calls atomic.Int64
	register(func(ctx context.Context, event ev) error {
		assert.Equal(t, 3, event.n)
		calls.Add(1)
		return nil
	})

	require.NoError(t, dispatch(context.Background(), ev{n: 3}))
	require.Equal(t, int64(1), calls.Load())
}

// ---------------------------------------------------------------------------
// internals
// ---------------------------------------------------------------------------

func TestTypeFuncs(t *testing.T) {
	var tf typeFuncs

	require.Empty(t, tf.get(fnKindHandler))
	require.Empty(t, tf.get(fnKindWrapper))

	tf.push(fnKindHandler, 1, 2)
	tf.push(fnKindWrapper, 3)
	tf.push(fnKindHandler, 4)

	require.Equal(t, []int{1, 2, 4}, tf.get(fnKindHandler))
	require.Equal(t, []int{3}, tf.get(fnKindWrapper))

	require.Panics(t, func() { tf.get(fnKind(0)) })
	require.Panics(t, func() { tf.push(fnKind(99), 1) })
}

func TestIDSequenceIsUnique(t *testing.T) {
	// every registration must get its own id, otherwise handlers would
	// overwrite one another in Dispatcher.fns.
	var d Dispatcher
	type ev struct{}

	const n = 25
	for range n {
		d.Register(func(ctx context.Context, event ev) error { return nil })
	}

	d.l.RLock()
	defer d.l.RUnlock()
	require.Len(t, d.concrete[reflect.TypeFor[ev]()].handlers, n)
	require.Len(t, d.fns, n)
}
