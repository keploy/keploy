package models

import (
	"os"
	"strings"
	"sync/atomic"
)

// Lifetime classifies how long a mock's usefulness extends and how the
// matcher should treat it. This is the single in-code source of truth for
// "is this mock per-test, reusable across tests, or scoped to one
// connection" — everything else (disk-layer kind-switch, Metadata["type"]
// reads inside matchers, IsConfigMock/IsReusable helpers) should eventually
// derive from this field.
//
// Lifetime is cached on TestModeInfo (not on Spec) so it stays a runtime
// concept and never touches the YAML wire format. Recorders continue to
// write Spec.Metadata["type"] verbatim; Lifetime is computed from that tag
// once at ingest and read at every match site.
//
// Migration contract (see docs/explanation/mock-lifetimes.md):
//
//   - zero value == LifetimePerTest is the correct default; a mock with no
//     tag and no kind-level legacy routing is per-test.
//   - LifetimeSession corresponds to Spec.Metadata["type"] == "config"
//     (the on-disk format is unchanged).
//   - LifetimeConnection corresponds to Spec.Metadata["type"] == "connection"
//     and requires Spec.Metadata["connID"] to be set. It is introduced to
//     make prepared-statement setup mocks (Postgres Parse, MySQL
//     COM_STMT_PREPARE) replay correctly under strict-window mode without
//     leaking across connections.
//
// Performance: the enum is a uint8, fits in an existing TestModeInfo cache
// line. Every `mock.TestModeInfo.Lifetime == LifetimeSession` compare is
// one instruction — materially cheaper than the `Spec.Metadata["type"]`
// map probe it replaces at hot-path read sites.
type Lifetime uint8

const (
	// LifetimePerTest: consumed on match; request-timestamp must fall
	// inside the outer test's window under strict-window mode. This is
	// the zero value and the safe default for any untagged mock.
	LifetimePerTest Lifetime = iota

	// LifetimeSession: reusable across every test in the session. Never
	// window-filtered. Produced by recorders for handshake/auth/SCRAM/
	// reflection/heartbeat traffic and session-affecting SQL like
	// SET/SHOW/RESET/DEALLOCATE.
	LifetimeSession

	// LifetimeConnection: reusable for the lifetime of a single client
	// connection (identified by Spec.Metadata["connID"]). Introduced to
	// let prepared-statement setup (Postgres Parse, MySQL
	// COM_STMT_PREPARE) replay correctly under strict-window mode —
	// the setup is neither per-test (executes reference it across test
	// boundaries) nor truly session (two connections may prepare
	// different SQL under colliding statement names). NOT window-
	// filtered; visibility is bounded by the connection's lifetime.
	LifetimeConnection
)

// String returns a human-readable label suitable for logs and telemetry.
func (l Lifetime) String() string {
	switch l {
	case LifetimeSession:
		return "session"
	case LifetimeConnection:
		return "connection"
	default:
		return "per-test"
	}
}

