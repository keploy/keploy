// Package integrations provides functionality for integrating different types of services.
package integrations

import (
	"context"
	"errors"
	"net"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/variables"
	"go.uber.org/zap"
)

// ErrParserDeclined is returned by a parser that MATCHED a connection but
// then decided not to handle it — a port outside its allowlist, a kill
// switch, an unsupported sub-protocol.
//
// It exists because "return nil" cannot express that. The dispatcher reads
// a nil error as "the parser handled this connection", logs success, and
// falls through to handleConnection's deferred close — so a parser that
// bailed politely had the user's connection torn down under it, which is
// an I1 violation (keploy must never be what ends a connection). Parsers
// documented that returning nil would "pass through"; it did not.
//
// Wrap it (fmt.Errorf("...: %w", integrations.ErrParserDeclined)) to carry
// the reason.
//
// CONTRACT, and both clauses are load-bearing:
//
//  1. If the parser READ from a connection to make its decision, it must
//     restore those bytes on the RecordSession conn it read from —
//     RecordSession.Ingress and/or RecordSession.Egress. The dispatcher
//     relays the SESSION's conns, not handleConnection's own, precisely so
//     a consumed protocol preamble is not lost. Relaying a drained stream
//     makes the peer reset the connection, which moves the user's break
//     rather than removing it.
//
//     Note this is integrations.RecordSession, NOT supervisor.Session.
//     supervisor.Session also has an Ingress field, but it is nil on the
//     V2 path and ignored — bytes restored there vanish silently.
//
//     A parser that upgraded the connection to TLS before declining must
//     not rely on this: util.ConnTLSUpgrader writes the upgraded conn back
//     through handleConnection's pointers, not into RecordSession, so the
//     session's Egress is the pre-upgrade socket.
//
//  2. The parser must not leave goroutines reading those conns. The relay
//     becomes the sole reader; a surviving parser reader races it for
//     bytes on the same socket.
//
// SCOPE: honoured on every legacy RECORD dispatch site — the
// matched-parser branch, the generic catch-all, and the MySQL probe
// branch. On the V2/supervisor path the sentinel is not consulted, but a
// parser error there already becomes FallthroughToPassthrough with a
// correctly activity-scoped orphan window, which is the better shape.
//
// Be aware that those legacy sites are no longer reachable for any parser
// in tree. Every parser the dispatcher can route to record implements
// IntegrationsV2, and the KEPLOY_NEW_RELAY knob that could force them onto
// the legacy path has been removed — so in practice a decline is only
// honoured for an out-of-tree parser that does not implement
// IntegrationsV2, or for the not-registered guards at the MySQL and
// generic sites. A V2 parser that returns this sentinel does NOT get the
// relay-and-restore treatment above; it degrades to
// FallthroughToPassthrough, which keeps user traffic alive but logs the
// connection as a retired parser rather than a decline.
//
// On replay there is no upstream connection to relay to — every
// destination dial in handleConnection is mode-gated — so a parser that
// needs to decline during replay is not yet supported and must not rely
// on this.
var ErrParserDeclined = errors.New("parser declined to handle this connection")

type Initializer func(logger *zap.Logger) Integrations

type IntegrationType string

// constants for different types of integrations
const (
	HTTP        IntegrationType = "http"
	HTTP2       IntegrationType = "http2"
	GRPC        IntegrationType = "grpc"
	GENERIC     IntegrationType = "generic"
	MYSQL       IntegrationType = "mysql"
	POSTGRES_V1 IntegrationType = "postgres_v1"
	POSTGRES_V2 IntegrationType = "postgres_v2"
	POSTGRES_V3 IntegrationType = "postgres_v3"
	MONGO_V1    IntegrationType = "mongo_v1"
	MONGO_V2    IntegrationType = "mongo_v2"
	AEROSPIKE   IntegrationType = "aerospike"
)

type Parsers struct {
	Initializer Initializer
	Priority    int
}

var Registered = make(map[IntegrationType]*Parsers)

type Integrations interface {
	MatchType(ctx context.Context, reqBuf []byte) bool
	RecordOutgoing(ctx context.Context, session *RecordSession) error
	MockOutgoing(ctx context.Context, src net.Conn, dstCfg *models.ConditionalDstCfg, mockDb MockMemDb, opts models.OutgoingOptions) error
}

// IntegrationsV2 is the optional capability interface implemented by
// parsers that have been migrated to the supervisor + relay + FakeConn
// architecture (see PLAN.md at the repository root).
//
// The dispatcher in handleConnection performs a type assertion against
// this interface. Parsers that satisfy it and return true from IsV2()
// are run under the supervisor with a supervisor.Session attached via
// RecordSession.V2; their RecordOutgoing must consume V2.ClientStream,
// V2.DestStream, and V2.Directives rather than the legacy Ingress /
// Egress / TLSUpgrader fields.
//
// Parsers that do not satisfy this interface continue on the legacy
// path unchanged; migration is parser-by-parser and additive.
type IntegrationsV2 interface {
	Integrations
	// IsV2 reports that this parser consumes RecordSession.V2 and is
	// safe to run under supervisor.Run. A parser may return false
	// dynamically (e.g. if a required config is missing) to opt out
	// on a per-instance basis.
	IsV2() bool
}

// GapResyncCapable is the optional capability interface implemented by
// parsers that can recover a byte stream after a HOLE in capture — bytes
// the proxy observed on the wire, forwarded to the real peer, but never
// delivered to the parser.
//
// Optional by design, in the same style as agent.WindowedProxy (pkg/agent/service.go): the
// relay type-asserts for it and falls back to the conservative answer when
// the assertion fails, so no existing parser breaks and third-party parsers
// keep compiling.
//
// # Why the relay needs to ask
//
// Every V2 parser frames a length-prefixed byte stream (mongo, mysql,
// postgres all read a header, then that many bytes). A hole therefore does
// not cost one message: the next header is read from the middle of a body,
// so every subsequent frame on the connection is garbage. That garbage is
// not inert — a Postgres framer once read four bytes of misread row data as
// a uint32 length and tried to allocate multiple gigabytes. So once the tee
// has permanently desynced, continuing to feed the parser produces no mocks
// and can produce a pathological allocation.
//
// Returning true asserts that the parser DETECTS the hole (the relay stamps
// a monotonic fakeconn.Chunk.SeqNo per direction, and a dropped chunk
// still consumes its ordinal, so a gap is visible as a SeqNo discontinuity)
// and re-anchors on a real message boundary before framing resumes. mongo/v2
// is the in-tree example: resyncReassemblyOnGap → beginResync →
// resyncToNextHeader. Such a parser MUST keep receiving bytes after the
// hole — that is the only way it can find the next boundary — so the relay
// keeps feeding it.
//
// Returning false, or not implementing this at all, means the parser has no
// recovery path. The relay then stops feeding that direction once it has
// desynced. Forwarding to the real peer is unaffected either way: this
// changes only what is CAPTURED, never what the application sees.
type GapResyncCapable interface {
	// CanResyncAfterGap reports whether this parser re-anchors its framer
	// after a hole in the delivered byte stream. It is consulted once per
	// connection, before the relay starts, so it must not depend on
	// per-connection state.
	CanResyncAfterGap() bool
}

// EndAtHoleCapable is the optional capability interface of a parser that
// cannot re-align after a hole (GapResyncCapable not implemented, or answered
// no) and reads each direction of its connection to that direction's own end.
// Optional, like GapResyncCapable: the relay type-asserts for it.
//
// Such a parser is fed nothing of a direction after its hole, but without
// this it is not told where the hole is: the direction is not ended, so the
// parser waits for bytes that will not come. A parser that runs behind its
// capture cannot place the hole from the incomplete-mock flag either: the
// relay sets it when the chunk is lost, and the parser takes it when it reads
// its next chunk, which can be a full queue of chunks teed before the hole
// later. Everything the relay still delivers was teed before the hole.
//
// Returning true asks the relay to end a direction at its hole: once every
// chunk teed before it is delivered, the direction's FakeConn returns io.EOF,
// and the session says the stream ended at a hole, and why
// (supervisor.Session.EndedAtHole), so the parser can tell it from the end of
// the connection. The relay then marks no mock incomplete for the lost chunk,
// nor for the chunks it refuses after it: a mark voids whichever mock is
// emitted next, which is one from before the hole. The other direction is fed
// as before, until the connection ends.
//
// It asserts that the parser reads each direction to its own end, and tells a
// hole's end from the connection's (supervisor.Session.EndedAtHole): one that
// does not would record what the hole cut as though the connection had closed
// there. Mid-connection, only a parser that claims it sees one direction end
// while the other goes on. Whenever the parser returns, at the end of both
// directions or before, the relay goes on forwarding the application's bytes
// until a peer closes: a parser's return never ends it.
//
// keploy/integrations' HTTP/2 recorder implements it, and asserts so at
// compile time, so renaming the interface or its method breaks that build
// rather than silently leaving the recorder waiting at its holes.
type EndAtHoleCapable interface {
	// CanEndAtHole reports whether this parser wants a direction that lost a
	// chunk ended at the hole (see EndAtHoleCapable). It is consulted once
	// per connection, before the relay starts, so it must not depend on
	// per-connection state. A parser that can re-align after a hole keeps
	// its feed whatever this answers.
	CanEndAtHole() bool
}

