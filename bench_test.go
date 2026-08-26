package ed

import (
	"context"
	"fmt"
	"testing"
)

// sink keeps dispatch results alive so the compiler cannot elide the work.
var sink error

// ---------------------------------------------------------------------------
// fixtures
// ---------------------------------------------------------------------------

type benchEvent struct {
	ID  string
	Seq int
}

type benchFooer interface{ benchFoo() }

func (benchEvent) benchFoo() {}

// bigEvent is deliberately fat, to show what reflect.ValueOf costs when the
// event does not fit in a word.
type bigEvent struct {
	Names  [8]string
	Values [16]int
	Flags  [8]bool
}

func noopHandler(ctx context.Context, event benchEvent) error { return nil }

func passthroughWrapper(ctx context.Context, event benchEvent, next func(context.Context) error) error {
	return next(ctx)
}

// decoy is a generic interface used to stand up N distinct interface types
// that no benchmark event implements. Dispatch has to run an Implements check
// against each one before deciding it does not match.
type decoy[T any] interface{ decoyMethod(T) }

func registerDecoy[T any](d *Dispatcher) {
	d.Register(func(context.Context, decoy[T]) error { return nil })
}

// each entry registers a handler for a distinct interface type.
var decoys = []func(*Dispatcher){
	registerDecoy[[0]int], registerDecoy[[1]int], registerDecoy[[2]int], registerDecoy[[3]int],
	registerDecoy[[4]int], registerDecoy[[5]int], registerDecoy[[6]int], registerDecoy[[7]int],
	registerDecoy[[8]int], registerDecoy[[9]int], registerDecoy[[10]int], registerDecoy[[11]int],
	registerDecoy[[12]int], registerDecoy[[13]int], registerDecoy[[14]int], registerDecoy[[15]int],
}

// benchDispatch runs the standard hot-loop against a dispatcher with a single
// registered handler for E.
func benchDispatch[E any](b *testing.B, event E) {
	b.Helper()

	var d Dispatcher
	d.Register(func(context.Context, E) error { return nil })

	ctx := context.Background()
	b.ReportAllocs()
	for b.Loop() {
		sink = d.Dispatch(ctx, event)
	}
}

// ---------------------------------------------------------------------------
// the floor: what the same work costs without the dispatcher in the way
// ---------------------------------------------------------------------------

// BenchmarkBaseline_DirectCall is the reference point for every Dispatch
// benchmark below: the same handler, called directly. The gap between this and
// BenchmarkDispatch_HandlerCount/handlers=1 is the whole cost of the library
// (map lookup, reflect.Call, and a goroutine per handler).
func BenchmarkBaseline_DirectCall(b *testing.B) {
	ctx := context.Background()
	event := benchEvent{ID: "evt", Seq: 1}

	b.ReportAllocs()
	for b.Loop() {
		sink = noopHandler(ctx, event)
	}
}

// ---------------------------------------------------------------------------
// dispatch: hot paths
// ---------------------------------------------------------------------------

// BenchmarkDispatch_NoHandlers is the cheapest possible dispatch: an event
// type nothing is registered for. This is the cost an application pays for
// dispatching events nobody is listening to yet.
func BenchmarkDispatch_NoHandlers(b *testing.B) {
	var d Dispatcher
	d.Register(func(context.Context, struct{ unrelated int }) error { return nil })

	ctx := context.Background()
	event := benchEvent{ID: "evt"}

	b.ReportAllocs()
	for b.Loop() {
		sink = d.Dispatch(ctx, event)
	}
}

// BenchmarkDispatch_ZeroValueDispatcher is the short-circuit for a Dispatcher
// nothing has ever been registered on.
func BenchmarkDispatch_ZeroValueDispatcher(b *testing.B) {
	var d Dispatcher

	ctx := context.Background()
	event := benchEvent{ID: "evt"}

	b.ReportAllocs()
	for b.Loop() {
		sink = d.Dispatch(ctx, event)
	}
}

// BenchmarkDispatch_HandlerCount shows how dispatch scales with the number of
// matching handlers. Every handler gets its own goroutine via errgroup, with
// no fast path for the single-handler case, so watch the handlers=1 number in
// particular.
func BenchmarkDispatch_HandlerCount(b *testing.B) {
	for _, n := range []int{1, 2, 4, 8, 16, 64} {
		b.Run(fmt.Sprintf("handlers=%d", n), func(b *testing.B) {
			var d Dispatcher
			for range n {
				d.Register(noopHandler)
			}

			ctx := context.Background()
			event := benchEvent{ID: "evt"}

			b.ReportAllocs()
			for b.Loop() {
				sink = d.Dispatch(ctx, event)
			}
		})
	}
}

