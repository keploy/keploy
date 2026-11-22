package agent

import "context"

type callerKey struct{}

// WithCaller carries, in a control request's context, how to read the
// process that made the request (in the agent's PID namespace) from its
// connection. It is read only when it is needed: in a run of more than one
// test worker, where a test's begin and end belong to the worker whose
// process sent them.
func WithCaller(ctx context.Context, read func() (int, error)) context.Context {
	return context.WithValue(ctx, callerKey{}, read)
}

// CallerOf is the reader WithCaller put in ctx; nil when none was.
func CallerOf(ctx context.Context) func() (int, error) {
	read, _ := ctx.Value(callerKey{}).(func() (int, error))
	return read
}