func Register(name IntegrationType, p *Parsers) {
	Registered[name] = p
}

// MockMemDb is the runtime mock pool contract consumed by every parser's
// matcher. It is intentionally a narrow interface over the in-memory
// pool structure owned by the agent's MockManager (see
// pkg/agent/proxy/mockmanager.go).
//
// The surface is split into four focused facets — MockReader,
// MockWriter, MockConsumer, WindowAware — and MockMemDb itself is
// the composite of those four. The composite shape is unchanged from
// prior releases so every existing implementation (the agent's
// MockManager, test doubles like match_test.go's mockMemDb) continues
// to satisfy it without edits. Parsers that only read mocks can now
// type their argument as MockReader; matchers that only need the
// window accessors can take WindowAware; and so on. This is
// opt-in — the Integrations.MockOutgoing signature still passes the
// composite MockMemDb, so migrating a parser to a narrower facet is
// a local, mechanical change.
//
// Unification plan (see docs/explanation/mock-lifetimes.md):
//
//   - The primary, lifetime-aware methods are GetSessionMocks,
//     GetPerTestMocksInWindow, and GetConnectionMocks(connID). Parsers
//     should consume THESE going forward.
//
//   - The historical methods (GetFilteredMocks, GetUnFilteredMocks,
//     GetFilteredMocksInWindow) remain as forwarding aliases during the
//     per-parser migration. They will be removed in Phase 4 once every
//     in-tree parser migrates. Third-party parsers consuming MockMemDb
//     will see a deprecation window of at least one release.
//
// Lifetime semantics:
//
// Parsers should NOT assume a single universal "most specific first"
// ranking — the right order is protocol-dependent. Common pattern in
// the in-tree matchers (HTTP, MySQL):
//
//   - Start with per-test mocks via GetPerTestMocksInWindow. They are
//     scoped to the current test, are the most specific candidate,
//     and are consumed on match via DeleteFilteredMock.
//   - Merge in session mocks from GetSessionMocks. Session-scoped
//     traffic covers handshake, auth, heartbeats, gRPC reflection,
//     Kafka coordination, Redis HELLO/AUTH, etc. Reusable across
//     tests; not consumed on match.
//   - Consult connection-scoped mocks via GetConnectionMocks(connID)
//     for protocol state tied to a live connection (e.g. prepared-
//     statement PREPARE/EXECUTE pairing). These never window-filter
//     and are bounded by the connection's own lifecycle.
//
// Protocols with state ordering requirements (e.g. MySQL's EXEC after
// PREPARE) may need to consult the connection pool ahead of per-test
// — follow the existing matcher's shape when in doubt.
type MockMemDb interface {
	MockReader
	MockWriter
	MockConsumer
	WindowAware
}

// MockCursor is an optional MockMemDb capability that serves stateful
// dependencies: repeated identical requests answered with successive recorded
// responses in record order, then the last one again. MockCursorIndex returns
// the position to serve for a request key given n recorded responses WITHOUT
// advancing (a negative index means "no cursor": serve as if the capability
// were absent); AdvanceMockCursor moves the key past servedIdx, saturating at
// n, and is called only once the response is actually served, so a failed or
// retried match does not skip a recording. The agent's MockManager implements
// it, and every MockMemDb wrapper must forward it — a wrapper that drops it
// silently turns stateful replay off for the calls it wraps. A key need not
// name a request: the HTTP variable pass keeps a served mark per recording in
// the same store (n = 2), under keys no request's cursor uses.
//
// A peek and the commit after it must come from the same goroutine, with no
// other peek of the same store in between: a wrapper may resolve per peek what
// the commit then reuses (the proxy's per-connection views do). Parsers serve
// one connection from one goroutine, one request at a time, which satisfies it.
type MockCursor interface {
	MockCursorIndex(key string, n int) int
	AdvanceMockCursor(key string, servedIdx, n int)
}

// SequenceViewer is an optional MockMemDb capability, under exact serving: the
// stateful cursor of the view in force now, resolved once, and where the open
// test's sequences start (SequenceView). A parser that walks a reusable
// request's recordings reads one view per call (SequenceViewOf) and both
// peeks and commits through its Cursor, so a test opened or ended between the
// peek and the commit moves the sequence the call peeked, never the next
// view's. False with exact serving off: the parser then keeps MockCursor. The
// agent's MockManager implements it, and so do the proxy's connection views
// where they pass through to it (under exact serving); a worker's own scope's
// view does not: its sequences are its scope's own, resolved per peek and
// reused by the commit (MockCursor), and a worker's test is not the view's
// open test. A store held to one call (RoundHolder) gives the view the call
// was read in (CallRound.Sequence).
type SequenceViewer interface {
	SequenceView() (SequenceView, bool)
}

// SequenceViewOf returns db's sequence view (SequenceViewer): false for a
// store with none, and with exact serving off.
func SequenceViewOf(db MockMemDb) (SequenceView, bool) {
	if s, ok := db.(SequenceViewer); ok {
		return s.SequenceView()
	}
	return SequenceView{}, false
}

// SequenceView is, under exact serving, one view's stateful cursor and the
// open test's recorded window (SequenceViewer).
type SequenceView struct {
	// Cursor is the view's stateful cursor (MockCursor), its keys and served
	// marks resolved once: between tests the pool slice's, in a test the
	// test's own, empty when the test opens (a retry too). Once the test
	// ends it is kept for the calls read in the test: one walked after the
	// end reads and moves the test's sequence where the test's calls left
	// it, never the next view's, until the next set is staged.
	Cursor MockCursor
	// Test is the open test, "" between tests.
	Test string
	// Start and End are the open test's recorded window, the request and
	// response times of its recording: zero between tests, and for a test
	// the replay sent no window of (a kind whose test case has no request
	// time).
	Start, End time.Time
	// Worker reports, in a test, whether a data-plane recording is one of
	// the set's worker-scope mocks the view shows (models.Mapping.Worker):
	// nil between tests, and in a view that shows none. A worker-scope
	// recording belongs to no test (record files one there when no test's
	// window was open, at its place in the gap it was made in, and an idle
	// answer wherever it was recorded), so its request's sequence is walked
	// on Between over every reached recording of it, by placement, with no
	// window: one sequence for the set wherever its calls land.
	Worker func(*models.Mock) bool
	// Between is, with Worker, the stateful cursor the calls between tests
	// advance (the last pool view's), which a worker-scope call in a test
	// reads and advances too.
	Between MockCursor
}

// Span returns the recordings, of n of one reusable request in record order
// (at(i) the i-th's recorded request time), that the view's sequence walks,
// as [lo, hi). In a test with a recorded window: those the window holds, so
// the test is answered its own recordings whichever tests ran before it (a
// test's answers never depend on the tests a selective replay or the kept
// set drops); with none in it, the one recorded last before the window (the
// answer in force when the test began), else the first. Between tests, and in
// a test with no window, all of them. The walk answers the first of them not
// served in the view, then the last again.
func (v SequenceView) Span(n int, at func(i int) time.Time) (lo, hi int) {
	if n <= 0 {
		return 0, 0
	}
	if v.Test == "" || v.Start.IsZero() {
		return 0, n
	}
	lo = n
	for i := 0; i < n; i++ {
		if !at(i).Before(v.Start) {
			lo = i
			break
		}
	}
	hi = lo
	for hi < n && (v.End.IsZero() || !at(hi).After(v.End)) {
		hi++
	}
	if hi > lo {
		return lo, hi
	}
	if lo > 0 {
		return lo - 1, lo
	}
	return 0, 1
}

// StatementCatalog is an optional MockMemDb capability: the stored set's
// statement catalog, the first recorded mock of each statement
// (models.StatementKey) among the per-test mocks the agent keeps on disk, by
// kind, in recorded order. The agent seeds it once, when the set's mocks are
// stored. A client may prepare at app start a statement it runs only inside
// tests, whose mocks the startup staging does not load: a parser answers that
// prepare (a PostgreSQL Describe's row shape) from the catalog when no mock it
// may serve holds the statement. The catalog is describe-only: a parser never
// serves one of its mocks as a call's answer, nor consumes it, and never
// changes one (the mocks are shared). The agent's MockManager implements it,
// and every MockMemDb wrapper must forward it: a wrapper that drops it leaves
// a prepare at app start without the statement's shape.
type StatementCatalog interface {
	StatementCatalog(kind models.Kind) []*models.Mock
}

// VariableStore is an optional MockMemDb capability: the table of the test
// variables the replay has bound (package variables), which a matcher reads
// to follow a mock's declared variable places under the proven switch. The
// agent's MockManager implements it, and every MockMemDb wrapper must forward
// it: a wrapper that drops it leaves the calls it wraps unable to match any
// mock that declares a variable place.
type VariableStore interface {
	Variables() *variables.Table
}