// DeriveLifetime resolves a mock's runtime Lifetime from its on-disk
// metadata tag, with a legacy-format fallback for recordings captured
// before the tag convention was universally applied.
//
// Idempotency: safe to call more than once on the same mock. The
// several ingest paths (disk loader, syncMock.AddMock, agent.StoreMocks)
// all call it defensively so no single site becomes load-bearing.
// Two guards make re-derivation side-effect-free:
//   - The LifetimeDerived bool short-circuits the entire function on
//     the first byte, preventing any reclassification work.
//   - The legacy-kind-fallback counter is only incremented when an
//     untagged mock (tag == "") actually falls through to the kind-
//     switch branch, so even if LifetimeDerived were bypassed the
//     counter can't double-count a mock that was already routed by
//     its Metadata["type"] tag.
//
// Precedence (applied top-to-bottom; first match wins):
//  1. Kind == MySQL AND first request is a connection-alive command
//     with an input-independent response (COM_PING, COM_STATISTICS,
//     COM_DEBUG, COM_RESET_CONNECTION) → LifetimeSession. Applied
//     BEFORE the tag switch so an explicitly-tagged "mocks" mock from
//     the recorder is still promoted to session when semantically
//     reusable — this is how HikariCP startup COM_PING mocks survive
//     strict-window pre-filtering.
//  2. Spec.Metadata["type"] == "config"       → LifetimeSession
//  3. Spec.Metadata["type"] == "connection"   → LifetimeConnection
//     (requires non-empty connID; falls back to Session if missing).
//  4. Untagged (tag == "") + kind in the legacy always-session list →
//     LifetimeSession. Increments legacyKindFallbackFires ONCE per
//     mock so the Phase-4 deletion gate can be measured accurately.
//  5. Lax-mode + non-canonical tag ("mocks" / "HTTP_CLIENT" / etc.) +
//     kind in the legacy always-session list → LifetimeSession.
//     Preserves pre-Phase-2 implicit reusability for data-plane mocks
//     under lax. Gated off when KEPLOY_STRICT_MOCK_WINDOW is set to an
//     enabling value so strict containment stays intact. Does NOT
//     increment legacyKindFallbackFires (the counter measures pre-tag
//     recordings, not tagged-but-lax-promoted ones).
//  6. Anything else                            → LifetimePerTest
//
// The kind-fallback preserves behaviour for recordings produced before
// recorders began tagging explicitly. The counter is the operator-
// visible signal (surfaced at replay completion); per-call logging is
// deliberately avoided because DeriveLifetime runs on every mock load.
func (m *Mock) DeriveLifetime() {
	// Idempotent short-circuit. LifetimeDerived is distinct from the
	// Lifetime field itself because LifetimePerTest IS the zero value
	// — without the bool, a PerTest classification is
	// indistinguishable from "never derived", and the heavy switch +
	// kind-fallback path would re-run on every ingest site (disk →
	// StoreMocks → syncMock). The bool also guards
	// legacyKindFallbackFires from double-counting when a mock passes
	// through multiple ingest paths.
	if m.TestModeInfo.LifetimeDerived {
		return
	}
	defer func() { m.TestModeInfo.LifetimeDerived = true }()

	// Protocol-specific override that runs BEFORE the tag-based
	// classification. A small allowlist of MySQL command-phase packet
	// types have input-independent responses (COM_PING → OK,
	// COM_STATISTICS → stats blob, COM_DEBUG → server-side no-op,
	// COM_RESET_CONNECTION → OK) and are typically recorded at app
	// startup (JDBC / HikariCP pool warm-up) BEFORE any test window
	// begins. Without this override they'd be tagged "mocks" (per-
	// test) by the recorder → strict-window pre-filter drops them →
	// replay fails at connection init with "no matching mock". The
	// promotion keeps the on-disk tag unchanged (backward compatible
	// with older replayers) but steers the in-memory routing so the
	// mock lands in the session pool here.
	//
	// Deliberately narrow: only the four commands whose response we
	// know is input-independent. COM_QUERY / COM_INIT_DB /
	// COM_CHANGE_USER / COM_SET_OPTION all depend on input and must
	// stay per-test.
	if m.Kind == MySQL && mysqlIsSessionReusableCommand(m) {
		m.TestModeInfo.Lifetime = LifetimeSession
		return
	}
	tag := ""
	if m.Spec.Metadata != nil {
		tag = m.Spec.Metadata["type"]
	}
	switch tag {
	case "config":
		m.TestModeInfo.Lifetime = LifetimeSession
		return
	case "connection":
		// LifetimeConnection requires a non-empty connID so the
		// per-connID pool lookup (GetConnectionMocks) has a stable
		// key. A mock tagged "connection" without a connID is
		// malformed — fall through to session semantics (still
		// reusable, just not connection-scoped) rather than
		// promoting it to per-test (which would be consumed on
		// first match and break replay for the paired execute).
		if m.Spec.Metadata["connID"] != "" {
			m.TestModeInfo.Lifetime = LifetimeConnection
			return
		}
		m.TestModeInfo.Lifetime = LifetimeSession
		return
	}
	// Kind-based fallback for UN-TAGGED mocks only. Pre-tag recordings
	// relied on the kind-switch in pkg/platform/yaml/mockdb/db.go to
	// route everything HTTP/HTTP2/MySQL/Postgres/PostgresV2/Generic
	// /DNS to the "config" pool. Emulate that here so the Lifetime field
	// has a defensible value for those recordings.
	//
	// CRITICAL: this fallback fires ONLY when tag == "" (no type
	// metadata present). Explicit non-empty tags that aren't
	// "config"/"connection" — e.g. the Postgres/MySQL recorders emit
	// "mocks" for per-test captures — are authoritative and must be
	// honoured as LifetimePerTest. Treating an explicit "mocks" tag as
	// a fallthrough to kind-fallback would wrongly promote per-test
	// mocks to LifetimeSession for the listed kinds, breaking the
	// entire per-test routing story.
	//
	// Observability: the LegacyKindFallbackFires counter tracks this
	// branch — it is surfaced at replay completion so operators can
	// tell whether pre-tag recordings are still in the wild. Non-zero
	// is EXPECTED for HTTP/Generic where recorders don't classify
	// lifetime at record time; it's a diagnostic, not an alarm. We
	// deliberately avoid per-call logging because DeriveLifetime runs
	// on every mock load.
	if tag == "" && kindsWithImplicitSessionLifetime(m.Kind) {
		m.TestModeInfo.Lifetime = LifetimeSession
		atomic.AddUint64(&legacyKindFallbackFires, 1)
		return
	}

	// Lax-mode preservation of pre-Phase-2 behaviour. Pre-my-PR
	// keploy had NO distinction between tag="" and tag="mocks" for
	// kind-fallback — everything MySQL/Postgres/Generic/... got
	// promoted to session regardless of on-disk tag. That made
	// data-plane mocks reusable across tests, which apps like the
	// fuzzer (1000+ queries per session) and ORM-based apps
	// (fixture-row re-reads from every test) depended on.
	//
	// Under strict mode we correctly tightened the gate to tag==""
	// — tagged "mocks" mocks are honoured as per-test and get
	// consumed on match inside their window. Under lax mode that
	// tightening has no upside (no window filter to evade) and
	// causes mock depletion in long-session apps.
	//
	// Mirror the pre-PR byte-for-byte: under lax, also apply the
	// kind-based session fallback for any non-canonical tag when
	// the kind is in the implicit-session list. Strict mode takes
	// the narrow path above and returns before reaching here.
	if !laxKindFallbackDisabled() && kindsWithImplicitSessionLifetime(m.Kind) {
		m.TestModeInfo.Lifetime = LifetimeSession
		return
	}
	m.TestModeInfo.Lifetime = LifetimePerTest
}

