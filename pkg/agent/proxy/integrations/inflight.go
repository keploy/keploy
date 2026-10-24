package integrations

import (
	"context"

	"go.keploy.io/server/v3/pkg/matcher/learn"
)

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
// holds, until release is called. For a connection the proxy does not keep
// (it keeps them under the proven switch, or for an agent settling reads), it
// marks nothing.
func HoldRequest(ctx context.Context) (release func()) {
	if h, ok := ctx.Value(requestHolderKey{}).(RequestHolder); ok && h != nil {
		return h.Hold()
	}
	return func() {}
}

// UnknownMarker is a connection whose parser can say that, for a while,
// whether it holds a request cannot be told.
type UnknownMarker interface {
	// MarkUnknown marks it so until release, called once (later calls do
	// nothing); protocol names it in what keploy says.
	MarkUnknown(protocol string) (release func())
}

// MarkUnknown says that, until release, whether the parser of ctx's
// connection holds a request cannot be told: a parser that marks its holds
// (RequestHolding) but has a state it cannot mark exactly, such as Mongo's
// moreToCome group, open until a later frame ends it, which may come only in
// the next test. A test's end that finds the mark leaves the test not
// verified, never passed, and does not wait for the mark to go. For a
// connection the proxy does not keep (it keeps them under the proven switch,
// or for an agent settling reads), it marks nothing.
//
// The parser marks before it reads on with the state open, and releases the
// mark before it reads again once the state ends, after the requests the mark
// covered are held (HoldRequest) or answered: in between, its handler is not
// blocked reading, so a test's end waits for it. A mark released while the
// parser is already reading on with those requests unanswered leaves them
// neither marked nor held. The holder WithRequestHolder carries is also the
// marker: a stand-in holder that is not an UnknownMarker drops every mark.
func MarkUnknown(ctx context.Context, protocol string) (release func()) {
	if m, ok := ctx.Value(requestHolderKey{}).(UnknownMarker); ok && m != nil {
		return m.MarkUnknown(protocol)
	}
	return func() {}
}

// IdleMarker is a connection whose parser can say that, for a while, it waits
// out an answer the dependency held idle.
type IdleMarker interface {
	// MarkIdle marks it so until release, called once (later calls do
	// nothing).
	MarkIdle() (release func())
}

// MarkIdle says that, until release, the parser of ctx's connection waits out
// the time the dependency held a request idle before it answered: a long poll
// that has caught up with the recording, whose answer is that nothing came
// (an SQS ReceiveMessage that finds no message, a Kafka Fetch with nothing
// recorded to fetch, waiting out its MaxWait). The wait is the dependency's
// time, not keploy's work, so a test's end does not wait for it: while marked,
// the connection's handler counts as waiting for the application, as one
// blocked reading does. For a connection the proxy does not keep (it keeps
// them under the proven switch, or for an agent settling reads), it marks
// nothing.
//
// The parser marks once it has chosen the answer and consumed what answers
// with it, and releases the mark before it writes the answer: from the release
// to its next read its handler is not blocked reading, so a test's end waits
// for the answer as for any other. The mark covers only the handler: bytes the
// application sent meanwhile, and a request the parser holds (HoldRequest),
// still keep the connection pending, so a parser that holds the request it
// waits on releases that hold for the wait. The holder WithRequestHolder
// carries is also the marker: a stand-in holder that is not an IdleMarker
// drops every mark.
//
// A mock.real relay marks the same once the application has ended its side
// and all of it has gone to the real server (realdeps.Relay.AwaitsAnswer):
// what is left is that server's answer, the dependency's time, not keploy's
// work.
func MarkIdle(ctx context.Context) (release func()) {
	if m, ok := ctx.Value(requestHolderKey{}).(IdleMarker); ok && m != nil {
		return m.MarkIdle()
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

// FlightCarrier is a connection whose requests are told to the learn step
// (learn.Flight), under the proven switch: InFlight is its request aloft
// now, nil with none.
type FlightCarrier interface {
	InFlight() *learn.Flight
}

// InFlightOf is the request aloft now on the connection ctx's parser serves
// (WithRequestHolder, FlightCarrier); nil with none, or none told.
func InFlightOf(ctx context.Context) *learn.Flight {
	if c, ok := ctx.Value(requestHolderKey{}).(FlightCarrier); ok && c != nil {
		return c.InFlight()
	}
	return nil
}

// ServeTeller is an optional MockMemDb capability: ServingFor is the store
// whose consume doors tell the learn step that what they serve is served in
// the request f (learn.Tracker.ServedMockIn). For one call's consumes: not
// for sharing between goroutines, nor for keeping as the parser's store (a
// wrapper is a new store identity). The agent's MockManager and its round
// scopes implement it.
type ServeTeller interface {
	ServingFor(f *learn.Flight) MockMemDb
}

// ServingIn is db for one consume of a parser serving its connection's
// request (ctx is the parser's: WithRequestHolder): the store that tells the
// learn step the serve is that request's, so another request aloft on a
// parallel connection that reads no recording (a driver's heartbeat, a
// health check) never makes it one keploy cannot tell (learn.Flight). db
// itself where ctx carries no request aloft, or db does not tell
// (ServeTeller): its serves are the one request aloft's, as before.
func ServingIn(ctx context.Context, db MockMemDb) MockMemDb {
	f := InFlightOf(ctx)
	if f == nil {
		return db
	}
	if s, ok := db.(ServeTeller); ok {
		return s.ServingFor(f)
	}
	return db
}