// ExactConsumedReader is an optional MockMemDb extension of a store that
// serves each test its own mocks exactly (design v14 §5.2, under the proven
// switch): each test mock answers at most one request, so once it answered
// it is in no read. ConsumedTestMocks returns the open test's mocks that have
// answered, in the order they did (none while no test is open), so a matcher
// that finds no mock for a call can tell a call the test made once more than
// it recorded (models.MissExtraCall, with MockMismatchReport.Recorded) from
// one it never recorded. A matcher must only read them: they answer nothing.
type ExactConsumedReader interface {
	ConsumedTestMocks() []*models.Mock
}

// ExactServedReader is an optional MockMemDb extension of a store that
// serves each test its own mocks exactly (the proven switch), with a served
// observer (the learn tracker's ServedMock). ServedInOpenTest returns, of one
// read of the open test's view, its stamp and its account:
//
//   - The stamp is the learn tracker's open test (Tracker.OpenEpoch) as the
//     view was installed. It is 0 while no test is open, with no tracker,
//     when the tracker had no test open as the view was installed, and in a
//     tracker test two tests' views were installed in: in each, the tracker
//     began no test for this view alone, and 0 is no test of the tracker's.
//   - The account is every mock a parser was served while the view was
//     installed, of any lifetime and through any door (the test's own, a
//     protocol-phase or reusable one, one spent through a delete door, a
//     worker-scope one), once each by learn.MockID, in the order they came;
//     none while no test is open, and none with no observer. A consume the
//     store refused, in its view (a mock it does not show, one that answered
//     already) or in the pool the view passed it to (a claim that failed: the
//     door said false), is no serve: neither this account nor the tracker
//     holds it, and it makes its request no serve in the tracker's order. The
//     test's own mocks the pool view answered before the test's view was
//     installed (in flight when it began: InFlightIntoOpenTest) are in
//     ConsumedTestMocks and not here: they were served before the test
//     opened.
//
// A learn step takes the tracker's Epoch as the call it learns of is
// matched, before it is answered, and reads this and the tracker's Test as
// it decides. When the stamp is not 0 and equals both epochs, the call, the
// store's open test and the tracker's running test are one test: the view
// was installed after the tracker began that test, and no test began and no
// set started from the match to the reads. The store loads the view a serve
// is filed in before it tells the observer of the serve, and files it after,
// so every mock the account holds is then among the tracker's served in its
// running test. Otherwise the reads are of no one test (a test's end and the
// next one's begin, or a new set, landed after the match, or the store's
// test is not the tracker's) and the step learns nothing from them. A
// matcher must only read them: they answer nothing.
type ExactServedReader interface {
	ServedInOpenTest() (uint64, []*models.Mock)
}

// AnsweredFinder is an optional MockMemDb extension: of the per-test
// recordings of kind that answered a call earlier in the test set staged now,
// indexed under any of keys (models.RequestKeys), the one answered last that
// match accepts and that the open test does not own. A parser that finds no
// mock for a call asks it whether the call repeats one an earlier test made
// (models.RepeatNote): the cue of an application that kept the earlier
// answer for a span of elapsed time the replay passed. For a miss's
// diagnosis only: the recording is never served. Every MockMemDb wrapper
// must forward it.
type AnsweredFinder interface {
	FindAnsweredEarlier(kind models.Kind, keys []string, match func(*models.Mock) bool) (AnsweredCall, bool)
}

// AnsweredCall is what AnsweredFinder found: the recording, and when it
// answered at this replay.
type AnsweredCall struct {
	Mock       *models.Mock
	AnsweredAt time.Time
}

// StagedFinder is an optional MockMemDb extension under exact serving (the
// proven switch): the name of a staged mock of kind that the open view does
// not show and no call of the set consumed, for which match reports true: a
// recording another test owns, or a worker-scope one not reached yet; "" for
// none. A parser names it on a miss (MockMismatchReport.NotOwned): the call
// is that recording, made where it was not recorded. For a miss's diagnosis
// only: the mock is never handed out, nor served.
//
// The lookup is asked of the round the call was made in (CallRound), which
// the parser marks (MarkCallRound) as the call's first read of the store,
// before any matcher reads it: a recording that read's view held as its own
// is never named (the replay ended the call's test under it, and the call
// is that test's, not one record's judging moves to the worker scope), and
// none is once the replay staged another set (restaged): a recording of the
// next set named for the call would be moved there, and its one miss spent
// on a call it never made. The staging is checked last, right before the
// store spends a name, and again before it returns one. A lookup asked of
// the zero CallRound names nothing.
type StagedFinder interface {
	MarkCallRound() CallRound
	FindStagedNotOwned(kind models.Kind, call CallRound, match func(*models.Mock) bool) (name string, restaged bool)
}

// StagedKeyFinder is StagedFinder that also looks, when what is staged holds
// no recording match accepts, in the recordings of kind the store keeps on
// disk indexed under any of keys (models.RequestKeys): a test's own are
// staged only as it opens, so a call made before its test would find none
// there. Only those keys' recordings are read. A parser asks by keys every
// recording its comparison would accept is indexed under at least one of.
// For a miss's diagnosis only.
type StagedKeyFinder interface {
	StagedFinder
	FindStagedNotOwnedByKeys(kind models.Kind, call CallRound, keys []string, match func(*models.Mock) bool) (name string, restaged bool)
}

// CallRound is where a call's first read of the store found it made: the
// staging it was in (WindowPacer.StagingEpoch) and the recordings, by name,
// that read's view held as its own (the open test's, in flight into it,
// shared with it, a pool view's ahead). A store makes it in O(1), keeping
// what it needs to tell them later (StagedFinder.MarkCallRound); a round
// snapshot is one too (RoundSnapshot.CallRound). It also says whether that
// view was one the replay serves while it stages a set's mocks (Staging),
// for the call's miss to say so (StampMark); whether it was a test's view
// (InTest), for the miss to be filed where its call was made; and whether
// its set was served by proven routing (Background), for a parser to say of
// the call's miss what that view's set learned. Each is the view the read
// found, never the one serving when the miss is reported: the replay can end
// the call's test, or stage the next set, before then. A store that reads
// its round in one snapshot (RoundReader) also keeps the round's generation
// and boundary count the read was made at (Round, read under the same lock
// as the epoch and the view), for a parser that marks a call before its
// learn step's first read of the round to tell whether the round moved
// between the two (MovedSince). The zero CallRound is of no read.
type CallRound struct {
	read       bool
	staging    bool
	inTest     bool
	background bool
	epoch      uint64
	// gen and bounds are the round's generation and boundary count the
	// read was made at (RoundAt.Gen, RoundAt.Bounds), atRead whether the
	// store that made the read keeps them.
	gen, bounds uint64
	atRead      bool
	held        func(name string) bool
	// seq is the sequence view of the view the read was made in
	// (WithSequence), seqOK whether the store keeps one.
	seq   SequenceView
	seqOK bool
	// worker is whether the read's view showed a test the worker scope
	// (WithSequence's Worker, InWorkerView): ShowsBackgroundIn.
	worker bool
}

// NewCallRound is a store's CallRound of staging epoch, held telling, by
// name, the recordings its view held as its own (nil: none).
func NewCallRound(epoch uint64, held func(name string) bool) CallRound {
	return CallRound{read: true, epoch: epoch, held: held}
}

// Read reports whether c is of a read (not the zero CallRound).
func (c CallRound) Read() bool { return c.read }

// Epoch is the staging c was read in.
func (c CallRound) Epoch() uint64 { return c.epoch }

// Held reports whether the read's view held the recording name as its own.
func (c CallRound) Held(name string) bool { return c.held != nil && c.held(name) }

// WithSequence is c, read in the view whose sequence view is sv
// (SequenceViewer): a store held to c's call (RoundHolder) walks the
// call's reusable recordings in it, the view the call was read in, never one
// installed after the read.
func (c CallRound) WithSequence(sv SequenceView) CallRound {
	c.seq, c.seqOK = sv, true
	c.worker = c.worker || sv.Worker != nil
	return c
}

// InWorkerView is c, of a read whose view showed a test the worker scope
// when shows is true: a test's view with a worker scope, under any rules. A
// store that reads its round in one snapshot (RoundSnapshot.CallRound),
// which keeps no sequence view, says so here, so a call decided on such a
// read (a call that waited for the store to open its test) is held to the
// worker scope's rule as one marked by MarkCallRound is (ShowsBackgroundIn).
func (c CallRound) InWorkerView(shows bool) CallRound {
	c.worker = c.worker || shows
	return c
}

// ShowsWorker reports whether c's read found a test's view that showed the
// worker scope (WithSequence, InWorkerView).
func (c CallRound) ShowsWorker() bool { return c.worker }

// Sequence is the sequence view of the view c was read in (WithSequence):
// false for a store that keeps none, and for a read made with exact serving
// off.
func (c CallRound) Sequence() (SequenceView, bool) { return c.seq, c.seqOK }

