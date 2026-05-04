package supervisor

import (
	"context"
	"net"
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

	// --- Internal bookkeeping ---

	mockIncomplete   atomic.Bool
	hookMu           sync.Mutex
	lastReqMu        sync.Mutex
	lastReqTimestamp time.Time // most recent ReqTimestampMock emitted on this session
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

// MarkMockIncomplete sets the session's incomplete-mock flag. EmitMock
// drops silently while the flag is set. Reason is logged at Debug so
// operators can trace why a mock was withheld (memory pressure, channel
// full, dropped chunk, parser-internal inconsistency).
//
// The relay calls this when it gates a chunk at the tee; parsers call
// it when they cannot continue decoding a mock cleanly. Safe to call
// repeatedly — subsequent calls are no-ops until MarkMockComplete.
func (s *Session) MarkMockIncomplete(reason string) {
	if s == nil {
		return
	}
	if !s.mockIncomplete.Swap(true) && s.Logger != nil {
		s.Logger.Warn("mock marked incomplete", zap.String("reason", reason), zap.String("connID", s.ClientConnID))
	}
}

// MarkMockComplete clears the incomplete-mock flag. Parsers call it
// when they have finished a mock cycle (typically right after EmitMock
// or when sending directive.FinalizeMock). Idempotent.
func (s *Session) MarkMockComplete() {
	if s == nil {
		return
	}
	s.mockIncomplete.Store(false)
}

// IsMockIncomplete reports whether the session's active mock has been
// marked incomplete. Parsers may use this to short-circuit expensive
// encoding work they know will be dropped.
func (s *Session) IsMockIncomplete() bool {
	if s == nil {
		return false
	}
	return s.mockIncomplete.Load()
}

// EmitMock sends m to the mocks channel. If the session's active mock
// is marked incomplete, EmitMock returns nil without sending (the mock
// is dropped on the floor and the incomplete flag is cleared, matching
// the "partial mocks are dropped" invariant).
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
// All other invariants (mockIncomplete gate, ClientConnID
// propagation, ReqTimestampMock monotonicity, OnMockRecorded hook
// chain, OnPendingCleared) match EmitMock byte-for-byte.
func (s *Session) EmitMockOnShutdown(m *models.Mock) error {
	return s.emitMockCore(m, true)
}

func (s *Session) emitMockCore(m *models.Mock, shutdown bool) error {
	if s == nil || m == nil {
		return nil
	}
	if s.mockIncomplete.Load() {
		// Drop silently and reset so the next mock in this cycle gets
		// a fresh chance. Matches invariant I4 in PLAN.md.
		//
		// Still clear pending work — the parser has consumed the input
		// even though the mock is being abandoned. Leaving pending
		// armed would cause the hang watchdog to fire after the
		// connection goes idle, producing spurious aborts after a
		// benign drop (memory pressure, chunk gate, short write).
		s.mockIncomplete.Store(false)
		if s.Logger != nil {
			s.Logger.Warn("emitMockCore: dropped mock due to mockIncomplete flag",
				zap.String("connID", s.ClientConnID),
				zap.String("mockName", m.Name),
			)
		}
		if s.OnPendingCleared != nil {
			s.OnPendingCleared()
		}
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

	// Route through the package-singleton SyncMockManager when the
	// caller opts in. Legacy parsers (http, mysql, generic, etc.)
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
		if mgr := syncMock.Get(); mgr != nil {
			// Best-effort ctx probe. mgr.AddMock (the SyncMock
			// manager method obtained from syncMock.Get()) doesn't
			// observe s.Ctx — it takes its own mutex and may sit
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
