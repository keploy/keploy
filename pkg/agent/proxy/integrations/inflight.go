package integrations

import "context"

// A test's end count (KEPLOY_PROVEN, design v14 §5.2) waits, in a replay,
// until no accepted connection holds a request the proxy has read and not yet
// answered. The proxy tells most of those apart by itself: a parser that
// answers each request before it reads the next is working on one exactly
// while it is not blocked reading its connection. A parser that reads ahead
// is blocked reading while it still holds a request it read (one goroutine
// reads while another matches, or one read waits on a deadline before the
// requests it gathered are matched): only the parser can say it holds one.
//
// Such a parser marks each request it holds from the moment it has read it
// (HoldRequest) and releases the mark once the request is answered, or its
// miss is filed in-band (models.ReportMockMismatch). A miss it returns as
// MockOutgoing's error is filed by the proxy before the connection is let go,
// and letting the connection go drops every mark it still has: such a request
// is not released by the parser. It says it marks them (RequestHolding): a
// parser that reads ahead (ReadsAhead) and does not say so leaves its
// connection's requests unknown, and a test's end that finds one is not
// verified, never passed.

// RequestHolder keeps the requests a connection's parser holds.
type RequestHolder interface {
	// Hold marks one request read and not yet answered; release, called once
	// (later calls do nothing), unmarks it.
	Hold() (release func())
}

type requestHolderKey struct{}

// WithRequestHolder is ctx carrying h, for the parser of h's connection.
func WithRequestHolder(ctx context.Context, h RequestHolder) context.Context {
	if h == nil {
		return ctx
	}
	return context.WithValue(ctx, requestHolderKey{}, h)
}

// HoldRequest marks a request the parser of ctx's connection has read and
// holds, until release is called. Without the proven switch, or for a
// connection the proxy does not keep, it marks nothing.
func HoldRequest(ctx context.Context) (release func()) {
	if h, ok := ctx.Value(requestHolderKey{}).(RequestHolder); ok && h != nil {
		return h.Hold()
	}
	return func() {}
}

// RequestHolding is the optional capability of a parser that marks every
// request it holds (HoldRequest). The proxy type-asserts the parser it
// serves a connection with, as it does GapResyncCapable.
type RequestHolding interface {
	// HoldsRequests reports that the parser marks every request it reads
	// and has not yet answered or filed while it reads on.
	HoldsRequests() bool
}

// readsAhead are the parsers whose replay can read on while it holds a
// request it read: a reader goroutine apart from the one that answers
// (grpc, http2, kafka), a read on a deadline before the gathered requests
// are matched (generic), or a request held until a later one ends it (Mongo's
// moreToCome). Every other parser answers or files each request before it
// reads again, so the proxy tells by itself when it works on one.
var readsAhead = map[IntegrationType]bool{
	GENERIC:  true,
	GRPC:     true,
	HTTP2:    true,
	MONGO_V2: true,
	"kafka":  true,
}

// ReadsAhead reports whether parser t's replay can read on while it holds a
// request it read.
func ReadsAhead(t IntegrationType) bool { return readsAhead[t] }

// HoldsUnknown reports whether the requests a connection served by parser p,
// of type t, holds cannot be told: t reads ahead and p does not mark them
// (RequestHolding).
func HoldsUnknown(t IntegrationType, p any) bool {
	if !ReadsAhead(t) {
		return false
	}
	h, ok := p.(RequestHolding)
	return !ok || !h.HoldsRequests()
}