// ReadAt is c, of a read made at at: its staging (at.Epoch), generation and
// boundary count (at.Gen, at.Bounds). A store that reads its round in one
// snapshot (RoundReader) sets it as it marks the call, all read under the
// round lock with the view the mark describes, so they are of one instant.
func (c CallRound) ReadAt(at RoundAt) CallRound {
	c.epoch, c.gen, c.bounds, c.atRead = at.Epoch, at.Gen, at.Bounds, true
	return c
}

// Round is the instant of the round c's read was made at (its Epoch, Gen and
// Bounds; Taken is not kept); known is false for a read of a store that
// keeps no generation (a CallRound made by NewCallRound alone).
func (c CallRound) Round() (at RoundAt, known bool) {
	return RoundAt{Epoch: c.epoch, Gen: c.gen, Bounds: c.bounds}, c.atRead
}

// MovedSince reports whether the round moved between the read c is of and
// at, a later read of the round (RoundSnapshot.At): the replay staged a set
// (another epoch), or a boundary of the round landed (another boundary
// count, RoundAt.Bounds: a view's install, a boundary of the learn tracker:
// a test's begin or end on it, a new set). No consume, spend or claim moves
// it, nor a test process's claim of its key again
// (MockManager.RestartExactKey), which gives back what its key spent and
// moves the generation alone: the call is still in the round it was marked
// in, and its first read decides it from what that round shows then.
//
// A parser that marks a call before any matcher reads the store, and
// decides it with a learn step whose first read of the round comes after,
// asks this of that first read: a call marked in a test whose end (a pool
// view's install, the tracker's end of it), or end and the next test's
// begin, landed between the mark and the first read was made in a round no
// read shows any more. Decided in the round the first read shows, its mark
// holds the ended test's recordings as its own while the read's view offers
// the pool's, or the next test's in-flight recordings: the call is paired
// with one of them and what it differs in learned onto it as noise, a false
// green. Moved, it is the moved miss, learning nothing. A mark whose store
// keeps no generation (Round's known false) is moved for every at: nothing
// tells the round did not move under it (fail closed). The zero CallRound,
// of no read, is never moved: the later read is the call's first. The agent's
// MockManager gives it only where exact serving is off, where no round is
// read; a call marked so, before exact serving began, is decided by its
// first read alone (the agent's settle answers every call made before a test
// opens).
//
// Stated limits, each failing closed (the moved miss learns nothing), the
// same class as a take a boundary refuses under a learn step:
//   - A call marked where the learn tracker had begun a test and the store
//     still showed its pool view (RoundSnapshot.PoolsCall) is the pool's,
//     decided at once by a first read made before the store installs that
//     test's view, and the moved miss for a first read made after it: which
//     of the two depends on where the install lands. HTTP's learn step,
//     whose first read is its mark, reads the pool's call so too: a take
//     the install lands under is refused, and its next pass is the moved
//     miss. keploy mock's scope flow settles once, before the tracker
//     begins the test, and installs its view after with no second settle,
//     so any call marked between the begin and the install meets it there;
//     keploy test only a call that arrives after its settle returns and
//     before the install.
//   - A call marked where the tracker had begun a test whose view the store
//     had not installed, over the view of the test before
//     (RoundSnapshot.TrackerAhead), is moved by the store's install of that
//     test too, where a wait for that install from the mark could have
//     decided the call in that test's round. keploy test's flow never
//     produces the state (it begins no test over the last test's view).
func (c CallRound) MovedSince(at RoundAt) bool {
	if !c.read {
		return false
	}
	return c.epoch != at.Epoch || !c.atRead || c.bounds != at.Bounds
}

// InStaging is c, of a read whose view the replay served while it staged a
// set's mocks (a staging view: no recording answers a call there) when
// staging is true. A store sets it as it marks the call, from the view it
// read.
func (c CallRound) InStaging(staging bool) CallRound {
	c.staging = staging
	return c
}

// Staging reports whether c's read found the replay staging a set's mocks.
func (c CallRound) Staging() bool { return c.staging }

// InView is c, of a read whose view was a test's (an open test's view, not a
// pool view between tests) when inTest is true, and whose set was served by
// proven routing (S12Background) when background is true. A store sets it as
// it marks the call, from the view it read, with InStaging.
func (c CallRound) InView(inTest, background bool) CallRound {
	c.inTest, c.background = inTest, background
	return c
}

// InTest reports whether c's read found a test's view: a miss of the call
// was made in that test (models.MissInTest), wherever the replay was when
// the miss is reported.
func (c CallRound) InTest() bool { return c.inTest }

// Background reports whether c's read found its set served by proven routing
// (S12Background of the store then).
func (c CallRound) Background() bool { return c.background }

// S12BackgroundIn is S12Background as the call marked call found it: the
// view its first read found (CallRound.Background), never the one serving
// when the parser asks, which can be the next set's. A parser that says of a
// call's miss whether its set was served by proven routing asks this, not
// S12Background(db), once the call is marked. For the zero CallRound (a
// store that marks no round), db's now.
func S12BackgroundIn(call CallRound, db MockMemDb) bool {
	if call.Read() {
		return call.Background()
	}
	return S12Background(db)
}

// StampMark writes onto r, the miss report of the call marked call, where
// that call was when it first read the round (models.MockMismatchReport.Mark):
// the proxy says from it whether the miss was made while the set's mocks were
// being staged, and whether in a test or outside every test
// (models.MockMismatchReport.MarkedInTest). A miss is decided where its call
// was marked, and the view serving when the report reaches the proxy can be
// the next staging's, the staged set's after the call's staging, or the pool
// view after the call's test ended. The staging it was marked in too
// (models.MockMismatchReport.MarkEpoch): a miss marked before a test set's
// staging is not that set's, whenever its report arrives. Nothing for the
// zero CallRound (a store that marks no round): the proxy reads the view
// serving then.
func StampMark(r *models.MockMismatchReport, call CallRound) {
	if r == nil || !call.Read() {
		return
	}
	r.Mark = models.RoundMarkedSettled
	if call.Staging() {
		r.Mark = models.RoundMarkedStaging
	}
	r.MarkedInTest = call.InTest()
	r.MarkEpoch = call.Epoch()
}

// MarkCallRound is mockDb's CallRound now (StagedFinder.MarkCallRound): the
// zero CallRound from a store that names no recording made elsewhere.
func MarkCallRound(mockDb MockMemDb) CallRound {
	if f, ok := mockDb.(StagedFinder); ok {
		return f.MarkCallRound()
	}
	return CallRound{}
}

// RoundHolder is an optional MockMemDb capability under exact serving: the
// store held to the staging a call's first read was made in (call, from
// MarkCallRound). Once the replay staged a test set after that read, every
// read of held finds nothing and every consume door of it is refused, so a
// matcher that reads the store after the mark never answers the call with a
// recording of the next set, nor names one as its closest: the call misses,
// and restaged then reports true, for the miss to say why. A read is checked
// after it is made (a staging moves its epoch before it changes what a read
// shows), a consume in the step that settles it. held is one call's, as a
// ServingIn store is: not for sharing between goroutines, nor for keeping as
// the parser's store. The agent's MockManager and its connection views
// implement it.
type RoundHolder interface {
	HoldToRound(call CallRound) (held MockMemDb, restaged func() bool)
}

// HoldToRound is db held to call (RoundHolder): the store a parser's matcher
// reads for the call it marked, and its miss's diagnosis reads too. db
// itself, never restaged, for the zero CallRound (a store that marks no
// round). A call db marked but cannot hold (db is no RoundHolder: a wrapper
// that forwards StagedFinder and not RoundHolder) fails closed: the store
// returned reads no recording and refuses every consume and write, so the
// call is never answered with, nor diagnosed against, a recording of a set
// the replay staged after its mark, and its miss says why (UnheldWhy).
func HoldToRound(db MockMemDb, call CallRound) (MockMemDb, func() bool) {
	if !call.Read() {
		return db, notRestaged
	}
	if h, ok := db.(RoundHolder); ok {
		return h.HoldToRound(call)
	}
	return unheldRound{window: db}, notRestaged
}

func notRestaged() bool { return false }

// UnheldNote is what a miss says when its call's store marked the call's
// round but could not hold the call to it (HoldToRound's fail-closed store).
const UnheldNote = "keploy answered this call with no recording and names none made elsewhere for it: its mock store marks the test set a call is made in but cannot hold the call to that set (the store does not implement integrations.RoundHolder), and a call is never answered from, nor diagnosed against, a store the replay could stage another test set into while it is decided."

// UnheldTeller is an optional capability of a store HoldToRound returned:
// why it reads nothing for its call, non-empty when it could not hold the
// call to its round (HoldToRound's fail-closed store, or a view over one,
// which forwards it).
type UnheldTeller interface {
	Unheld() string
}

// UnheldWhy is why db, a store HoldToRound returned, reads nothing for its
// call when it could not hold the call to its round (UnheldNote); "" for a
// store that holds or marks no round.
func UnheldWhy(db MockMemDb) string {
	if u, ok := db.(UnheldTeller); ok {
		return u.Unheld()
	}
	return ""
}