// laxKindFallbackDisabled reports whether strict mode is forcing the
// DeriveLifetime kind-fallback to stay narrow (tag=="" only). When
// this returns false (the Phase 1 default, matching
// config.Test.StrictMockWindow default), any non-canonical tag on a
// kind in kindsWithImplicitSessionLifetime also falls through to
// session — preserving pre-Phase-2 byte-for-byte behaviour under lax
// mode.
//
// Scope: this env-only gate controls ONE specific behaviour — the
// lax-mode promotion of explicitly-tagged non-canonical mocks (e.g.
// MySQL "mocks" tag on COM_INIT_DB) to LifetimeSession. It is
// intentionally NOT tied to config.Test.StrictMockWindow because:
//
//  1. pkg/models cannot import pkg (would create a cycle); reading the
//     env var is the only process-wide signal available here without
//     an upward dependency flip.
//  2. DeriveLifetime runs at disk-load time — often before the agent
//     has parsed keploy.yaml into runnable form — so a config value
//     isn't yet available at the call site.
//  3. The env var's enabling values (1/true/on/yes) already short-
//     circuit strictWindowEnabled() ahead of the per-call flag, so
//     honouring the env here is consistent with the process-wide
//     precedence declared in pkg/util.go's strictWindowEnabled.
//
// Users who enable strict mode via config alone (strictMockWindow:
// true, no env var) still get strict filtering at the agent-level
// filter (filterByTimeStamp / FilterPerTestAndLaxPromoted), which
// runs after config parse and honours StrictMockWindow. They do NOT
// additionally disable this disk-load lax-kind-fallback — which is a
// deliberate no-op: without the fallback, legitimately untagged
// pre-Phase-2 recordings would route to LifetimePerTest and be
// strict-filtered twice in a row, double-penalising users who ship
// strict=true before their recordings are re-captured under the new
// classification coverage. Set KEPLOY_STRICT_MOCK_WINDOW=1 in env to
// disable the fallback if you need the stricter behaviour from
// disk-load time onwards.
//
// The env parse is cached at package init (laxKindFallbackDisabledCache)
// since DeriveLifetime runs once per mock on large pools and re-
// parsing os.Getenv every call is avoidable overhead.
func laxKindFallbackDisabled() bool {
	return laxKindFallbackDisabledCache
}