// BenchmarkDispatch_WrapperCount shows the per-dispatch cost of wrappers. The
// wrapper chain is not cached: Dispatch rebuilds the whole closure onion on
// every call, so this is expected to be linear in the number of wrappers with
// an allocation or two each.
func BenchmarkDispatch_WrapperCount(b *testing.B) {
	for _, n := range []int{0, 1, 2, 4, 8} {
		b.Run(fmt.Sprintf("wrappers=%d", n), func(b *testing.B) {
			var d Dispatcher
			for range n {
				d.Wrap(passthroughWrapper)
			}
			d.Register(noopHandler)

			ctx := context.Background()
			event := benchEvent{ID: "evt"}

			b.ReportAllocs()
			for b.Loop() {
				sink = d.Dispatch(ctx, event)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// dispatch: routing
// ---------------------------------------------------------------------------

// BenchmarkDispatch_Routing contrasts the two routing paths for the same
// event: a direct hit in the concrete index versus the fallback scan over the
// interface index.
func BenchmarkDispatch_Routing(b *testing.B) {
	ctx := context.Background()
	event := benchEvent{ID: "evt"}

	b.Run("concrete-hit", func(b *testing.B) {
		var d Dispatcher
		d.Register(noopHandler)

		b.ReportAllocs()
		for b.Loop() {
			sink = d.Dispatch(ctx, event)
		}
	})

	b.Run("interface-scan", func(b *testing.B) {
		// benchEvent itself is never registered, so each dispatch re-walks
		// the interface index instead of hitting the concrete map.
		var d Dispatcher
		d.Register(func(context.Context, benchFooer) error { return nil })

		b.ReportAllocs()
		for b.Loop() {
			sink = d.Dispatch(ctx, event)
		}
	})
}

// BenchmarkDispatch_InterfaceScanWidth shows how the fallback scan degrades as
// more interfaces are registered. The result is recomputed on every dispatch
// and never memoised (see the "todo, memoize?" note in Dispatch), so an
// application that registers many interface handlers and dispatches types that
// have no concrete registration pays this repeatedly.
func BenchmarkDispatch_InterfaceScanWidth(b *testing.B) {
	for _, total := range []int{1, 2, 4, 8, 16} {
		b.Run(fmt.Sprintf("interfaces=%d", total), func(b *testing.B) {
			// one interface actually matches; the rest are decoys that
			// Dispatch must test and reject on every call.
			var d Dispatcher
			d.Register(func(context.Context, benchFooer) error { return nil })
			for _, register := range decoys[:total-1] {
				register(&d)
			}

			ctx := context.Background()
			event := benchEvent{ID: "evt"}

			b.ReportAllocs()
			for b.Loop() {
				sink = d.Dispatch(ctx, event)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// dispatch: event shape
// ---------------------------------------------------------------------------

// BenchmarkDispatch_EventShape shows what the event type itself costs. Every
// dispatch runs reflect.ValueOf on the event, which boxes anything that does
// not fit in an interface word.
func BenchmarkDispatch_EventShape(b *testing.B) {
	b.Run("empty-struct", func(b *testing.B) { benchDispatch(b, struct{}{}) })
	b.Run("int", func(b *testing.B) { benchDispatch(b, 42) })
	b.Run("small-struct", func(b *testing.B) { benchDispatch(b, benchEvent{ID: "evt", Seq: 1}) })
	b.Run("big-struct", func(b *testing.B) { benchDispatch(b, bigEvent{}) })
	b.Run("pointer-to-big-struct", func(b *testing.B) { benchDispatch(b, &bigEvent{}) })
}

// ---------------------------------------------------------------------------
// dispatch: contention
// ---------------------------------------------------------------------------

// BenchmarkDispatch_Parallel measures dispatch under concurrent load. Dispatch
// holds a read lock for the whole call, so this is where read-lock contention
// across cores would show up.
func BenchmarkDispatch_Parallel(b *testing.B) {
	for _, n := range []int{1, 4} {
		b.Run(fmt.Sprintf("handlers=%d", n), func(b *testing.B) {
			var d Dispatcher
			for range n {
				d.Register(noopHandler)
			}
			event := benchEvent{ID: "evt"}

			b.ReportAllocs()
			b.RunParallel(func(pb *testing.PB) {
				ctx := context.Background()
				for pb.Next() {
					if err := d.Dispatch(ctx, event); err != nil {
						b.Error(err)
						return
					}
				}
			})
		})
	}
}

// ---------------------------------------------------------------------------
// registration (startup cost)
// ---------------------------------------------------------------------------

// BenchmarkRegister measures standing up a dispatcher from scratch. This is
// startup cost, not a hot path, but it is worth watching because binding an
// interface handler walks every concrete type already known to the dispatcher
// and vice versa, which is quadratic in the limit.
func BenchmarkRegister(b *testing.B) {
	b.Run("16-concrete", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			var d Dispatcher
			for range 16 {
				d.Register(noopHandler)
			}
		}
	})

	b.Run("16-interfaces", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			var d Dispatcher
			for _, register := range decoys {
				register(&d)
			}
		}
	})

	b.Run("16-interfaces-then-concrete", func(b *testing.B) {
		// registering the concrete type has to back-fill from every known
		// interface.
		b.ReportAllocs()
		for b.Loop() {
			var d Dispatcher
			for _, register := range decoys {
				register(&d)
			}
			d.Register(noopHandler)
		}
	})
}