// unheldRound is HoldToRound's fail-closed store, for a call a store marked
// but cannot hold: every read finds no recording, every consume and write
// door is refused, and no optional capability of the store is offered (a
// lookup of a recording made elsewhere included). Only the store's test
// window state, which names no recording, is read through.
type unheldRound struct{ window WindowAware }

func (unheldRound) Unheld() string { return UnheldNote }

func (unheldRound) GetFilteredMocks() ([]*models.Mock, error)                 { return nil, nil }
func (unheldRound) GetUnFilteredMocks() ([]*models.Mock, error)               { return nil, nil }
func (unheldRound) GetMySQLCounts() (total, config, data int)                 { return 0, 0, 0 }
func (unheldRound) GetFilteredMocksInWindow() ([]*models.Mock, error)         { return nil, nil }
func (unheldRound) GetPerTestMocksInWindow() ([]*models.Mock, error)          { return nil, nil }
func (unheldRound) GetSessionMocks() ([]*models.Mock, error)                  { return nil, nil }
func (unheldRound) GetStartupMocks() ([]*models.Mock, error)                  { return nil, nil }
func (unheldRound) GetStartupMocksByKind(models.Kind) ([]*models.Mock, error) { return nil, nil }
func (unheldRound) GetSessionScopedMocks() ([]*models.Mock, error)            { return nil, nil }
func (unheldRound) GetConnectionMocks(string) ([]*models.Mock, error)         { return nil, nil }
func (unheldRound) SessionMockHitCounts() map[string]uint64                   { return nil }
func (unheldRound) UpdateUnFilteredMock(*models.Mock, *models.Mock) bool      { return false }
func (unheldRound) DeleteFilteredMock(models.Mock) bool                       { return false }
func (unheldRound) DeleteUnFilteredMock(models.Mock) bool                     { return false }
func (unheldRound) DeleteStartupMock(models.Mock) bool                        { return false }
func (unheldRound) MarkMockAsUsed(models.Mock) bool                           { return false }

func (u unheldRound) SetCurrentTestWindow(start, end time.Time) {
	u.window.SetCurrentTestWindow(start, end)
}
func (u unheldRound) IsTestWindowActive() bool                  { return u.window.IsTestWindowActive() }
func (u unheldRound) HasFirstTestFired() bool                   { return u.window.HasFirstTestFired() }
func (u unheldRound) FirstTestWindowStart() time.Time           { return u.window.FirstTestWindowStart() }
func (u unheldRound) WindowSnapshot() models.WindowSnapshot     { return u.window.WindowSnapshot() }
func (u unheldRound) CurrentTestWindow() (time.Time, time.Time) { return u.window.CurrentTestWindow() }

var (
	_ MockMemDb    = unheldRound{}
	_ UnheldTeller = unheldRound{}
)

// BegunOwnerReader is an optional MockMemDb extension under exact serving:
// whether a staged mock (one StagedFinder names) is owned by a test that has
// begun in the current staging. A parser whose answer depends on what a test
// caused (an SQS poll, whose message a test sent) reads it to tell a call made
// after that test, which is the recording made later than recorded, from one
// made before it, which rightly finds nothing.
type BegunOwnerReader interface {
	StagedOwnerBegun(name string) bool
}

// StagedReader is an optional MockMemDb capability: every mock staged in the
// store (its per-test, session and start-up tiers), whatever the open view
// shows and whether or not it was served. For the learn step's earlier places
// only (design v14 §7.2 step 6: a value the recording held before a call is
// one the call's value may have flowed from, served or not): such a mock is
// never handed out, nor served.
type StagedReader interface {
	StagedMocks() []*models.Mock
}

// RecordedBeforeReader is an optional MockMemDb capability: it visits every
// recording of the staged test set made before t (by request time), and
// those made at t's own instant (a fixture recorded to the whole second may
// have made one before t: the caller tells them apart, as the paired
// recording is one), of any kind, whether the store holds it in its trees or keeps it on disk (a test's
// own recordings are staged only as that test opens), served or not, each
// once, until visit returns false. A recording kept on disk is decoded for
// visit alone and kept by no one: visit must not keep m (or any part of it a
// cache would key by m) past its return, so that no reader holds every
// recording at once. For the learn step's earlier places of a recording that
// is the pool's, which may carry a value any recording made before it held:
// nothing it visits is handed out, nor served. err is why the store cannot
// read them all; the learn step then refuses, as it cannot tell where a value
// came from.
type RecordedBeforeReader interface {
	RangeRecordedBefore(t time.Time, visit func(m *models.Mock) bool) error
}

// StagedRecordingReader is an optional MockMemDb capability: it visits every
// recording of kind of the staged test set, those the store holds in its
// trees and those of kind it keeps on disk indexed under any of keys
// (models.RequestKeys; a test's own are staged only as that test opens),
// served or not and whatever the open view shows, each once, until visit
// returns false. A recording kept on disk is decoded for visit alone: visit
// keeps at most the one it stops at. For the learn step alone: a call that
// is exactly a recording its scope does not offer (another test's, the
// previous test's own after that test ended) is that recording's call, never
// a drift of the recording the learn step would pair it with. Nothing it
// visits is handed out, nor served. err is why the store cannot read them
// all; the learn step then learns nothing from the call, as it cannot tell.
type StagedRecordingReader interface {
	RangeStagedRecordings(kind models.Kind, keys []string, visit func(m *models.Mock) bool) error
}

// LearnEnder is an optional MockMemDb capability: whether the test set's
// learn step has ended (models.MockFilterParams.LearnEnded). keploy replay's
// mock-consistency retry cycles run the passing test cases again, test-1
// after test-N, so recorded order is not replay order in them: a late call of
// one test read in another's window would be paired with that test's own
// recording. Those cycles are judged, and learn nothing.
type LearnEnder interface {
	LearnEnded() bool
}

// LearnEnded reports whether db says the test set's learn step has ended
// (LearnEnder): a parser's learn step then decides no request, and the
// request is served as a replay serves it. false for a store that does not
// tell, which keeps learning as before.
func LearnEnded(db MockMemDb) bool {
	e, ok := db.(LearnEnder)
	return ok && e.LearnEnded()
}

// RecordedTests is an optional TestWindows capability: where a recording's
// time lies among the staging's recorded test windows. For the learn step's
// reasons and its check that a call is exactly a recording its scope does not
// offer (StagedRecordingReader).
type RecordedTests interface {
	// RecordedTestAt is the test case whose recorded window t is inside, ""
	// where t is inside none, the replay sent none, or the window carries no
	// name.
	RecordedTestAt(t time.Time) string
	// BeforeTestWindow reports whether t is before the start of the
	// recorded window of the test the view in force serves; known is false
	// where no such window is known or t is zero.
	BeforeTestWindow(t time.Time) (before, known bool)
}

// TestWindows is an optional MockMemDb capability: where the request time of
// a recorded mock falls among the replay's test windows. The learn step tells
// the open test's own mocks, the pool's and other tests' apart by it where
// the store's tiers cannot (a reusable HTTP mock of any test is in the
// session tier).
type TestWindows interface {
	// InTestWindow reports whether t is inside the window of the test the
	// replay runs now.
	InTestWindow(t time.Time) bool
	// InRecordedTestWindow reports whether t is inside the window of some
	// recorded test of the set; known is false when the replay sent none.
	InRecordedTestWindow(t time.Time) (inside, known bool)
	// SameRecordedTestWindow reports whether a and b are inside one recorded
	// test's window; known is false when the replay sent none.
	SameRecordedTestWindow(a, b time.Time) (same, known bool)
}

// RecordedWindowIndex is an optional TestWindows capability: where a time
// lies among the set's recorded test windows, by their starts. A matcher
// tells by it which gap between two recorded tests a recording made outside
// every test lies in, and which gaps a call made with a test's window open
// may fall in (the one before that window, or the one after it).
type RecordedWindowIndex interface {
	// RecordedWindowIndex is the index, in start order, of the last
	// recorded test window that starts at or before t, -1 when t precedes
	// every window (the app's start-up) or is zero. A time inside no window
	// lies in the gap after that window. known is false when the replay
	// sent none.
	RecordedWindowIndex(t time.Time) (index int, known bool)
}

// VariableScopes is an optional MockMemDb capability, beside VariableStore:
// the scope a call binds test variables in, by the mock that answers it. Under
// exact serving, between tests, the next test's calls in flight the pool view
// answers (InAheadScope) bind in that test's scope, begun early
// (AheadVariables); every other call, in the store's (VariableStore). A
// matcher reads both scopes before it reads the pool, as it reads one.
type VariableScopes interface {
	AheadVariables() *variables.Table
	InAheadScope(m *models.Mock) bool
}

