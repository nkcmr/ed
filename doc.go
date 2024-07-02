// Package ed implements an event dispatcher that is meant to allow codebases
// to organize concerns around events. Instead of placing all concerns about a
// particular business logic event in one function, this package allows you to
// declare events by type and dispatach those events to other code so that other
// concerns may be maintained in separate areas of the codebase.
//
// # Stability
//
// This module is tagged as v0, thus complies with Go's definition and rules
// about v0 modules (https://go.dev/doc/modules/version-numbers#v0-number). In
// short, it means that the API of this module may change without incrementing
// the major version number. Each releasable version will simply increment the
// patch number.
//
// Given the surface area of this module is quite small, this should not be a
// huge issue if used in production code.
//
// # Register/Dispatch events
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
// interfaces instead of just concrete types. Thus, creating an event handler
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
//
// # Goals
//
// Overall, this package's goal is to mostly be an aid in allowing application
// business logic to remain clear of ancillary conerns/activities that might
// otherwise pile up in certain code areas.
//
// For example, as a SaaS company grows, a lot of cross-cutting concerns can build
// up around a customer signing up for a service. In a monolithic
// codebase/service, the code that handles a new customer signup would become
// full of "do this check", "send this data to marketing systems", "screen this
// signup for abusive behavior" logic.
//
// This is where ed is meant to be: taking all of those concerns/side-effects
// that are ancillary to the core act of signing up a user, and placing them in
// there own code areas and allowing them to be communicated with via events.
//
// # Non-goals
//
// This package is not trying to be a job queue, or a messaging system. The
// dispatching of events is done synchronously and the events are designed to
// allow errors to be returned so that use-cases like synchronous validation are
// supported.
//
// That being said, a common use-case might be to use this package to allow the
// publishing of events/messages to a Kafka/SQS-like system, an event handler:
//
//	ed.Register(func(ctx context.Context, event UserSignupEvent) error {
//	    return kafka.Send(ctx, KafkaMessage{Topic: "send_welcome_email", Body: json.MustEncode(event)})
//	})
//
// Then in your main business logic for user signups:
//
//	func UserSignupEndpoint(ctx context.Context, request UserSignupRequest) (UserSignupResponse, error) {
//	    // check email not in use
//	    // check password strength
//	    err := db.SaveUser(...)
//	    if err != nil { /* ... */ }
//	    if err := ed.Dispatch(ctx, UserSignupEvent{/* ... */}); err != nil {
//	        return UserSignupResponse{}, err
//	    }
//	    // The welcome email was sent!
//	    // ...
//	}
package ed
