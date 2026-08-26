package ed_test

import (
	"context"
	"errors"
	"fmt"

	"code.nkcmr.net/ed"
)

type UserSignupEvent struct {
	Email string
}

func (UserSignupEvent) IsUserEvent() {}

type UserEvent interface{ IsUserEvent() }

// Registering a handler for a concrete event type and dispatching that type.
func Example() {
	ed.Register(func(ctx context.Context, event UserSignupEvent) error {
		fmt.Println("welcome,", event.Email)
		return nil
	})

	if err := ed.Dispatch(context.Background(), UserSignupEvent{Email: "gopher@example.com"}); err != nil {
		fmt.Println("dispatch failed:", err)
	}
	// Output:
	// welcome, gopher@example.com
}

// A Dispatcher can be used directly instead of the package-level functions,
// which keeps registrations scoped to that instance.
func ExampleDispatcher() {
	var d ed.Dispatcher

	type OrderPlaced struct{ ID string }
	d.Register(func(ctx context.Context, event OrderPlaced) error {
		fmt.Println("order placed:", event.ID)
		return nil
	})

	_ = d.Dispatch(context.Background(), OrderPlaced{ID: "ord_123"})
	// Output:
	// order placed: ord_123
}

// Handlers may be registered against an interface, in which case any event
// whose type satisfies that interface is routed to them.
func ExampleDispatcher_Register_interface() {
	var d ed.Dispatcher

	d.Register(func(ctx context.Context, event UserEvent) error {
		fmt.Printf("a user event happened: %T\n", event)
		return nil
	})

	_ = d.Dispatch(context.Background(), UserSignupEvent{Email: "gopher@example.com"})
	// Output:
	// a user event happened: ed_test.UserSignupEvent
}

// Wrappers run around every matching handler, which makes them a good fit for
// cross-cutting concerns such as logging, metrics or tracing.
func ExampleDispatcher_Wrap() {
	var d ed.Dispatcher

	type CacheEvicted struct{ Key string }

	d.Wrap(func(ctx context.Context, event CacheEvicted, next func(context.Context) error) error {
		fmt.Println("start:", event.Key)
		err := next(ctx)
		fmt.Println("done:", event.Key, "err:", err)
		return err
	})
	d.Register(func(ctx context.Context, event CacheEvicted) error {
		return errors.New("could not evict")
	})

	fmt.Println("dispatch returned:", d.Dispatch(context.Background(), CacheEvicted{Key: "k1"}))
	// Output:
	// start: k1
	// done: k1 err: could not evict
	// dispatch returned: could not evict
}

// An error returned by any handler is returned from Dispatch, which allows
// events to be used for synchronous validation.
func ExampleDispatcher_Dispatch_error() {
	var d ed.Dispatcher

	type Withdrawal struct{ Cents int }

	d.Register(func(ctx context.Context, event Withdrawal) error {
		if event.Cents > 1000 {
			return errors.New("amount exceeds limit")
		}
		return nil
	})

	fmt.Println(d.Dispatch(context.Background(), Withdrawal{Cents: 500}))
	fmt.Println(d.Dispatch(context.Background(), Withdrawal{Cents: 5000}))
	// Output:
	// <nil>
	// amount exceeds limit
}