// ExactBackground is exact serving's background, which a
// parser answers from after the open test's own: the worker scope the run
// reached (a no-sibling recording from the set's start) and the pool.
// An optional MockMemDb capability, under the proven switch: the agent's
// MockManager implements it, and every MockMemDb wrapper must forward it.
type ExactBackground interface {
	// Background is kind's background recordings the view shows unused, in
	// the view's order: the worker scope's by place (the set's start, a
	// no-sibling recording among them, then each test's in the order the
	// tests began), within one place a delivering answer before an idle one,
	// then recorded order; then the pool's, in recorded order. A parser takes
	// the first that answers, without re-sorting, and consumes it through
	// its usual delete door.
	Background(kind models.Kind) []*models.Mock
	// SpentBackground is kind's background recordings this staging consumed,
	// read-only, in recorded order: what a by-kind reuse answers with.
	SpentBackground(kind models.Kind) []*models.Mock
}

// ByKindBackground is an optional MockMemDb capability under the background
// rules: a kind's whole background as answering by kind reads it. By-kind
// answers ignore Place. So whether a kind may be answered
// by kind (every background reply of the kind equal) is decided over all of
// the kind's background, and a by-kind answer may spend a recording placed
// after a test the run has not begun yet: neither depends on which places
// the run has reached when the call lands. Background, which exact serving
// reads, keeps identical siblings gated by their places, and its consume
// doors refuse one the view hides. The agent's MockManager implements it,
// and every MockMemDb wrapper must forward it.
type ByKindBackground interface {
	// KindBackground is kind's background recordings (data-plane, no async
	// lane's), the worker scope at every place and the pool: those unused,
	// and those this staging spent, each in recorded order. nil, nil for a
	// set served by today's rules.
	KindBackground(kind models.Kind) (unused, spent []*models.Mock)
	// SpendByKind spends m, one of KindBackground's unused, for the staging:
	// a by-kind answer's consumption, whatever m's place. False when m is
	// none (spent already, no background recording, or today's rules); the
	// parser then answers nothing with it.
	SpendByKind(m *models.Mock) bool
}

// KindBackground is db's ByKindBackground.KindBackground: nil, nil when db
// does not implement it.
func KindBackground(db MockMemDb, kind models.Kind) (unused, spent []*models.Mock) {
	if b, ok := db.(ByKindBackground); ok {
		return b.KindBackground(kind)
	}
	return nil, nil
}

// SpendByKind is db's ByKindBackground.SpendByKind: false when db does not
// implement it.
func SpendByKind(db MockMemDb, m *models.Mock) bool {
	if b, ok := db.(ByKindBackground); ok {
		return b.SpendByKind(m)
	}
	return false
}

// BackgroundCursor is an optional MockMemDb capability under the background
// rules (the pool keeps its recorded reuse and cursor-saturate,
// its cursor shared with background callers): the stateful cursor of the
// staged set's background, its worker scope and its pool, the one its calls
// between tests advance. Inside a test the store's own MockCursor keeps no
// cursor, as a test's own recordings answer once each, in recorded order; a
// call inside a test that a background recording answers reads and advances
// this one instead, so the background's recorded sequence, and each of its
// recordings' served or delivered mark, is one whether its calls land inside
// a test or between tests. ok is false for a set served by today's rules: a
// parser then uses MockCursor as before. A test's own recordings and the
// background are never one sequence: a parser picks among the open test's
// own first. The agent's MockManager implements it, and every MockMemDb
// wrapper must forward it.
type BackgroundCursor interface {
	BackgroundCursor() (MockCursor, bool)
}

// BackgroundCursorOf is db's BackgroundCursor: false when db does not
// implement it, or serves its set by today's rules.
func BackgroundCursorOf(db MockMemDb) (MockCursor, bool) {
	if b, ok := db.(BackgroundCursor); ok {
		return b.BackgroundCursor()
	}
	return nil, false
}

// S12Background reports whether db serves its staged set by the background
// rules (BackgroundCursorOf's ok): false for a set served by
// today's rules, and for a store that implements none of them.
func S12Background(db MockMemDb) bool {
	_, ok := BackgroundCursorOf(db)
	return ok
}

// Pairing is record's full round's pairing. It answers only
// while the session pairs: OutgoingOptions.Learn under the proven switch.
// An optional MockMemDb capability: the agent's MockManager implements it,
// and every MockMemDb wrapper must forward it.
type Pairing interface {
	// PairingCandidates is kind's data-plane recordings other tests own that
	// their owners have not consumed and no pairing has claimed, in recorded
	// order; nil when the session does not pair.
	PairingCandidates(kind models.Kind) []*models.Mock
	// ClaimPairing claims m for one pairing in this staging. It is false
	// when another call claimed it, its owner consumed it, or the session
	// does not pair. A claim is no consumption: m stays its owner's.
	ClaimPairing(m *models.Mock) bool
}

// Background is db's background recordings of kind (ExactBackground); nil
// when db does not implement it.
func Background(db MockMemDb, kind models.Kind) []*models.Mock {
	if b, ok := db.(ExactBackground); ok {
		return b.Background(kind)
	}
	return nil
}

// SpentBackground is the background recordings of kind db's staging consumed
// (ExactBackground); nil when db does not implement it.
func SpentBackground(db MockMemDb, kind models.Kind) []*models.Mock {
	if b, ok := db.(ExactBackground); ok {
		return b.SpentBackground(kind)
	}
	return nil
}

// PairingCandidates is db's pairing candidates of kind (Pairing); nil when db
// does not implement it.
func PairingCandidates(db MockMemDb, kind models.Kind) []*models.Mock {
	if p, ok := db.(Pairing); ok {
		return p.PairingCandidates(kind)
	}
	return nil
}

// ClaimPairing claims m for one pairing in db's staging (Pairing); false when
// db does not implement it.
func ClaimPairing(db MockMemDb, m *models.Mock) bool {
	if p, ok := db.(Pairing); ok {
		return p.ClaimPairing(m)
	}
	return false
}

// InFlightOwner is an optional MockMemDb capability: whether a mock was in
// flight when the test the replay runs now began, sent in the gap before that
// test's recorded window and answered inside it (models.SentInGapAnsweredBy),
// which the store serves as that test's own (GetPerTestMocksInWindow). Its
// request was recorded outside every recorded test's window, and it is still
// no start-up or between-tests call of the pool's: the learn step counts it
// the open test's (TestWindows alone, by its request time, cannot).
type InFlightOwner interface {
	InFlightIntoOpenTest(m *models.Mock) bool
}

// InFlightIntoOpenTest reports whether db says m was in flight when the open
// test began (InFlightOwner); false for a db that cannot tell.
func InFlightIntoOpenTest(db MockMemDb, m *models.Mock) bool {
	o, ok := db.(InFlightOwner)
	return ok && o.InFlightIntoOpenTest(m)
}

// PoolMembership is an optional MockMemDb capability: whether a mock is the
// pool's by the replay's scope table (keploy mock replay under the proven
// switch: a mock its recording made while no test was open, which the table
// puts in a pool slice, not in a test's mapping). keploy mock replay sends no
// recorded test windows (TestWindows does not know), and its mappings hold
// each test's begin but not its end, so the table is the store's only word on
// whose a mock is there. known is false until the replay sent the table.
type PoolMembership interface {
	InPool(m *models.Mock) (pool, known bool)
}

// InPool reports whether db says m is the pool's by the replay's scope table
// (PoolMembership); known is false for a db that cannot tell.
func InPool(db MockMemDb, m *models.Mock) (pool, known bool) {
	p, ok := db.(PoolMembership)
	if !ok {
		return false, false
	}
	return p.InPool(m)
}

// TestOwnership is an optional MockMemDb capability: whether a mock is one of
// the open test's own, which its mapping lists, rather than the pool's. A view
// that scopes the pool to a test (`keploy mock replay`'s) knows; known is
// false when no test is open in it, and a store that is no such view does
// not implement it: there a test's own mocks are its per-test tier. The learn
// step pairs the open test's own mocks, and the pool's only in a full record
// (design v14 §7.2 step 10).
type TestOwnership interface {
	OwnedByTest(m *models.Mock) (owned, known bool)
}

// SessionWindowReader is an optional MockMemDb extension: the GetSessionMocks
// snapshot narrowed to the mocks recorded inside one test window, without
// walking the rest of it.
//
// A replay re-stages every test's window but leaves the reusable pool whole,
// and lax mode keeps a recording's per-test MySQL data mocks in that pool, so
// it holds the traffic of every test in the set. A matcher that only wants the
// current test's mocks would otherwise walk all of them on every command, and a
// replay's total cost would grow with the square of its length.
type SessionWindowReader interface {
	// GetSessionMocksInWindow returns, in GetSessionMocks order, the mocks of
	// that snapshot whose ReqTimestampMock lies in [start, end] (bounds
	// included) or is unset: exactly GetSessionMocks filtered by that
	// predicate.
	GetSessionMocksInWindow(start, end time.Time) ([]*models.Mock, error)
}

