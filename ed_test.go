package ed

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWrapper(t *testing.T) {
	var d Dispatcher

	var x, y atomic.Int32
	Using[any](&d).Wrap(func(ctx context.Context, event any, next func(context.Context) error) error {
		x.Add(1)
		return next(ctx)
	})
	type fooer interface{ Foo() }
	Using[fooer](&d).Wrap(func(ctx context.Context, event fooer, next func(context.Context) error) error {
		y.Add(1)
		return next(ctx)
	})

	// no handlers are registered, therefore no wrappers will fire
	concreateAEv := Using[concreteA](&d)
	err := concreateAEv.Dispatch(context.Background(), concreteA{})
	require.NoError(t, err)
	require.Equal(t, int32(0), x.Load())
	require.Equal(t, int32(0), y.Load())

	var z atomic.Int32
	// runtime.Breakpoint()
	concreateAEv.Register(func(ctx context.Context, event concreteA) error {
		z.Add(1)
		return nil
	})

	err = concreateAEv.Dispatch(context.Background(), concreteA{})
	require.NoError(t, err)
	require.Equal(t, int32(1), x.Load())
	require.Equal(t, int32(1), y.Load())
	require.Equal(t, int32(1), z.Load())

}

type concreteA struct {
	onfoo func()
}

func (c concreteA) Foo() {
	if c.onfoo != nil {
		c.onfoo()
	}
}

func TestNoConcrete(t *testing.T) {
	// this test is ensuring that even if no event handlers are registered
	// for a non-interface type (concrete type), emitting a concrete type will
	// still match event handlers that are registered against interfaces.
	var d Dispatcher
	type fooer interface{ Foo() }
	Using[fooer](&d).Register(func(ctx context.Context, event fooer) error {
		event.Foo()
		return nil
	})

	var x atomic.Int32
	event := concreteA{onfoo: func() { x.Add(1) }}
	err := Using[concreteA](&d).Dispatch(context.Background(), event)
	require.NoError(t, err)
	require.Equal(t, int32(1), x.Load())
}

func TestError(t *testing.T) {
	var d Dispatcher
	type simpleEvent struct {
		foo string
	}
	ev := Using[simpleEvent](&d)
	ev.Register(func(ctx context.Context, event simpleEvent) error {
		return nil
	})
	ev.Register(func(ctx context.Context, event simpleEvent) error {
		return fmt.Errorf("bad!")
	})

	err := ev.Dispatch(context.Background(), simpleEvent{foo: "expecting error"})
	require.Error(t, err)
}

func TestSimple(t *testing.T) {
	var d Dispatcher
	var x atomic.Int64

	type simpleEvent struct {
		foo string
	}

	ev := Using[simpleEvent](&d)
	ev.Register(func(ctx context.Context, event simpleEvent) error {
		assert.Equal(t, simpleEvent{
			foo: "cool! an event!",
		}, event)
		x.Add(1)
		return nil
	})

	err := ev.Dispatch(context.Background(), simpleEvent{foo: "cool! an event!"})
	require.NoError(t, err)
	require.Equal(t, int64(1), x.Load())
	// Using[int](&d).Register(func(ctx context.Context, event int) error {
	// 	t.Logf("concrete type handler triggered")
	// 	y.Add(1)
	// 	return nil
	// })
	// type noImplementers interface{ noOneShouldImplementThis() }
	// Using[noImplementers](&d).Register(func(ctx context.Context, event noImplementers) error {
	// 	fail.Store(true)
	// 	return nil
	// })

	// err := Using[int](&d).Emit(context.Background(), -1)
	// require.NoError(t, err)
	// require.False(t, fail.Load())
	// require.Equal(t, int64(1), x.Load())
	// require.Equal(t, int64(1), y.Load())
}
