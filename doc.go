// Package ed implements an event dispatcher that is meant to allow codebases
// to organize concerns around events. Instead of placing all concerns about a
// particular business logic event in one function, this package allows you to
// declare events by type and dispatach those events to other code so that other
// concerns may be maintained in separate areas of the codebase.
//
// # Register events
//
// ed uses Go's own type system to dispatch events to the correct event
// handlers.
//
// By simply registering an event handler that accepts an event of a particular
// type:
//
//	ed.Register(func(ctx context.Context, event MyEvent) error {
//	    fmt.Println("neat! got an event!")
//	    return nil
//	})
//
// That handler will be triggered when an event of the same type is dispatched
// from somewhere else in the program:
//
//	err := ed.Dispatch(ctx, MyEvent{Handy: "Info"}) // neat! got an event!
//
// Because it is Go's type system, event handlers can also be registered against
// _interfaces_ instead of just concrete types. Thus, creating an event handler
// that will be triggered for all events is as simple as:
//
//	ed.Register(func(ctx context.Context, event any) error {
//	    fmt.Println("give me all the events!")
//	    return nil
//	})
//	type CreateEvent interface { IsCreatedEvent() }
//	ed.Register(func(ctx context.Context, createEvent CreateEvent) error {
//	    fmt.Println("give me all events about stuff getting created.")
//	    return nil
//	})
package ed