// MockIndex files mocks under the keys Keys gives them, so a matcher can find
// its candidates in a large pool without walking it (see SessionKeyReader).
//
// Keys must depend only on what a mock recorded and how replay classified it
// (its Lifetime and its "type" metadata), never on its place in a pool: a
// store builds one index per MockIndex value and pool, and keeps it while the
// pool lasts. So pass the same *MockIndex to every lookup, and declare it
// once, at package level.
type MockIndex struct {
	Keys func(*models.Mock) []string
	// OwnedKeys, when set, gives the open test's own mocks their keys under
	// exact serving, in place of Keys: the mocks a SessionKeyReader's walk
	// reads ahead of the staged ones. Under exact serving the per-test tier
	// (GetPerTestMocksInWindow) leads with every one of those mocks, in that
	// walk's order. A matcher that walks the per-test tier whole before each
	// keyed walk has read them all there already, so it files them only
	// under the keys it looks up without that read, and a test that owns
	// thousands of mocks has none of their other keys computed. The same
	// rules as for Keys hold.
	OwnedKeys func(*models.Mock) []string
}

// SessionKeyReader is an optional MockMemDb extension: the GetSessionMocks
// snapshot narrowed to the mocks an index files under one key, without
// walking the rest of it. A replay's reusable pool holds every test's traffic
// in lax mode, so a matcher that walked it for each command would cost more
// with every test the set holds.
type SessionKeyReader interface {
	// RangeSessionMocksWithKey calls fn with each mock of the session tier
	// that ix files under key, in GetSessionMocks order, until fn returns
	// false. fn is called without the store's locks held, so it may call
	// back into the store; the walk then sees the tier as it is by the time
	// it gets there, as successive reads would.
	RangeSessionMocksWithKey(ix *MockIndex, key string, fn func(*models.Mock) bool) error
}

// PerTestRanger is an optional MockMemDb extension: the
// GetPerTestMocksInWindow read handed one mock at a time, in its order, until
// the caller stops. Under exact serving the open test's own unanswered mocks
// come first, read a few at a time, and the tier only after them: a matcher
// that stops at the test's next recording reads neither the mocks the test
// answered nor the rest of the tier, so a test that owns many recordings
// costs each call the same as one that owns few.
type PerTestRanger interface {
	// RangePerTestMocksInWindow calls fn with each mock
	// GetPerTestMocksInWindow would return, in its order, until fn
	// returns false. fn is called without the store's locks held, so it
	// may call back into the store; the walk then sees the tier as it is
	// by the time it gets there, as successive reads would.
	RangePerTestMocksInWindow(fn func(*models.Mock) bool) error
}

// MockReader is the read-only facet of MockMemDb. Parsers that need
// to enumerate mocks from the in-memory pool but never mutate or
// consume on match should take this interface directly. Includes both
// the historical accessor API (GetFilteredMocks / GetUnFilteredMocks
// / GetFilteredMocksInWindow) and the unification API (GetSessionMocks
// / GetPerTestMocksInWindow / GetStartupMocks / GetConnectionMocks /
// GetSessionScopedMocks).
type MockReader interface {
	// --- Historical API (alias layer during Phase 2 migration). ---
	//
	// Deprecated: use GetPerTestMocksInWindow or GetSessionMocks.
	// GetFilteredMocks returns the current per-test-pool snapshot,
	// ignoring any active test window. Kept for compatibility with
	// parsers that have not yet migrated.
	GetFilteredMocks() ([]*models.Mock, error)
	// Deprecated: use GetSessionMocks.
	// GetUnFilteredMocks returns the current session-pool snapshot.
	GetUnFilteredMocks() ([]*models.Mock, error)

	GetMySQLCounts() (total, config, data int)

	// Deprecated: use GetPerTestMocksInWindow.
	GetFilteredMocksInWindow() ([]*models.Mock, error)

	// --- Unification API (primary going forward). ---

	// GetPerTestMocksInWindow returns only those per-test (Lifetime ==
	// LifetimePerTest) mocks placed inside the current window [start,
	// end] (models.PlaceOf: Spec.ReqTimestampMock, or Spec.ResTimestampMock
	// for a mock placed at its answer), and those in flight when the
	// window's test began: sent in the gap before the window
	// (models.WindowSchedule.GapBefore) and answered at or after start
	// (models.InFlightEligible). Mocks whose Spec.ResTimestampMock
	// is EARLIER than Spec.ReqTimestampMock are excluded as invalid.
	// Response timestamps after `end` are tolerated (async straggle).
	// If no window has been set (zero values), falls back to the full
	// per-test pool snapshot.
	//
	// Canonical name; GetFilteredMocksInWindow aliases this.
	GetPerTestMocksInWindow() ([]*models.Mock, error)

	// Deprecated: Wave 2 split startup-tier traffic out of the session
	// pool into its own storage tier. GetSessionMocks now returns the
	// UNION of startup + session so pre-wave-2 parsers keep working; new
	// parsers that want strict tier separation should call
	// GetStartupMocks and GetSessionScopedMocks directly.
	//
	// GetSessionMocks returns the session-scoped mock pool snapshot
	// (Lifetime == LifetimeSession) plus any startup-tier mocks (those
	// placed before the first test window, models.PlaceOf).
	// Session mocks are reusable across every test — never
	// window-filtered, never consumed on match.
	GetSessionMocks() ([]*models.Mock, error)

	// GetStartupMocks returns the startup-tier mock pool snapshot —
	// app-bootstrap recordings placed before the first HTTP test window
	// (models.PlaceOf: Spec.ReqTimestampMock, or the answer's for a mock
	// placed at its answer) (Flyway migrations, Hibernate metadata
	// boot, HikariCP pool warm-up). Strictly disjoint from
	// GetSessionScopedMocks, GetPerTestMocksInWindow, and
	// GetConnectionMocks; a mock lives in exactly one of the four
	// tiers.
	//
	// Wave 2 addition: tier-aware parsers build a dedicated startup
	// index from this pool to serve bootstrap traffic, then switch to
	// the session / per-test tiers as the runner advances into real
	// test windows.
	GetStartupMocks() ([]*models.Mock, error)

	// GetStartupMocksByKind returns startup-tier mocks matching the
	// given Kind. Symmetric counterpart to the session- / per-test-
	// tier by-kind accessors (GetUnFilteredMocksByKind,
	// GetFilteredMocksByKind). Parsers that build per-kind indices
	// over the startup tier should prefer this over filtering the
	// GetStartupMocks snapshot client-side.
	//
	// Returns (nil, nil) when the startup tree has never been
	// populated on the underlying manager (fresh manager, pre-first
	// SetMocksWithWindow call).
	GetStartupMocksByKind(kind models.Kind) ([]*models.Mock, error)

	// GetSessionScopedMocks returns the session-tier + connection-tagged
	// mocks strictly — startup-tier entries are NOT included (those are
	// in GetStartupMocks). Parsers opting into the Wave 2 tier-aware
	// routing call this in preference to the legacy GetSessionMocks
	// union shim.
	GetSessionScopedMocks() ([]*models.Mock, error)

	// GetConnectionMocks returns connection-scoped mock pool entries
	// (Lifetime == LifetimeConnection) associated with the given
	// connID (Spec.Metadata["connID"]). These are reusable for the
	// lifetime of that specific client connection only. Prepared-
	// statement setup mocks (Postgres Parse, MySQL COM_STMT_PREPARE)
	// use this pool so their executes still find them across test-
	// window boundaries while remaining isolated per connection.
	//
	// Returns an empty slice (no error) if connID has no associated
	// connection pool — the caller should then fall through to the
	// session / per-test pools.
	GetConnectionMocks(connID string) ([]*models.Mock, error)

	// SessionMockHitCounts returns the match counts of the session- and
	// connection-scoped mocks in the pool, counted by MarkMockAsUsed over
	// the current test set. Used by replay summary output and "which
	// reusable mocks actually got reused?" telemetry. Key is mock.Name
	// (mocks sharing a name share a count). Inherently racy as a
	// snapshot — counters may increment during iteration — but that's
	// tolerable for observability.
	SessionMockHitCounts() map[string]uint64
}

// MockWriter is the in-place mutation facet of MockMemDb — narrow on
// purpose, because in-place mock updates are rare (the recorder writes
// through a different path). UpdateUnFilteredMock is used by parsers
// that rewrite a session-pool entry after augmenting it on the fly
// (e.g. stamping server-issued auth tokens onto a session mock so a
// later matcher sees the updated bytes).
type MockWriter interface {
	UpdateUnFilteredMock(old *models.Mock, new *models.Mock) bool
}

