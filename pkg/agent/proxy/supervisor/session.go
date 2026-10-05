package supervisor

import (
	"context"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/directive"
	"go.keploy.io/server/v3/pkg/agent/proxy/fakeconn"
	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

// PostRecordHook is invoked after a shared parser produces a mock and before
// the mock is handed off for storage. Wrapper parsers (for example an
// enterprise SQS parser that delegates recording to the OSS HTTP parser) use
// this hook to annotate or reshape the mock without teaching the shared
// parser about downstream protocols.
//
// Call Session.AddPostRecordHook rather than assigning to OnMockRecorded
// directly — the helper preserves any hook already installed by an outer
// parser, which is the usual chaining contract.
type PostRecordHook func(*models.Mock)

// Session bundles every resource a parser needs during record mode under
// the new split-ownership architecture. It is the successor to
// integrations.RecordSession and is a strict superset of that type's
// surface so parser migration is mechanical.
//
// The fields divide into three layers:
//
//  1. The new FakeConn-based I/O path (ClientStream, DestStream, Directives,
//     Acks, Mocks, ClientConnID, DestConnID, Opts, Logger, Ctx). Every
//     migrated parser should read exclusively from these.
//
//  2. Backward-compatibility fields retained only until every parser is
//     ported (Ingress, Egress, TLSUpgrader, ErrGroup). The shim that
//     feeds the old parsers into the new proxy populates these; new
//     parsers must not read them.
//
//  3. Hook surface (OnMockRecorded) shared across generations.
//
// Session carries modest internal bookkeeping (incomplete-mock flag,
// post-record hook chain mutex) used by the EmitMock / MarkMock*
// helpers. Methods on *Session are safe for concurrent use.
type Session struct {
	// --- New architecture ---

	// ClientStream is the read-only view of bytes the real client sent.
	// The relay tees each chunk of client→server traffic onto its channel.
	ClientStream *fakeconn.FakeConn

	// DestStream is the read-only view of bytes the real destination sent.
	// The relay tees each chunk of server→client traffic onto its channel.
	DestStream *fakeconn.FakeConn

	// Directives is the parser's send channel for control messages to
	// the relay/supervisor (TLS upgrade, abort, finalize, pause/resume).
	Directives chan<- directive.Directive

	// Acks is the parser's receive channel for directive acknowledgements.
	Acks <-chan directive.Ack

	// Mocks is the mock-sink channel. Parsers should call EmitMock
	// rather than sending here directly so the incomplete-mock gate
	// and the post-record hook chain run consistently.
	Mocks chan<- *models.Mock

	// Logger is pre-configured with connection-scoped fields
	// (client conn ID, dest conn ID, addresses).
	Logger *zap.Logger

	// Ctx is the supervisor-managed lifetime for this parser run. It
	// is cancelled on outer cancel, hang, panic, or mem-cap. The
	// supervisor overwrites this field with its derived context
	// before invoking the parser; callers should not set it.
	Ctx context.Context

	// ClientConnID identifies the client connection for logging and
	// mock grouping. Carried into emitted mocks as ConnectionID.
	ClientConnID string

	// DestConnID identifies the destination connection for logging.
	DestConnID string

	// Opts carries protocol-specific options (bypass rules, passwords,
	// TLS keys, noise config, etc.).
	Opts models.OutgoingOptions

	// ClientWritesHeld reports that the relay behind this session armed
	// a client write hold (relay.Config.HoldClientWrites), so nothing
	// the client sends reaches the real destination until the parser
	// ends the hold — with directive.ReleaseClient, or with a
	// directive.UpgradeTLS carrying a ClientFlushBytes count.
	//
	// The dispatcher sets it from the same capability probe that arms
	// the hold, so a parser can tell "I asked for a hold" from "a hold
	// is actually in place". Those differ: an observe-only proxyless
	// capture has no relay to hold anything, and a parser that assumed
	// otherwise would send a release that nobody answers and block on
	// the ack forever. False on every path that does not hold, which
	// is every path today except MySQL under the V2 relay.
	ClientWritesHeld bool

	// JoinedMidConnection reports that this capture of the connection began
	// after the connection was established: the agent started, or restarted,
	// or a TLS hook attached, while the connection was open, so a request in
	// flight then was not captured and its answer may start the server
	// stream. The producer sets it once, when it makes the session, from what
	// it saw: false when it saw the connection open (the relay, which carries
	// a connection from its first byte, never sets it), true otherwise.
	// Session.NextRequest reports the server bytes it drops as the answer to
	// a request the capture does not have only when it is set. A producer
	// that sets it ends the server stream (closes DestStream's channel) as
	// the connection ends and as Ctx does: a parser that has not read that
	// stream past its start waits on it as it returns (EndExchanges). In
	// proxy mode every session's stream ends so: the relay ends it with the
	// connection and with the recording's context, which Ctx derives from,
	// and the supervisor closes it as it cancels Ctx on its own (a hang, the
	// memory cap).
	JoinedMidConnection bool

	// RouteMocksViaSyncMock, when true, makes EmitMock deliver the
	// mock via the package-singleton syncMock.SyncMockManager
	// (AddMock) instead of directly sending on s.Mocks. Production
	// recordViaSupervisor sets this to true so the V2 path gets the
	// same firstReqSeen session-window buffering, lifetime
	// derivation, and drop accounting that legacy parsers enjoy —
	// without it, V2-recorded mocks captured before the first app
	// test request fall outside the session window and are lost at
	// replay.
	//
	// Tests that wire a bare mocks channel and want to observe the
	// emitted mocks directly should leave this false (default) so
	// the direct-channel fallback below still fires. The two paths
	// both run the OnMockRecorded hook chain and the ClientConnID
	// / monotonic-timestamp normalisation — they only differ on the
	// final handoff.
	RouteMocksViaSyncMock bool

	// --- Backward-compatibility (populated by the migration shim) ---

	// Ingress is the legacy client-side net.Conn handle. nil on the
	// new code path; parsers on the new path must not use it.
	Ingress net.Conn

	// Egress is the legacy destination-side net.Conn handle. nil on
	// the new code path; parsers on the new path must not use it.
	Egress net.Conn

	// TLSUpgrader is the legacy mid-stream TLS upgrade API. On the
	// new code path, parsers send directive.KindUpgradeTLS instead.
	// Retained until every parser is migrated.
	TLSUpgrader models.TLSUpgrader

	// ErrGroup is the legacy parser-goroutine accounting. New parsers
	// use Supervisor.RegisterGoroutine. Deprecated; remove after
	// migration.
	ErrGroup *errgroup.Group

	// --- Hook surface ---

	// OnMockRecorded runs against each newly created mock before it is
	// stored. Wrapper parsers use AddPostRecordHook to chain hooks
	// front-of-chain so an outer hook's annotations are preserved.
	OnMockRecorded PostRecordHook

	// OnPendingCleared is called by EmitMock after a successful
	// emit so the supervisor can release "pending work" state — the
	// parser has visibly made progress on the request in flight.
	// Typically wired to supervisor.ClearPendingWork by the dispatcher.
	// Nil is safe.
	OnPendingCleared func()

	// SuspendWatchdog, when non-nil, permanently disarms the hang
	// watchdog for this connection. A parser calls it once it identifies
	// the in-flight request as a long-poll (e.g. an async httpPoll lane):
	// such a request holds the connection open with no byte progress for
	// far longer than the hang budget, which the watchdog would otherwise
	// abort as a hang (falling through to passthrough and losing the
	// mock). Typically wired to supervisor.SuspendWatchdog by the
	// dispatcher. Nil is safe.
	SuspendWatchdog func()

	// OnStop, when non-nil, is told the session's stop instant (StoppedAt)
	// once, by the StoppedAt call that reads it from the clock, before that
	// call returns. That call is made by whatever stops the connection's
	// recording first, as it happens: the parser as it reports the exchange
	// it stops on (ReportStoppedOn), the supervisor as it learns that the
	// parser panicked or returned with an error, the dispatcher's abort as it
	// pauses the capture of a parser it retires, or the relay at a capture
	// hole. The dispatcher opens the span of what the connection carries from
	// the stop on here (syncMock.UnrecordedConn), so the span is open from the
	// moment the stop is stamped, before any of those logs it: a test case
	// checked from then on overlaps it. It runs on the goroutine of the event
	// that stamped the stop, so it must not block, and must not call
	// StoppedAt. Set it before the parser runs.
	OnStop func(at time.Time)

	// RecordingDone is closed once the recording this connection is captured
	// for stops: the dispatcher sets it to the Done channel of the recording's
	// context. It is not the parser's lifetime (Ctx), which also ends when
	// the parser is retired. It is read through RecordingStopping, the rule.
	// nil: the session is not told when its recording stops, and
	// RecordingStopping is false. Set it before the parser runs.
	RecordingDone <-chan struct{}

	// Mgr is the per-session sync-mock manager EmitMock routes through when
	// RouteMocksViaSyncMock is set. Lets a multi-app caller give each app its
	// own manager (so the V2 emit path is per-app, matching the legacy
	// parsers). nil ⇒ the package-global manager (single-session default).
	// Session.manager is that rule: the session reads its manager only
	// through it, never through Mgr directly.
	Mgr *syncMock.SyncMockManager

	// Orphans, when set, receives the spans this connection could not record
	// (RecordOrphanWindow, OpenOrphanWindow) instead of Mgr. A caller whose
	// test cases are checked against spans kept elsewhere sets it: a DaemonSet
	// agent keeps them per pod, since its test cases can only ride their own
	// pod's connections. nil ⇒ Mgr, as before.
	Orphans OrphanSpans

	// --- Internal bookkeeping ---

	// exchanges is NextRequest's state.
	exchanges exchanges
	// endedWithConnection, per fakeconn.Direction: that stream ended because
	// its connection did (MarkEndedWithConnection).
	endedWithConnection [2]atomic.Bool
	// endedAtHole, per fakeconn.Direction: that stream ended at a capture
	// hole, and why (MarkEndedAtHole). nil while it has not.
	endedAtHole      [2]atomic.Pointer[string]
	hookMu           sync.Mutex
	lastReqMu        sync.Mutex
	lastReqTimestamp time.Time // most recent ReqTimestampMock emitted on this session

	// incomplete is the incomplete-mock flag (MarkMockIncomplete): nil while
	// it is clear, else why it was set, the first reason since it was last
	// cleared. The reason is the flag, one atomic: whoever takes the flag
	// (TakeMockIncomplete) takes the reason that set it, and a mark that
	// lands while the flag is being taken is either taken with it or left set
	// for the next mock, never lost.
	incomplete atomic.Pointer[string]

	// stoppedAt is the instant the connection stopped being recorded
	// (StoppedAt), in Unix nanoseconds: 0 until StoppedAt first reads the
	// clock. The call that stores it tells OnStop.
	stoppedAt atomic.Int64
}

// AddPostRecordHook adds h to the front of the session's post-record chain
// so h runs before any previously-installed hook. The previously-installed
// hook (if any) then observes the mock already annotated by h and can layer
// its own annotations on top without clobbering them.
//
// Calling with a nil hook, or on a nil *Session, is a no-op. Making
// the nil-receiver case safe lets defensive call sites drop their own nil
// guard before invoking AddPostRecordHook.
func (s *Session) AddPostRecordHook(h PostRecordHook) {
	if s == nil || h == nil {
		return
	}
	s.hookMu.Lock()
	defer s.hookMu.Unlock()
	prev := s.OnMockRecorded
	if prev == nil {
		s.OnMockRecorded = h
		return
	}
	s.OnMockRecorded = func(m *models.Mock) {
		h(m)
		prev(m)
	}
}

// MarkMockIncomplete sets the session's incomplete-mock flag. The next mock
// EmitMock is handed, or a parser checks the flag for itself, is left out
// and reported (LeaveOutIfIncomplete, ReportLeftOut): its exchange as one the
// connection could not record (RecordOrphanWindow), a count for the
// recording's summary, and a log line, WARN rate-limited, with reason. The
// forward path is unaffected: only mock recording is.
//
// The relay calls this when it gates a chunk at the tee; parsers call
// it when they cannot continue decoding a mock cleanly. Safe to call
// repeatedly: while the flag is set, further calls are no-ops, and the
// reason kept is the first. Each call that sets the flag logs its reason at
// Debug.
//
// The flag leaves out whichever mock is emitted next. A parser that knows
// which exchange it cannot record reports that exchange instead, with
// ReportLeftOut(m, reason), m a minimal mock of it (its Kind and times), and
// leaves the flag alone: marking it here leaves out the next mock it emits,
// which may be a healthy one. ReportLeftOut records the exchange's span,
// counts it, logs the rate-limited WARN and clears pending work, and does not
// touch the flag. RecordOrphanWindow alone records only the span: nothing
// counts the loss or says why it happened, so a parser that leaves out an
// exchange of a connection it goes on recording reports it with ReportLeftOut,
// as the MySQL recorder's leaveOut does, and one that returns on the exchange
// reports it with ReportStoppedOn.
func (s *Session) MarkMockIncomplete(reason string) {
	if s == nil {
		return
	}
	// The relay calls this for every chunk it drops, so the usual call finds
	// the flag set already and must cost nothing: return before anything is
	// allocated. Only a call that may set the flag copies its reason to the
	// heap (r escapes through the CompareAndSwap; reason itself must not, or
	// every call would allocate it on entry).
	if s.incomplete.Load() != nil {
		return
	}
	r := reason
	if s.incomplete.CompareAndSwap(nil, &r) && s.Logger != nil {
		s.Logger.Debug("mock marked incomplete", zap.String("reason", reason), zap.String("connID", s.ClientConnID))
	}
}

// MarkEndedWithConnection says dir's stream ended because its connection did,
// with every byte the capture was handed for it: not at a hole, and not because
// the capture stopped (the recording ended) while the connection carried on.
// The caller calls it before it ends the stream (closes its chunk channel), so
// a parser that reaches the end of the stream sees it. It is tied to the end of
// the stream, the one place a parser that runs behind its capture can be sure
// of (unlike MarkMockIncomplete, which voids whichever mock is emitted next).
//
// It lets a parser of an observe-only capture (OutgoingOptions.SkipTLSMITM: a
// DaemonSet's), which carries the bytes the client read and nothing else,
// record a message its stream ends in the middle of as the client had it: the
// client read that much and closed the connection. Without it the end is not
// known to be the client's (the rest may have been lost, or never captured),
// and such a message is not recorded. Nil-safe.
func (s *Session) MarkEndedWithConnection(dir fakeconn.Direction) {
	if s == nil || int(dir) >= len(s.endedWithConnection) {
		return
	}
	s.endedWithConnection[dir].Store(true)
}

// EndedWithConnection reports whether dir's stream ended because its
// connection did (MarkEndedWithConnection). Nil-safe.
func (s *Session) EndedWithConnection(dir fakeconn.Direction) bool {
	if s == nil || int(dir) >= len(s.endedWithConnection) {
		return false
	}
	return s.endedWithConnection[dir].Load()
}

// MarkEndedAtHole says dir's stream ended at a capture hole: a chunk of it was
// lost, for reason (a relay drop reason, such as relay.DropPerConnCap), and the
// capture delivers nothing after it, while the connection may go on. The stream
// carries every byte captured before the hole. The caller calls it before it
// ends the stream (closes its chunk channel), so a parser that reaches the end
// of the stream sees it. The first reason is kept. Nil-safe.
//
// Like MarkEndedWithConnection, it is tied to the end of the stream, the one
// place a parser that runs behind its capture can be sure of. A mark of the
// incomplete-mock flag (MarkMockIncomplete) is set when the chunk is lost, and
// the parser takes it, or EmitMock does, at its next chunk or mock, which can
// be a full queue of chunks captured before the hole later. The relay calls it
// for a parser that asks for it (relay.Config.EndAtHole,
// integrations.EndAtHoleCapable), and marks no mock incomplete for the hole.
func (s *Session) MarkEndedAtHole(dir fakeconn.Direction, reason string) {
	if s == nil || int(dir) >= len(s.endedAtHole) {
		return
	}
	r := reason
	s.endedAtHole[dir].CompareAndSwap(nil, &r)
}

// EndedAtHole reports whether dir's stream ended at a capture hole, and why
// (MarkEndedAtHole). A parser asks it once the stream has ended: an end with no
// hole is the connection's, or the capture's. Nil-safe.
func (s *Session) EndedAtHole(dir fakeconn.Direction) (reason string, ok bool) {
	if s == nil || int(dir) >= len(s.endedAtHole) {
		return "", false
	}
	r := s.endedAtHole[dir].Load()
	if r == nil {
		return "", false
	}
	return *r, true
}

// MarkMockComplete clears the incomplete-mock flag, and with it the mark that
// set it. A parser calls it when it ends an exchange it records nothing for (a
// passthrough request, say), so a mark set during that exchange does not leave
// out the next exchange's mock. Idempotent.
//
// It is never called after EmitMock. EmitMock has taken the flag already, so a
// mark set since (by the relay, while EmitMock delivered) is the next mock's:
// clearing it records that mock with nothing reporting it. A parser that drops
// a mock for the flag calls LeaveOutIfIncomplete instead, which reports it.
func (s *Session) MarkMockComplete() {
	if s == nil {
		return
	}
	s.incomplete.Store(nil)
}

// IsMockIncomplete reports whether the session's active mock has been
// marked incomplete. Parsers may use this to short-circuit expensive
// encoding work they know will be dropped. A parser that then drops its
// mock without emitting it must say so: LeaveOutIfIncomplete takes the flag
// and reports the mock in one call. Clearing the flag itself instead
// (MarkMockComplete) loses the mock with nothing reporting it, and loses a
// mark set between the check and the clear as well.
func (s *Session) IsMockIncomplete() bool {
	if s == nil {
		return false
	}
	return s.incomplete.Load() != nil
}

// TakeMockIncomplete takes the incomplete-mock flag: it clears the flag and
// returns the reason that set it, or ok false if the flag was clear. It is one
// atomic step, so a mark set while it runs is either taken with it or left set
// for the next mock, never lost, unlike IsMockIncomplete followed by
// MarkMockComplete.
//
// It is for a parser that decides itself which exchanges a mark hits, instead
// of leaving out the next mock it emits: one that carries several exchanges at
// once on a connection, say, and drops the ones in flight when a chunk was
// lost. Whoever takes the flag takes the duty to report what it leaves out:
// such a parser calls ReportLeftOut once for each exchange it leaves out for
// the mark, passing the reason it took, and emits nothing for it.
// RecordOrphanWindow alone is not enough: it records the span, but nothing
// counts the mock or says why it is missing. A parser that leaves out one mock
// for the flag calls LeaveOutIfIncomplete. Nil-safe.
func (s *Session) TakeMockIncomplete() (reason string, ok bool) {
	// EmitMock takes the flag for every mock, and it is nearly always clear:
	// read it before writing it, so the usual call does not write the word the
	// relay reads on every chunk it drops.
	if s == nil || s.incomplete.Load() == nil {
		return "", false
	}
	r := s.incomplete.Swap(nil)
	if r == nil {
		return "", false
	}
	return *r, true
}

// LeaveOutIfIncomplete takes the incomplete-mock flag (TakeMockIncomplete)
// and, if it was set, leaves m out and says so (ReportLeftOut): m's exchange,
// [ReqTimestampMock, ResTimestampMock], is reported as one the connection
// could not record, which leaves out the test cases recorded over it that the
// recording has not saved yet (ReportLeftOut says when one may have been); it
// is counted for the recording's summary and logged with why the flag was set;
// and the session's pending work is cleared, since the parser has consumed its
// input. It reports whether m was left out: the caller must not emit m then. A
// caller that keeps its own spans (Session.Orphans: a DaemonSet agent, per
// pod) checks that pod's test cases against them instead.
//
// EmitMock calls it first, so a parser that emits every mock it builds need
// not. A parser that checks the flag itself, to drop a mock without building
// or emitting it, calls this instead of IsMockIncomplete and
// MarkMockComplete, passing what it knows of the mock: its kind and its
// exchange's times are enough. Its flag is cleared as it is taken, so the
// next mock gets a fresh chance.
//
// A nil m is a no-op that leaves the flag alone, like EmitMock's. Nil-safe.
func (s *Session) LeaveOutIfIncomplete(m *models.Mock) bool {
	if s == nil || m == nil {
		return false
	}
	reason, ok := s.TakeMockIncomplete()
	if !ok {
		return false
	}
	s.ReportLeftOut(m, reason)
	return true
}

// RecordOrphanWindow marks a [start,end] interval over which this connection
// carried an exchange that was not recorded as a mock. It is called for:
//
//   - a hole a parser reports once it knows its width: the mongo/v2 parser's
//     reassembly resync hole (a dropped chunk desyncs the framer, so a message
//     delivered during the hole is never turned into a mock);
//   - every mock the incomplete-mock flag leaves out (LeaveOutIfIncomplete,
//     ReportLeftOut), whatever set the flag: a decode failure, a short or
//     failed write, or a chunk the relay's tee gated, memory pressure
//     included. That covers each exchange a parser that takes the flag itself
//     leaves out for it, such as the half a generic-parser split carries out
//     of an exchange the flag left out;
//   - each exchange a parser reports it cannot record, without the flag
//     (ReportLeftOut): the MySQL recorder's leaveOut, say, for a command that
//     does not decode, a cursor's fetch, a packet of 16 MiB or more, or a
//     response it cannot frame, after which it goes on from the client's
//     next command;
//   - the exchange a parser stops on as it returns (ReportStoppedOn): the
//     HTTP/1 recorder's request or response that does not decode, or the
//     command the MySQL recorder was in when it stopped recording a
//     connection (lost framing of the client's stream, or a response it
//     cannot frame and cannot take the connection up again after, as from a
//     client that pipelines its commands).
//
// So a recording under memory pressure records orphan spans as well as
// pressure spans: an orphan span says a connection lost an exchange, not that
// something other than pressure lost it (SyncMockManager.OrphanRangeCount).
//
// It routes to Orphans when set, else to the session's manager
// (Session.manager: the per-app s.Mgr, or the package singleton), the one
// EmitMock adds the session's mocks to, so record.go's TC-suppression treats
// the span like a memory-pressure interval and suppresses every TC whose window
// overlaps it, instead of shipping that TC mock-less (replay would then report
// match_phase=no_mocks). It reaches only the TCs record.go checks after the
// span is recorded: one it streamed before is saved without the mock
// (ReportLeftOut). The keploy/integrations parsers call this session's methods
// directly, not through an interface they probe for: mongo/v2 calls
// RecordOrphanWindow, for its resync holes, and ReportLeftOut, and http2 calls
// TakeMockIncomplete, ReportLeftOut and EndedAtHole (their tests also call
// ResetLeftOutWarningsForTest and SyncMockManager.MocksLeftOut). So the names
// and signatures of those methods are an API across the two repositories:
// renaming one, or changing its signature, breaks the integrations build.
// RecordOrphanWindow itself no-ops on a zero start and a nil manager.
//
// Reader/writer symmetry: the suppressor (routes/record.go) must query
// WasMockOrphanedInWindow on the SAME manager this writes to. In OSS that always
// holds — nothing calls syncMock.NewContext, so s.Mgr is always nil and both the
// write here and the read there resolve to syncMock.Get(). A multi-app composer
// that sets a per-app s.Mgr must likewise give its suppressor a per-app reader;
// there is no global fan-out for orphan windows (unlike memory pressure).
func (s *Session) RecordOrphanWindow(start, end time.Time) {
	if s == nil {
		return
	}
	if s.Orphans != nil {
		s.Orphans.Record(start, end)
		return
	}
	s.manager().RecordOrphanWindow(start, end)
}

// manager is the SyncMockManager this session's mocks, counts and spans go to:
// Mgr, the per-app manager a multi-app caller sets, or the package-global one
// (syncMock.Get) when Mgr is nil, the single-session default. It is the one
// rule for that choice: every use of the session's manager goes through it, so
// a mock, its left-out count (ReportLeftOut) and its span (RecordOrphanWindow,
// OpenOrphanWindow) cannot reach different managers. Never nil.
func (s *Session) manager() *syncMock.SyncMockManager {
	if s.Mgr != nil {
		return s.Mgr
	}
	return syncMock.Get()
}

// OrphanSpans is where a Session's unrecordable spans go instead of its manager
// (Session.Orphans). *syncMock.Spans is one.
type OrphanSpans interface {
	Record(start, end time.Time)
	Open(start time.Time) func()
}

// OpenOrphanWindow is the open-ended twin of RecordOrphanWindow, for a hole
// whose end is not yet known. It routes the same way (Orphans when set, else
// Session.manager) and returns the closer; see
// SyncMockManager.OpenOrphanWindow.
//
// The caller is the V2 dispatcher on a passthrough fallthrough: once a parser
// is retired the relay raw-forwards the rest of that connection, so it emits no
// further mock and every test case recorded against it would otherwise ship
// mock-less and replay as match_phase=no_mocks.
func (s *Session) OpenOrphanWindow(start time.Time) func() {
	if s == nil {
		return func() {}
	}
	if s.Orphans != nil {
		return s.Orphans.Open(start)
	}
	return s.manager().OpenOrphanWindow(start)
}

// EmitMock sends m to the mocks channel. If the session's active mock
// is marked incomplete, EmitMock returns nil without sending (the mock
// is left out and the incomplete flag is cleared, matching the "partial
// mocks are dropped" invariant), and reports the mock's exchange as one
// the connection could not record (LeaveOutIfIncomplete), which leaves out
// the test cases recorded over it that the recording has not saved yet
// (ReportLeftOut says when one may have been). A caller that keeps its own
// spans (Session.Orphans: a DaemonSet agent, per pod) checks that pod's test
// cases against them instead.
//
// Context cancellation semantics differ between the two delivery paths:
//
//   - Direct-channel path (RouteMocksViaSyncMock=false, s.Mocks bound):
//     EmitMock selects between `s.Mocks <- m` and `<-ctx.Done()`. While
//     the send is not yet ready, cancellation causes EmitMock to return
//     ctx.Err(). If ctx is ALREADY cancelled AND the send is also ready
//     (e.g. s.Mocks is buffered with spare capacity), Go's select is
//     free to pick either case — the outcome is non-deterministic:
//     EmitMock may emit the mock and return nil, OR it may pick the
//     ctx.Done() arm and return ctx.Err() without emitting. Once the
//     send case wins, delivery is guaranteed; callers that want a
//     strict "no emit past cancel" barrier must probe ctx.Err()
//     themselves before calling EmitMock.
//
//   - SyncMock path (RouteMocksViaSyncMock=true): ctx is checked ONCE
//     before calling mgr.AddMock. If ctx was already cancelled,
//     EmitMock returns ctx.Err() without touching the manager. If ctx
//     cancels AFTER the AddMock call starts, AddMock runs to completion
//     (buffers the mock, or blocks in its own bounded sendToOutChan
//     up to sendBudget) and EmitMock returns nil. AddMock may also
//     drop the mock under backpressure/memory-pause without signalling
//     the caller — the drop is recorded on the manager's drop counter
//     instead. This is a deliberate asymmetry: the syncMock manager
//     owns its own lifecycle so best-effort delivery with ctx-probe
//     on entry is the cleanest fit; forcing ctx through would
//     require threading it into every AddMock call site in the legacy
//     record path too.
//
// Caller's post-record hook chain (OnMockRecorded) runs synchronously
// before either delivery branch so wrappers can annotate.
//
// It is safe to call EmitMock with a nil session (returns nil); this
// matches the defensive shape of RecordSession call sites. A nil m is
// a programming error and treated as a no-op returning nil.
func (s *Session) EmitMock(m *models.Mock) error {
	return s.emitMockCore(m, false)
}

// EmitMockOnShutdown is the shutdown-time variant of EmitMock used by
// parsers that have just reconstructed an in-flight invocation from
// chunks the relay observed BEFORE ctx cancelled. The standard EmitMock
// short-circuits via the SyncMock-path ctx pre-check (and the direct-
// channel select-against-ctx) which would discard exactly the work
// the shutdown drain just recovered.
//
// Semantics relative to EmitMock:
//
//   - SyncMock path: skip the s.Ctx pre-check and call mgr.AddMock
//     unconditionally. AddMock owns its own lifecycle (memory-pause,
//     drop accounting, sendBudget) and remains the right place to
//     enforce shutdown-time backpressure.
//
//   - Direct-channel path: replace the select-against-ctx with a
//     non-blocking best-effort send. If the channel is full at
//     shutdown the mock is dropped (the alternative — blocking on a
//     channel whose consumer is itself shutting down — would deadlock
//     the parser). Callers that need stricter delivery semantics
//     during shutdown should serialise through the SyncMock manager.
//
// All other invariants (incomplete-mock gate, ClientConnID
// propagation, ReqTimestampMock monotonicity, OnMockRecorded hook
// chain, OnPendingCleared) match EmitMock byte-for-byte.
func (s *Session) EmitMockOnShutdown(m *models.Mock) error {
	return s.emitMockCore(m, true)
}

func (s *Session) emitMockCore(m *models.Mock, shutdown bool) error {
	if s == nil || m == nil {
		return nil
	}
	if s.LeaveOutIfIncomplete(m) {
		// Left out, and the flag reset, so the next mock in this cycle
		// gets a fresh chance. Matches invariant I4 in PLAN.md.
		//
		// Pending work is still cleared: the parser has consumed the
		// input even though the mock is abandoned. Leaving it armed
		// would make the hang watchdog fire once the connection goes
		// idle, a spurious abort after a benign drop (memory pressure,
		// chunk gate, short write).
		return nil
	}

	// Propagate the session's ClientConnID onto the mock when the
	// parser didn't already set it. This matches the documented
	// contract on Session.ClientConnID and removes a common source
	// of per-parser boilerplate. Parsers that want to override (e.g.
	// a wrapper tagging a composite connection ID) can still assign
	// m.ConnectionID before calling EmitMock.
	if m.ConnectionID == "" && s.ClientConnID != "" {
		m.ConnectionID = s.ClientConnID
	}

	// Enforce per-session ReqTimestampMock monotonicity (I5). In
	// debug builds, a regression panics the test so parser bugs
	// surface immediately; in prod the timestamp is clamped up to
	// lastReq + 1ns so the matcher's ordering invariant still holds.
	// The clamp is strictly within the same connection; sessions
	// across connections are independent.
	s.enforceReqMonotonic(m)

	s.hookMu.Lock()
	hook := s.OnMockRecorded
	s.hookMu.Unlock()
	if hook != nil {
		hook(m)
	}

	// Route through the session's SyncMockManager (Session.manager)
	// when the caller opts in. Legacy parsers (http, mysql, generic, etc.)
	// call (*SyncMockManager).AddMock — obtained via syncMock.Get() —
	// because it does:
	//
	//   1. Lifetime derivation from Metadata["type"] (session vs
	//      per-test), stamped onto TestModeInfo.Lifetime.
	//   2. firstReqSeen buffering — mocks captured BEFORE the first
	//      app test request are treated as "session" scope and
	//      re-delivered on every replay; mocks after belong to the
	//      currently-active test window. Dispatchers that skip this
	//      will emit per-test mocks even for startup-phase traffic,
	//      and replay against a recording loses them because the
	//      session window is empty.
	//   3. Memory-pause gating and drop counters.
	//
	// Tests that wire a bare s.Mocks channel and want to observe
	// emitted mocks directly leave RouteMocksViaSyncMock false so
	// the direct-channel fallback below runs. Production
	// recordViaSupervisor sets it true.
	if s.RouteMocksViaSyncMock {
		mgr := s.manager()
		// Best-effort ctx probe. mgr.AddMock doesn't observe
		// s.Ctx — it takes its own mutex and may sit
		// in sendToOutChan up to the manager's internal
		// sendBudget without signalling cancellation back to
		// us. A pre-check at least catches the "already
		// cancelled" case so we don't spend mgr.AddMock's
		// bounded wait on work the parser already abandoned.
		// Post-call cancellation during mgr.AddMock's send is
		// documented as a semantic difference from the
		// direct-channel branch (see the EmitMock doc comment
		// above); forcing full ctx-honoring into AddMock would
		// require threading ctx through every legacy
		// record-path call site too.
		//
		// Shutdown variant SKIPS this pre-check: the parser has
		// just reconstructed an invocation from chunks captured
		// before ctx cancelled, and discarding that work because
		// ctx is now done would lose the very last mock on a
		// connection in flight at SIGINT.
		if !shutdown {
			if ctx := s.Ctx; ctx != nil {
				if err := ctx.Err(); err != nil {
					return err
				}
			}
		}
		mgr.AddMock(m)
		if s.OnPendingCleared != nil {
			s.OnPendingCleared()
		}
		return nil
	}

	if s.Mocks == nil {
		if s.OnPendingCleared != nil {
			s.OnPendingCleared()
		}
		return nil
	}
	ctx := s.Ctx
	if ctx == nil {
		// A session without a bound ctx can still send; we just
		// don't gate on cancellation.
		s.Mocks <- m
		if s.OnPendingCleared != nil {
			s.OnPendingCleared()
		}
		return nil
	}
	if shutdown {
		// Non-blocking send: at shutdown the consumer is also
		// winding down so a blocked send would deadlock the
		// parser. Drop on full channel; callers that want stricter
		// delivery during shutdown should route via SyncMock.
		select {
		case s.Mocks <- m:
			if s.OnPendingCleared != nil {
				s.OnPendingCleared()
			}
			return nil
		default:
			return nil
		}
	}
	select {
	case s.Mocks <- m:
		if s.OnPendingCleared != nil {
			s.OnPendingCleared()
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// leftOutWarnEvery is how often ReportLeftOut's WARN may be logged for each
// cause (leftOutCause).
const leftOutWarnEvery = 10 * time.Second

// leftOutWarns limits ReportLeftOut's WARN, process-wide, keyed by
// leftOutCause, or past maxOpenKinds causes by leftOutClass.
var leftOutWarns = NewWarnLimiters(leftOutWarnEvery)

// ResetLeftOutWarningsForTest forgets every left-out WARN let through and
// held back so far, so a test in another package (a parser's) can count the
// ones it causes: the limit is process-wide. Production code never calls it.
func ResetLeftOutWarningsForTest() { leftOutWarns.Reset() }

// ReportLeftOut reports m as a mock left out of the recording, for reason: an
// exchange its connection could not record. It has five callers, and each
// emits nothing for the exchange it reports:
//   - LeaveOutIfIncomplete (and so EmitMock), for the mock it leaves out for
//     the incomplete-mock flag, with the reason that set the flag;
//   - a parser that takes the flag itself (TakeMockIncomplete), once for each
//     exchange it leaves out for the mark, with the reason it took;
//   - a parser that knows which exchange it cannot record, with its own
//     reason, instead of marking the flag (MarkMockIncomplete);
//   - ReportStoppedOn, for the exchange a parser stops on as it returns;
//   - NextRequest's floor (dropped), for the server bytes at the start of a
//     connection the capture joined mid-way that answer a request the
//     capture does not have.
//
// Each reports its exchange whenever the parser gets to it, after the
// recording stopped too: its bytes were captured, and it is not recorded.
// RecordingStopping decides only what follows a stop (the span after it, and
// the WARNs that say every later test case is left out), never whether such
// an exchange is reported.
//
// m needs only the mock's Kind and its exchange's Spec.ReqTimestampMock and
// Spec.ResTimestampMock: a minimal mock is enough. It does not touch the flag:
// a mark set since it was taken, or by the relay meanwhile, is the next
// mock's.
//
// m's exchange, [ReqTimestampMock, ResTimestampMock], is one the connection
// could not record. It goes where the session's unrecordable spans go
// (RecordOrphanWindow), and the recording leaves out each test case over it
// that it checks from then on. A test case is checked when it is saved: in
// proxy mode as routes/record.go streams it; with a watermark
// (syncMock.Watermark), once its verdict is final, or when the hold's bound
// lets it go. The span is reported when the parser gets to the exchange, and
// a parser runs behind the traffic (in proxy mode, by up to its connection's
// capture buffer): by a full buffer when the reason is per_conn_cap, and the
// mock the flag leaves out is then the next one it parses, which can be from
// an exchange before the chunk the relay lost. So a test case recorded over
// the exchange can have been saved already: it lacks the mock, and its replay
// fails with no_mocks. The WARN says so, and must not promise more. A caller
// that keeps its own spans (Session.Orphans: a DaemonSet agent, per pod)
// checks that pod's test cases against them instead.
//
// The mock is counted on the manager its session's mocks go to
// (Session.manager, SyncMockManager.NoteMockLeftOut), for the recording's
// summary, and the session's pending work is cleared (OnPendingCleared), as an
// emitted mock clears it: the parser has consumed the input, and work left
// pending through the next idle spell is taken for a hung parser. It is logged:
// at WARN once per leftOutWarnEvery for each cause (leftOutCause),
// process-wide, with why the flag was set, the setting that fits its class
// (leftOutNextStep), and how many of its cause were held back since the last
// such WARN (sameWarningsHeldBack); each one held back at Debug. Each is a mock
// missing from the recording, so no cause of them may be silent, nor wait
// behind another cause's WARN; and a connection that loses a chunk per request
// must not log a line per request.
//
// The span is the mock's own times, one rule: [ReqTimestampMock,
// ResTimestampMock], ending where it starts when the mock has no response
// time or one before its request's. A mock with no request time gets no span:
// no time is invented for it. The moment it is reported is not its
// exchange's (the parser runs behind the traffic, by up to a full capture
// buffer), so a span there would leave out test cases in flight then, which
// did not use the exchange, and miss the ones that did. Such a mock is still
// counted and logged, with spanRecorded false, and its next_step says that no
// test case was left out for it. So a span handed to RecordOrphanWindow, and
// so to a Session.Orphans the caller implements, always has a start and never
// ends before it. A nil m is reported as a mock with no times: the flag it was
// left out for has been taken, so it must still be counted. Nil-safe.
func (s *Session) ReportLeftOut(m *models.Mock, reason string) {
	if s == nil {
		return
	}
	if m == nil {
		m = &models.Mock{}
	}
	start, end := m.Spec.ReqTimestampMock, m.Spec.ResTimestampMock
	spanned := !start.IsZero()
	if spanned {
		if end.Before(start) {
			end = start
		}
		s.RecordOrphanWindow(start, end)
	}
	s.manager().NoteMockLeftOut()
	if s.OnPendingCleared != nil {
		s.OnPendingCleared()
	}
	if s.Logger == nil {
		return
	}
	fields := []zap.Field{zap.String("connID", s.ClientConnID), zap.String("mockName", m.Name),
		zap.String("kind", string(m.Kind)), zap.String("reason", reason), zap.Bool("spanRecorded", spanned)}
	cause := leftOutCause(reason)
	class := leftOutClass(cause)
	ok, held, _ := leftOutWarns.AllowOr(cause, class)
	if !ok {
		s.Logger.Debug(leftOutDebugMsg, fields...)
		return
	}
	if held > 0 {
		fields = append(fields, zap.Uint64("sameWarningsHeldBack", held))
	}
	s.Logger.Warn(leftOutWarnMsg, append(fields, zap.String("next_step", leftOutNextStep(class, spanned)))...)
}

// ReportStoppedOn reports the exchange a parser stops on, as it returns
// without recording it, as a mock left out (ReportLeftOut): the exchange of
// kind whose request reached the parser at reqTs, for reason. It is the one
// rule for that event, whichever parser stops: the HTTP/1 recorder on a request
// or response that does not decode, the MySQL recorder on a command in whose
// exchange it stops recording the connection (lost framing of the client's
// stream, or a response it cannot frame and cannot take the connection up
// again after, as from a client that pipelines its commands; a response it
// cannot frame otherwise costs that exchange alone, which it reports with
// ReportLeftOut, and its recording goes on). So the count, the span and the
// WARN do not depend on which parser stopped.
//
//   - Its span runs from its request to the stop: the session's stop instant
//     (StoppedAt), or reqTs when that reads earlier than a chunk's stamp
//     (ReportLeftOut ends a span no earlier than it starts). Not to the last
//     byte the parser read of the exchange: a parser runs behind its
//     connection, and the bytes the connection carried between that one and
//     the stop are in its capture buffer, unread, and lost with it. The span
//     of what the connection carries after the stop starts at the same
//     instant, and is open from it (OnStop): from this call, before it
//     reports the exchange and before the parser logs its error and
//     returns, unless the parser's retirement or a capture hole came first
//     and stamped the stop then. So the two spans meet: no test case
//     recorded between them is saved without its mocks, and one checked
//     after the stop overlaps the span after it.
//   - A mark on the incomplete-mock flag is taken with it. The parser returns,
//     so no later EmitMock takes the mark, and nothing would report it. A mark
//     is a chunk the relay lost on the connection, the likelier cause of what
//     the parser could not read, so the exchange is reported under the mark's
//     cause (the text before the first colon keys the WARN's limit and picks
//     its next_step), with reason after it.
//
// A zero reqTs is reported with no span, as ReportLeftOut reports any mock with
// no request time. The connection's stop is logged apart from it: by the
// dispatcher (its "parser retired" WARN), or by the parser, when the error it
// returns wraps ErrReported. Neither the end of a stream nor a recording's
// stop (ctx done) is an exchange stopped on: a parser that returns on either
// reports nothing. Nil-safe.
func (s *Session) ReportStoppedOn(kind models.Kind, reqTs time.Time, reason string) {
	if s == nil {
		return
	}
	stop := s.StoppedAt()
	if mark, ok := s.TakeMockIncomplete(); ok {
		reason = mark + ": " + reason
	}
	s.ReportLeftOut(&models.Mock{
		Kind: kind,
		Spec: models.MockSpec{ReqTimestampMock: reqTs, ResTimestampMock: stop},
	}, reason)
}

// StoppedAt is the instant this connection stopped being recorded: the first of
// the parser's stop on an exchange, its death or retirement, and a capture
// hole. It is the clock, read once, at the first call, and that same instant
// at every call after it, from any goroutine; the first call tells OnStop
// before it returns. Each event calls it as it happens, before it logs
// anything: the parser as it reports the exchange it stops on
// (ReportStoppedOn, whose span ends at it); the supervisor as it learns that
// the parser panicked or returned with an error (Supervisor.Run); the
// dispatcher's abort as it pauses the capture of a parser it retires that is
// still running, hung or cancelled (before the pause, so every chunk the pause
// drops is after it); and the relay's callback for a hole in the capture
// (OnCaptureDesync). The span of what the connection carries after the stop
// starts at it, and OnStop opens it there and then. Each event read the clock
// itself before, and the span after the stop was opened only once the parser
// had returned and its retirement was logged: the spans were apart, by the
// time the parser took to return and the dispatcher to log its retirement (or
// the supervisor a panic, with its stack), or by the time the connection took
// to fill the capture queue of a parser that had stopped reading, up to a
// hole, and a test case checked before the span opened overlapped none. The
// traffic in between, more of it the more starved the agent, was in no span:
// a test case recorded over it was saved without its mocks. A later event gets
// the earlier instant: an exchange a parser reports after its retirement (one
// that hung, say) is spanned up to the retirement, and a hole after the
// parser's stop opens its span at the stop. A nil Session's is the zero time.
//
// What a parser had captured and not yet recorded when it stops was captured
// before the stop, and is lost with it: the exchange it was in, and every
// exchange queued behind it in its capture buffer. A parser that reports the
// exchange (ReportStoppedOn) spans it from its request to the stop, which
// covers those behind it too. One that stops without reporting it (it
// panics, hangs, or returns an error without ReportStoppedOn) leaves them in
// no span and uncounted: a test case over them that ended before the stop, or
// was checked before it, is saved without its mocks.
func (s *Session) StoppedAt() time.Time {
	if s == nil {
		return time.Time{}
	}
	if n := s.stoppedAt.Load(); n != 0 {
		return time.Unix(0, n)
	}
	now := time.Now().UnixNano()
	if !s.stoppedAt.CompareAndSwap(0, now) {
		return time.Unix(0, s.stoppedAt.Load())
	}
	at := time.Unix(0, now)
	if s.OnStop != nil {
		s.OnStop(at)
	}
	return at
}

// RecordingStopping reports whether the recording this connection is captured
// for is stopping: RecordingDone is closed. A stop of the connection's
// recording then (StoppedAt), whatever stamps it and however the parser ends,
// is the end of the connection, not a loss: the connection is torn down with
// the recording, and nothing it would carry is left to lose. It is the one
// rule for every consequence of such a stop, each read as it happens, not
// where the stop was armed: whether a span of what the connection carries
// after the stop opens (the open func the dispatcher gives its
// syncMock.UnrecordedConn), and whether a WARN that says every test case
// recorded from then on is left out goes out. Those are the relay's for a
// capture hole (OnCaptureDesync answers whether it costs the recording
// anything), the dispatcher's "parser retired", and the one a parser that
// reports its own stop (ErrReported) logs in its place: the MySQL recorder's
// for lost framing. A recording that has stopped does not resume. A nil
// Session's is false.
func (s *Session) RecordingStopping() bool {
	if s == nil {
		return false
	}
	select {
	case <-s.RecordingDone:
		return true
	default:
		return false
	}
}

// ReportLeftOut's log lines: at WARN, and at Debug for one the limit holds
// back. Their reason field says why the exchange was not recorded: the
// flag's, or the parser's own.
const (
	leftOutWarnMsg  = "a captured exchange was not recorded as a mock: its capture was incomplete or could not be decoded"
	leftOutDebugMsg = "left out a mock: its capture was incomplete or could not be decoded"
)

// maxLeftOutCause is the longest cause leftOutCause keeps of a reason.
const maxLeftOutCause = 64

// leftOutCause is the cause a reason the incomplete-mock flag was set for
// names, which keys ReportLeftOut's WARN limit: the reason up to its first
// colon, trimmed, and at most maxLeftOutCause bytes. The relay's reasons are
// fixed tokens (memory_pressure, per_conn_cap, desynced, short_write,
// client_hold_cap), so each is a cause of its own. A parser's reason can carry
// an error's text after a colon ("http decode error: " + err.Error()), which
// the cause leaves off: one error must not get a WARN per text it carries. An
// empty reason is of class other.
func leftOutCause(reason string) string {
	if i := strings.IndexByte(reason, ':'); i >= 0 {
		reason = reason[:i]
	}
	reason = strings.TrimSpace(reason)
	if len(reason) > maxLeftOutCause {
		reason = reason[:maxLeftOutCause]
	}
	if reason == "" {
		return leftOutOther
	}
	return reason
}

// The classes of cause a mock is left out for, each with its own next_step
// (leftOutNextStep). The first three are the relay's drop reasons
// (relay.DropMemoryPressure, relay.DropPerConnCap, relay.DropDesynced), which
// it hands MarkMockIncomplete verbatim; the fourth is NextRequest's. A class
// also keys the WARN limit for a cause past the first maxOpenKinds
// (WarnLimiters.AllowOr).
const (
	leftOutMemoryPressure = "memory_pressure"
	leftOutPerConnCap     = "per_conn_cap"
	// leftOutDesynced is the consequence of one of the two above, not a
	// cause of its own: a direction that lost a chunk to either, whose
	// parser cannot resync, is no longer fed to its parser, and the relay
	// refuses each later chunk on it with this reason. Each refusal marks
	// the flag, and the parser goes on emitting mocks from what it captured
	// before the loss: the next one is left out for it. Its knob is the
	// loss's, which a re-run with --debug does not show.
	leftOutDesynced = "desynced"
	// leftOutUnansweredCause is the cause of the server bytes the floor drops
	// where the server stream of a connection the capture joined mid-way
	// (JoinedMidConnection) starts, before its first request (NextRequest):
	// the answer to a request the capture does not have. Nothing about the
	// recording's settings caused it.
	leftOutUnansweredCause = "server bytes that answer no captured request"
	leftOutOther           = "other"
)

// leftOutClass is the class of a cause (leftOutCause) the incomplete-mock
// flag was set for: the knob that fits it.
func leftOutClass(cause string) string {
	switch cause {
	case leftOutMemoryPressure, leftOutPerConnCap, leftOutDesynced, leftOutUnansweredCause:
		return cause
	}
	return leftOutOther
}

// leftOutNextStep is ReportLeftOut's next_step for a mock left out for a
// cause of class (leftOutClass), whose span was recorded or not (spanned):
// what was and was not lost, and the knob that fits the class. The WARN is
// rate-limited per cause, so the reason it carries is one mock's; --debug
// shows each.
//
// It says only what holds of the test cases over the exchange. With its span
// recorded, the ones not saved yet are left out, and one saved before the
// parser got to the exchange lacks the mock. With none (a mock with no request
// time), none is left out for it, and each one lacks the mock (ReportLeftOut).
func leftOutNextStep(class string, spanned bool) string {
	next := "the app's traffic is unaffected. Test cases recorded over this exchange that were not saved yet when the agent " +
		"parsed it are left out of the recording; one saved before then (this connection's parser was behind the " +
		"traffic) lacks this mock, and its replay fails with no_mocks. "
	if !spanned {
		next = "the app's traffic is unaffected. This mock carries no request time, so no span was recorded for it and no " +
			"test case is left out for it: each test case recorded over this exchange lacks this mock, and its replay " +
			"fails with no_mocks. "
	}
	switch class {
	case leftOutMemoryPressure:
		return next + "reason=memory_pressure: the agent ran short of memory and dropped capture; give it more memory " +
			"(raise --memory-limit), or lower the recorded traffic (enable sampling, reduce request concurrency)"
	case leftOutPerConnCap:
		return next + "reason=per_conn_cap: this connection's capture buffer was full, so its parser was a full buffer " +
			"behind the traffic, and the mock left out is the next one it parsed, which can be from an exchange before " +
			"the lost chunk: test cases over it may have been saved already. Raise " +
			"record.recordBuffer.maxMemoryPerConnection (env KEPLOY_RECORD_MAX_MEMORY_PER_CONN)"
	case leftOutDesynced:
		return next + "reason=desynced: this connection lost a chunk earlier, to per_conn_cap or memory_pressure, and its " +
			"parser cannot re-align after a hole, so its capture stopped there in that direction: every later chunk in it " +
			"is refused, and each mock its parser still emits after a refused chunk is left out, including ones from " +
			"exchanges captured whole before the loss. The setting to change is that loss's (its left-out WARN and the " +
			"relay's \"capture dropped a chunk\" WARN carry reason=per_conn_cap or reason=memory_pressure): for " +
			"per_conn_cap, raise record.recordBuffer.maxMemoryPerConnection (env KEPLOY_RECORD_MAX_MEMORY_PER_CONN); for " +
			"memory_pressure, give the agent more memory (raise --memory-limit)"
	case leftOutUnansweredCause:
		return next + "The request these bytes answer was in flight when the capture of this already open connection " +
			"began (the recording started, a TLS hook attached, or the agent restarted, while it was sent), so it was never " +
			"captured: there is no setting to change, and the connection's later exchanges are recorded each with its own answer"
	}
	return next + "Re-run with --debug to see each mock left out and why it could not be recorded"
}

// enforceReqMonotonic clamps m.Spec.ReqTimestampMock to at least
// s.lastReqTimestamp + 1ns when a regression is detected, and
// updates the session's last-seen timestamp. In debug builds we
// panic instead so regressions surface during testing. Thread-safe:
// holds lastReqMu while comparing + writing back.
//
// Zero-valued ReqTimestampMock values pass through untouched
// (parsers that haven't populated them, e.g. tests with pre-built
// minimal mocks, shouldn't trigger the clamp).
func (s *Session) enforceReqMonotonic(m *models.Mock) {
	req := m.Spec.ReqTimestampMock
	if req.IsZero() {
		return
	}
	s.lastReqMu.Lock()
	defer s.lastReqMu.Unlock()
	if s.lastReqTimestamp.IsZero() {
		s.lastReqTimestamp = req
		return
	}
	if req.Before(s.lastReqTimestamp) {
		clamped := s.lastReqTimestamp.Add(time.Nanosecond)
		if debugMonotonic.Load() {
			panic("supervisor.Session.EmitMock: out-of-order ReqTimestampMock detected; parser emitted a mock with a timestamp earlier than a previously-emitted mock on the same session — this violates I5 in PLAN.md and would cause wrong-mock selection at replay time")
		}
		m.Spec.ReqTimestampMock = clamped

		// Raising the request stamp can push it PAST the response stamp on a mock
		// that arrived perfectly well-ordered. filterByTimeStamp drops any mock
		// with res < req (pkg/util.go), so that mock is then silently discarded at
		// replay -- orphaned by this function, not by the recorder. Measured: a
		// pair (req 17.820597769, res 17.607310086) leaves EmitMock inverted by
		// 79.4ms.
		//
		// This REPORTS it and does not repair it, deliberately. Two attempts to
		// repair it by adjusting ResTimestampMock both regressed
		// go-memory-load-mongo, and the second one narrowly:
		//
		//   - clamping every inverted window resurrected mocks the filter had been
		//     discarding, and they were consumed ahead of the real ones: the lane
		//     went from green to 52 "no matching mock" failures.
		//   - carrying the response stamp along ONLY for the inversion introduced
		//     here still broke record_build_replay_latest with candidates=0, while
		//     record_build_replay_build passed. Same recording, different replay
		//     binary, different outcome -- i.e. it produced recordings the
		//     RELEASED replayer cannot consume. Those lanes exist to catch exactly
		//     that.
		//
		// So ResTimestampMock is load-bearing at replay in ways this call site
		// cannot see, and mutating it here is the wrong lever. The real repair
		// belongs where the ordering invariant and the window filter are designed
		// together. Until then this makes the loss visible instead of silent,
		// which is what was actually missing: the first instance was found only by
		// diffing recorded YAML by hand.
		if s.Logger != nil && !m.Spec.ResTimestampMock.IsZero() && m.Spec.ResTimestampMock.Before(clamped) {
			s.Logger.Warn("monotonic clamping inverted this mock's window; replay will DROP it (filterByTimeStamp discards res < req)",
				zap.String("mock", m.Name),
				zap.Duration("inversion", clamped.Sub(m.Spec.ResTimestampMock)),
				zap.Time("reqTimestampMock", clamped),
				zap.Time("resTimestampMock", m.Spec.ResTimestampMock),
				zap.Time("originalReqTimestampMock", req))
		}
		req = clamped
	}
	s.lastReqTimestamp = req
}

// debugMonotonic, when true, causes enforceReqMonotonic to panic on
// an out-of-order emission instead of clamping — surfacing parser
// bugs loudly in test binaries. Production builds leave this false
// so a timestamp regression silently clamps rather than crashing
// the agent; the clamp preserves matcher correctness either way.
// Tests that want strict checking set it via SetDebugMonotonic.
var debugMonotonic atomic.Bool

// SetDebugMonotonic toggles debug-build monotonicity enforcement.
// When enabled, EmitMock panics on an out-of-order ReqTimestampMock
// instead of clamping. Intended for test binaries; production should
// leave it disabled.
func SetDebugMonotonic(v bool) { debugMonotonic.Store(v) }