// laxKindFallbackDisabledCache snapshots KEPLOY_STRICT_MOCK_WINDOW at
// package init. Processes that change the env after startup will not
// see the update; none of keploy's callers rely on that dynamic, and
// matching strictWindowEnvOverride's startup-read semantics keeps the
// two gates in sync.
var laxKindFallbackDisabledCache = func() bool {
	v := os.Getenv("KEPLOY_STRICT_MOCK_WINDOW")
	if v == "" {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}()

// legacyKindFallbackFires counts how many times DeriveLifetime has fallen
// back to kind-based inference because a recording lacked an explicit
// Metadata["type"] tag. Exposed via LegacyKindFallbackFires() for
// telemetry — when this stays at zero for a full release cycle, the kind
// fallback (and the compat branch that reads it) can be deleted.
var legacyKindFallbackFires uint64

// LegacyKindFallbackFires returns the number of times DeriveLifetime
// inferred LifetimeSession from mock.Kind because Spec.Metadata["type"]
// was missing/empty, not because an explicit tag resolved to session.
// Non-zero values mean untagged mocks reached the kind-based
// inference branch. Interpretation:
//   - For Kinds whose recorder does NOT yet classify lifetime at
//     record time (currently HTTP and Generic), non-zero is EXPECTED
//     even for newly recorded sessions — those recorders do not emit
//     a type tag for per-test mocks.
//   - For Kinds whose recorder DOES classify (MySQL, Postgres, etc.),
//     non-zero implies recordings predating the tag convention. Once
//     every remaining Kind classifies at record time, this counter
//     becoming zero for a release cycle is the signal to delete the
//     fallback.
func LegacyKindFallbackFires() uint64 {
	return atomic.LoadUint64(&legacyKindFallbackFires)
}

// kindsWithImplicitSessionLifetime returns true for Kinds whose
// un-tagged mocks should be treated as session-lifetime. This is NOT
// a legacy/compat shim that's going away soon — HTTP and Generic
// recorders do not (and cannot, generically) classify each mock's
// lifetime at record time, so un-tagged mocks from those protocols
// have always been implicitly session-reusable. The fallback here
// preserves that semantic explicitly.
//
// The set matches the pre-unification disk-layer kind-switch
// byte-for-byte so every pre-tag recording replays identically.
// Additions need per-protocol justification: "un-tagged mocks of
// this kind should always be session"; the right fix for a
// newly-observed per-test use case of a listed kind is to tag at
// record time (via a protocol-specific classifier like
// recordMock's type=mocks/config/connection branches).
//
// The LegacyKindFallbackFires counter still tracks uses for
// observability, but non-zero fires are EXPECTED for HTTP/Generic
// recordings.
func kindsWithImplicitSessionLifetime(k Kind) bool {
	switch k {
	case HTTP, HTTP2, MySQL, Postgres, PostgresV2, GENERIC, DNS:
		return true
	}
	return false
}

// mysqlIsSessionReusableCommand returns true when a MySQL mock
// represents a single connection-alive command whose response is
// input-independent. These are safe to route into the session pool
// regardless of the recorder's on-disk tag because the recorded
// response matches any client's same-command invocation.
//
// The set is deliberately narrow — matches the matcher-side
// isSessionReusableCommandMock helper. Commands NOT listed here
// (COM_QUERY, COM_INIT_DB, COM_CHANGE_USER, COM_SET_OPTION) depend
// on input and must stay per-test.
//
// The check uses the packet Header.Type string so the models package
// doesn't need to import the mysql wire package.
func mysqlIsSessionReusableCommand(m *Mock) bool {
	if m == nil || len(m.Spec.MySQLRequests) != 1 {
		return false
	}
	hdr := m.Spec.MySQLRequests[0].PacketBundle.Header
	if hdr == nil {
		return false
	}
	switch hdr.Type {
	case "COM_PING", "COM_STATISTICS", "COM_DEBUG", "COM_RESET_CONNECTION":
		return true
	}
	return false
}
