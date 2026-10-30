package integrations

import "context"

// A connection to a local destination is listed mocked under KEPLOY_PROVEN
// (proxy.TopologySink, design v14 §3.1) only where keploy records or mocks
// its calls: one it passes through reaches its server for real, and listing it
// would claim an isolation that did not happen. A parser that passes some of
// a connection's calls through and records or mocks the others (HTTP, by a
// bypassRule's host and path) says which: it calls CallServed for each call it
// records or answers from mocks, never for one it passes through, so a
// connection whose every call was passed through is not listed.

type callServedKey struct{}

// WithCallServed is ctx carrying served, which the parser of ctx's connection
// calls for each of its calls it records or answers from mocks (CallServed).
func WithCallServed(ctx context.Context, served func()) context.Context {
	if served == nil {
		return ctx
	}
	return context.WithValue(ctx, callServedKey{}, served)
}

// CallServed says the parser of ctx's connection records, or answers from
// mocks, one of its calls: it passes it through to no server. With nothing on
// ctx (the connection is listed as it is dispatched, or not at all), it does
// nothing.
func CallServed(ctx context.Context) {
	if served, ok := ctx.Value(callServedKey{}).(func()); ok {
		served()
	}
}