// MockConsumer is the match-time consumption facet of MockMemDb.
// Per-test mocks are consumed on match (DeleteFilteredMock), session
// and connection mocks are marked-used for telemetry
// (MarkMockAsUsed); DeleteUnFilteredMock exists for the rare consumer
// that eagerly evicts a session-pool entry (e.g. the MySQL prepared-
// statement teardown path on COM_STMT_CLOSE); DeleteStartupMock
// consumes a startup-tier mock matched during application bootstrap so
// the next identical query picks the next-recorded mock in chronological
// order (the boot path's analogue of DeleteFilteredMock for per-test).
type MockConsumer interface {
	DeleteFilteredMock(mock models.Mock) bool
	DeleteUnFilteredMock(mock models.Mock) bool
	// DeleteStartupMock removes a matched startup-tier mock from the
	// startup tree. Returns true if the mock was found and removed,
	// false otherwise (e.g. the mock wasn't classified into startup at
	// load time, or it has already been consumed by a prior match).
	//
	// Required for boot-phase replay correctness when an application
	// issues the same query repeatedly during recording while DB state
	// mutates: the recorder captures multiple same-shape mocks with
	// diverging responses, the boot-phase matcher (with the earliest-
	// wins tiebreaker) picks them in chronological order, and consumption
	// here advances through the recorded sequence so the booting app
	// sees the same response chain it saw at record time. Without this,
	// the matcher repeatedly picks the same earliest mock and the
	// booting app's follow-on revalidation queries fail.
	DeleteStartupMock(mock models.Mock) bool
	MarkMockAsUsed(mock models.Mock) bool
}

// RecordedWindowsReader is an OPTIONAL MockMemDb facet (type-assert for it; the
// agent's MockManager implements it). It exposes the recorded window of every
// test of the set being replayed, selected or not, as the replayer seeded it at
// staging (models.MockFilterParams.RecordedWindows).
//
// A parser whose protocol has traffic the test windows do not pace by
// themselves — a broker's server push — uses it to hold that traffic until the
// replay reaches the recorded window it belongs to:
//
//	sched := db.RecordedWindows()                 // nil: release everything
//	start, _ := db.CurrentTestWindow()
//	due := sched.Released(msg.Spec.ReqTimestampMock, start)
//
// The schedule is immutable; it is replaced (never mutated) at each set's
// staging call, which also changes WindowPacer.StagingEpoch.
//
// The agent keeps the windows only while some kind has registered a carry-over
// predicate (models.RegisterCarryOver); otherwise RecordedWindows is nil. A
// parser that paces by them registers its kind, as the traffic it paces is
// what carry-over exists for.
type RecordedWindowsReader interface {
	RecordedWindows() *models.WindowSchedule
}

// WindowPacer is an OPTIONAL MockMemDb facet (type-assert for it; the agent's
// MockManager implements it) for a parser that holds traffic until the replay
// reaches a recorded window — FLOW permits that find nothing due yet — and must
// serve it when the window moves, with no request of its own to wake it.
//
//   - WindowChanged returns a channel closed at the next test-window change,
//     after the new window and trees are visible. Take it BEFORE reading the
//     state it guards; each change hands out a fresh channel. It is also closed
//     once when the manager is closed.
//   - StagingEpoch changes at every staging call (a new set, or the set staged
//     again for a replacement agent). State built from one staging snapshot,
//     such as per-set delivery queues, is rebuilt when it changes.
//
// A MockMemDb without it gives no signal: serve held work at the connection's
// next request instead.
type WindowPacer interface {
	WindowChanged() <-chan struct{}
	StagingEpoch() uint64
}

// CarryOverReader is an OPTIONAL MockMemDb facet (type-assert for it; the
// agent's MockManager implements it) for a parser whose kind registered a
// carry-over predicate (models.RegisterCarryOver). It returns, in recorded
// order, the registered per-test mocks that are reachable outside their own
// test window: loaded up to models.CarryOverLookahead ahead of their release
// window, and kept after their window closes until consumed. Serve them after
// the running test's own mocks (GetPerTestMocksInWindow), and consume them
// through DeleteFilteredMock, which falls back per-test, then startup, then
// carry-over, and reports a carry-over consume with MockState.CarryOver.
type CarryOverReader interface {
	GetCarryOverMocks() ([]*models.Mock, error)
	GetCarryOverMocksByKind(kind models.Kind) ([]*models.Mock, error)
}

// WindowAware is the test-window facet of MockMemDb. Parsers that
// partition their index into per-test / session / startup tiers
// consult these accessors at dispatch time to pick the right tier
// for a live query. The pair (IsTestWindowActive, HasFirstTestFired)
// is NON-ATOMIC — callers that need a coherent point-in-time read of
// both bits MUST use WindowSnapshot.
type WindowAware interface {
	// SetCurrentTestWindow records the [start,end] timestamps of the outer
	// HTTP/gRPC test currently being replayed. Parsers use this window via
	// GetPerTestMocksInWindow to filter non-config mocks by their REQUEST
	// timestamp. Responses may legitimately straggle past `end` (downstream
	// async completion); mocks with an invalid timestamp ordering
	// (ResTimestampMock earlier than ReqTimestampMock) are dropped as a
	// sanity check, but response containment is NOT enforced against `end`.
	SetCurrentTestWindow(start, end time.Time)

	// IsTestWindowActive reports whether a non-zero test window has been
	// set via SetCurrentTestWindow or SetMocksWithWindow, or a test of keploy
	// mock replay begun without a worker pid is open (SetScopeMocks; any
	// other window staging or SetCurrentTestWindow closes it): a window with
	// no recorded-time bounds, for which CurrentTestWindow reads zero and
	// FirstTestWindowStart does not move. Parsers that
	// partition their index into per-test and session tiers consult this
	// at dispatch time to decide which tier a live query should be served
	// from: true = inside a test-body window (route to per-test index),
	// false = session / connection-scoped traffic (route to session index).
	//
	// Inherently racy — a concurrent test-window flip could change the
	// answer between observation and use — but callers that need strict
	// window/pool atomicity go through GetPerTestMocksInWindow, which
	// snapshots both under the manager's swap lock.
	IsTestWindowActive() bool

	// HasFirstTestFired reports whether at least one real test window
	// has been set on the underlying MockManager via SetMocksWithWindow
	// (non-zero start that is not the BaseTime staging sentinel).
	// Sticky within a test-set; ResetForReplaySession clears it at each
	// set boundary, so a set being staged reads false again. This is the
	// same underlying value FirstTestWindowStart below exposes as a
	// timestamp — "has this set's first test fired" and "when did it fire"
	// are one field, so they share that per-set lifetime.
	//
	// One exception: while a test of keploy mock replay begun without a
	// worker pid is open (SetScopeMocks), this reports true with
	// IsTestWindowActive, and false again once it ends; the scope sets no
	// FirstTestWindowStart. Such a replay serves between tests as before the
	// first test.
	//
	// Parsers use this alongside IsTestWindowActive to distinguish
	// "app bootstrap" (before first test) from "between tests" (after
	// the first test fired but no test currently active):
	//
	//   IsTestWindowActive() == true                  → perTest tier
	//   !IsTestWindowActive() &&  HasFirstTestFired() → session tier
	//   !IsTestWindowActive() && !HasFirstTestFired() → startup tier
	//
	// NON-ATOMIC-PAIR WARNING: reading IsTestWindowActive and
	// HasFirstTestFired sequentially can observe the forbidden
	// intermediate state Active=true && FirstTestFired=false during a
	// SetCurrentTestWindow / SetMocksWithWindow transition because the
	// two bits are guarded by different locks on the underlying
	// MockManager. Callers that need the pair as a coherent point-in-
	// time read (the v3 dispatcher's routeTransactional and
	// TierIndex.orderForCurrentState) MUST use WindowSnapshot instead.
	HasFirstTestFired() bool

	// FirstTestWindowStart returns the earliest test window start observed
	// by the MockManager FOR THE TEST SET BEING REPLAYED, or zero if no real
	// test has fired in it yet (i.e. every SetMocksWithWindow call so far was
	// either absent or fired with the models.BaseTime staging sentinel).
	// ResetForReplaySession clears it at each set boundary, so each set
	// classifies against its own first test. Used by filter / strict-gate
	// callers to distinguish startup-init mocks (req_ts < firstWindowStart)
	// from stale previous-test mocks (firstWindowStart <= req_ts <
	// currentStart): the former are legitimate app-bootstrap traffic that
	// must be preserved, the latter are cross-test bleed that must be
	// dropped.
	FirstTestWindowStart() time.Time

	// WindowSnapshot returns the (IsTestWindowActive, HasFirstTestFired)
	// pair under one outer lock on the underlying MockManager, so
	// callers that read BOTH bits cannot observe a torn intermediate
	// state during a concurrent SetCurrentTestWindow /
	// SetMocksWithWindow transition.
	//
	// Required for any caller that routes based on the PAIR (the v3
	// Postgres dispatcher's routeTransactional and
	// types.TierIndex.orderForCurrentState). The individual bool
	// accessors are retained for legacy callers that read only one bit.
	WindowSnapshot() models.WindowSnapshot

	// CurrentTestWindow returns the [start, end] request-timestamp bounds of
	// the outer test currently being replayed, or (zero, zero) when no window
	// is active or the active one has no recorded-time bounds (a test of
	// keploy mock replay begun without a worker pid; IsTestWindowActive tells
	// the two apart). A caller must not read zero bounds as "nothing is in
	// the window". Matchers that receive per-test data mocks via the session
	// pool (the enterprise agent lax-promotes them and relies on the
	// MockManager for strict windowing) use this to tell an in-window mock
	// from a stale earlier-test mock when disambiguating repeated identical
	// prepared-statement reads.
	CurrentTestWindow() (time.Time, time.Time)
}
