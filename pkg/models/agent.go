package models

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

type OutgoingReq struct {
	OutgoingOptions OutgoingOptions `json:"outgoingOptions"`
}

type IncomingReq struct {
	IncomingOptions IncomingOptions `json:"incomingOptions"`
}

// AgentResp is the agent's reply to the control endpoints that report only
// success or failure: /mock, /updatemockparams and /storemocks.
//
// ErrorMsg carries the agent-side failure as a STRING, not an `error`. It used
// to be an `error`, which is an interface, and that made every failure reply
// undecodable:
//
//	json.Marshal   -> {"error":{}}          (message already destroyed:
//	                                         most error types export no fields)
//	json.Unmarshal -> "json: cannot unmarshal object into Go struct field
//	                   AgentResp.error of type error"
//	gob.Encode     -> "gob: type not registered for interface:
//	                   errors.errorString"  (the encode ABORTS partway: a
//	                   partial type descriptor has already been written to
//	                   the stream, so the peer receives a truncated value
//	                   and fails with "unexpected EOF", not an empty body)
//
// Only the success case (error == nil) ever round-tripped, so the CLI reported
// a JSON/gob decode error in place of every real agent failure.
//
// Two details are load-bearing for CLI/agent version skew — the two are
// released separately and are routinely mismatched during a rolling upgrade
// (see AgentClient.StoreMocks's legacy fallback for a precedent):
//
//  1. The JSON key stays "error" and is `omitempty`. On success the key is
//     omitted entirely, so an OLD CLI (which still has `Error error`) decodes
//     a new agent's success reply cleanly. Without omitempty it would receive
//     `"error":""` and fail to unmarshal a string into the interface — that
//     would be a flag day on the SUCCESS path, far worse than the bug fixed
//     here. On failure an old CLI still fails to decode, exactly as it does
//     today: no regression, and it fails loudly rather than silently.
//
//  2. The Go field is named ErrorMsg, not Error. gob puts FIELD NAMES and
//     FIELD TYPES in the type descriptor and rejects a field whose type
//     changed between versions, so keeping the name `Error` while changing
//     its type to string would break /storemocks for every mismatched pair,
//     in both directions and on success as well as failure. Renaming the
//     field instead makes it UNKNOWN to an old decoder, which gob silently
//     ignores, so IsSuccess still arrives and /storemocks keeps working.
//     Verified in TestAgentResp_WireCompatAcrossVersions.
type AgentResp struct {
	ErrorMsg  string `json:"error,omitempty"`
	IsSuccess bool   `json:"isSuccess"`
}

// UnmarshalJSON accepts both the current string form and the legacy form a
// pre-fix agent emits for "error" (an encoded error interface — `{}` in
// practice, or `null` on success). Without this a NEW CLI talking to an OLD
// agent would fail the decode and once again report a JSON error in place of
// the agent's failure; with it, IsSuccess and the HTTP status stay usable and
// the caller can surface the raw body. That direction is the one that actually
// occurs: the CLI ships first and the agent image trails it.
func (r *AgentResp) UnmarshalJSON(b []byte) error {
	var raw struct {
		Error     json.RawMessage `json:"error"`
		IsSuccess bool            `json:"isSuccess"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	r.IsSuccess = raw.IsSuccess
	r.ErrorMsg = ""
	if len(raw.Error) == 0 || string(raw.Error) == "null" {
		return nil
	}
	var msg string
	if err := json.Unmarshal(raw.Error, &msg); err == nil {
		r.ErrorMsg = msg
		return nil
	}
	// Legacy encoded-interface form. There is no message to recover — it was
	// destroyed by the old agent's marshal — so leave ErrorMsg empty and let
	// IsSuccess/status carry the verdict rather than failing the whole decode.
	return nil
}

// Err returns the agent-side failure as an error, or nil when there was none.
// Callers get a plain message: the agent's error VALUE cannot cross a process
// boundary, so errors.Is/errors.As against a sentinel never worked here and
// does not now. The replay classifier (isDockerComposeReplayShutdown) matches
// on the message text, which this restores.
func (r AgentResp) Err() error {
	if r.ErrorMsg == "" {
		return nil
	}
	return errors.New(r.ErrorMsg)
}

type TestMockMapping struct {
	TestName string   `json:"test_name"`
	MockIDs  []string `json:"mock_ids"`
}

// ScopeReq is the body of POST /agent/scope/begin and /agent/scope/end — a
// test-runner plugin / glue-code marks a named per-test scope so the mock flow
// can attribute captured mocks to a test (record) or restrict the served pool
// to that test (replay).
type ScopeReq struct {
	Name string `json:"name"`
	// Pid is the calling test WORKER's PID (e.g. Node process.pid, os.Getpid()).
	// It keys the per-worker scope so parallel workers don't stomp each other
	// (Design A). Optional: 0/omitted falls back to the single global scope
	// (correct for sequential single-worker runs and suite-level).
	Pid int `json:"pid,omitempty"`
	// At is the runner's own clock for this boundary; zero means the agent stamps its read time.
	At    time.Time `json:"at,omitzero"`
	Dir   string    `json:"dir,omitempty"`
	Suite bool      `json:"suite,omitempty"`
	// Outcome is the test's own verdict, sent with /agent/scope/end by a
	// harness that knows it (ScopeOutcomePassed / Failed / Skipped; common
	// spellings such as "pass", "fail", "pending" are accepted, see
	// NormalizeScopeOutcome). Optional: without it the replay knows what the
	// test used and missed, not whether it passed.
	Outcome string `json:"outcome,omitempty"`
	// CanSkip, sent with /agent/scope/begin, says the harness skips a test it
	// is told to (ScopeBeginResp.Action). Only such a begin is gated: a harness
	// that cannot skip would run the test anyway, against a pool the gate never
	// narrowed for it.
	CanSkip bool `json:"canSkip,omitempty"`
	// Attempt is the runner's attempt number for this test: 0 (or omitted) is
	// the first run, 1 and up are retries. Playwright exposes it as
	// testInfo.retry, Jest as the retry index, pytest-rerunfailures as the
	// rerun count.
	//
	// It exists because a retry and a deliberately reopened scope label are the
	// SAME two calls on the wire — begin(name) after end(name) — yet they want
	// opposite things. A retry re-runs a test that already consumed its mocks,
	// so it must replay that test's tape from the start. A reopened label is a
	// second test deliberately continuing the first one's tape, so it must keep
	// advancing. Only the runner can tell them apart, so it says which it is
	// rather than leaving Keploy to guess.
	Attempt int `json:"attempt,omitempty"`
}

// The verdicts a harness reports in ScopeReq.Outcome, and ScopeOutcomeGated,
// which the agent records for a test the replay told not to run.
const (
	ScopeOutcomePassed  = "passed"
	ScopeOutcomeFailed  = "failed"
	ScopeOutcomeSkipped = "skipped"
	ScopeOutcomeGated   = "gated"
)

// maxScopeOutcome bounds a verdict a harness sends; anything longer is not one.
const maxScopeOutcome = 32

// NormalizeScopeOutcome maps a harness's verdict onto ScopeOutcomePassed,
// Failed or Skipped, accepting the spellings test runners use (go test's
// pass/fail, jest's pending, pytest's error and xfail). Anything else is
// returned lower-cased and cut to a bounded length, so a policy matching
// "passed" never proves it; "" stays "".
func NormalizeScopeOutcome(s string) string {
	v := strings.ToLower(strings.TrimSpace(s))
	switch v {
	case "", ScopeOutcomePassed, ScopeOutcomeFailed, ScopeOutcomeSkipped:
		return v
	case "pass", "ok", "success", "succeeded", "xpassed":
		return ScopeOutcomePassed
	case "fail", "error", "errored", "broken", "timedout", "timeout", "interrupted":
		return ScopeOutcomeFailed
	case "skip", "pending", "todo", "disabled", "ignored", "xfail", "xfailed":
		return ScopeOutcomeSkipped
	}
	if len(v) > maxScopeOutcome {
		v = v[:maxScopeOutcome]
	}
	return v
}

// ScopeBeginResp answers POST /agent/scope/begin. A test the replay gated out
// (see ScopeGateReq), begun by a harness that can skip (ScopeReq.CanSkip),
// gets Action ScopeActionSkip and the Reason; its harness skips it and sends
// no end for it. Every other answer is exactly
// {"status":"ok"}, as before, so a harness that predates gating — or decodes
// the answer as strings — is unaffected, and a missing action means "run".
type ScopeBeginResp struct {
	Status string `json:"status"`
	Action string `json:"action,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// ScopeActionSkip is the ScopeBeginResp.Action telling a harness to skip.
const ScopeActionSkip = "skip"

// SpecSetupScopePrefix starts the name of a spec's setup scope: the window a
// browser harness opens around one spec file's beforeAll / afterAll hooks and
// the gaps between its tests, named "__suite__:<spec file>". Unlike a test it
// may legitimately open more than once in a run -- once for every runner
// project that includes the spec, and again when the runner replaces a worker
// partway through the spec -- and every opening feeds the same setup pool.
const SpecSetupScopePrefix = "__suite__:"

// IsSpecSetupScope reports whether name is a spec setup scope.
func IsSpecSetupScope(name string) bool {
	return len(name) > len(SpecSetupScopePrefix) && strings.HasPrefix(name, SpecSetupScopePrefix)
}

// ScopeGateReq is the body of POST /agent/replay/gate: the replay CLI names the
// only tests that should run this replay (for instance, the ones a previous
// replay proved pass with every dependency mocked). Any other test — other
// than a subtest of a named one — is answered ScopeActionSkip at
// /agent/scope/begin, with Reason. A nil Run removes the gate: every test runs,
// the default. The gate applies in replay only, and ends with the replay
// session.
type ScopeGateReq struct {
	Run    []string `json:"run"`
	Reason string   `json:"reason,omitempty"`
}

// ScopeWindow is one recorded per-test scope: the agent-clock interval during
// which the named test made its outgoing calls. Record correlates captured
// mocks into these windows to build mappings.yaml — by source PID when the
// worker reported one (exact, parallel-safe), else by request timestamp.
type ScopeWindow struct {
	Name  string    `json:"name"`
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
	// PID is the reporting worker's PID (ScopeReq.Pid), 0 if none. When set,
	// record attributes a captured mock to this window if the mock's own source
	// PID resolves (up the /proc tree) to this worker — exact even when windows
	// from parallel workers overlap in time.
	PID     uint32    `json:"pid,omitempty"`
	Dir     string    `json:"dir,omitempty"`
	Suite   bool      `json:"suite,omitempty"`
	App     bool      `json:"app,omitempty"`
	Port    uint16    `json:"port,omitempty"`
	Program string    `json:"program,omitempty"`
	Place   string    `json:"place,omitempty"`
	N       int       `json:"n,omitempty"`
	Ready   time.Time `json:"ready,omitzero"`
	Ref     string    `json:"ref,omitempty"`
	Worker  uint32    `json:"worker,omitempty"`
	// Outcome is the verdict the test's harness reported when it ended the
	// scope (ScopeReq.Outcome); "" when it reported none.
	Outcome string `json:"outcome,omitempty"`
}

// ScopeAck is the body of POST /agent/scope/begin. Status is retained verbatim
// for callers written against the original `{"status":"ok"}` contract; the
// remaining fields report whether the call actually took effect, so a test
// runner can assert it in one line instead of reading the agent's debug log.
//
// Scoped is true only when the served mock pool was genuinely narrowed to this
// scope (replay). It is therefore false for every record-mode call — recording
// serves nothing — and Reason names which case it was.
type ScopeAck struct {
	Status string `json:"status"`
	Scoped bool   `json:"scoped"`
	// Mocks is how many recordings mappings.yaml attributes to THIS scope --
	// the count of mapped names, not the number of mocks the agent went on to
	// stage (the served pool also carries recordings no test owns, as overflow).
	// It is the useful number for a runner: it knows how many dependency calls
	// its test makes, so a short count is how a truncated mapping shows up --
	// most often a second scope/begin at record with no end between, which
	// resets the window and orphans everything captured before it.
	Mocks  int    `json:"mocks"`
	Reason string `json:"reason"`
	// Attempt echoes ScopeReq.Attempt. An agent that predates the field echoes
	// nothing, so a fixture can distinguish "this build ignored my attempt
	// number" from "this build reset nothing because there was nothing to
	// reset".
	Attempt int `json:"attempt,omitempty"`
	// RetryReset is true when Attempt > 0 AND this scope's consumed state was
	// cleared, so its mocks replay from the start. False with Attempt > 0 means
	// nothing was reset and Reason says why: the scope has no mapping entry to
	// reset (no_mapping_table / unmapped_scope / empty_mapping), or the call is
	// worker-scoped (worker_scoped), where the served pool is never re-staged.
	RetryReset bool `json:"retry_reset,omitempty"`
	// RestoredMocks is how many of this scope's mocks the retry reset returned
	// to the servable pool. 0 alongside RetryReset true means the scope had
	// consumed nothing yet — e.g. the previous attempt failed before its first
	// outgoing call.
	RestoredMocks int `json:"restored_mocks,omitempty"`
	// ClockBase is the time this scope's clock starts at, FormatScopeStart: at
	// record the scope's own window start, at replay the start its mocks were
	// recorded under (ScopeTableReq.ClockBases). A harness that pins its clock
	// pins each scope here, so a test reads the real time it ran at record, and
	// the same time at replay. Empty when the agent has none for the scope.
	ClockBase string `json:"clock_base,omitempty"`
}

// Reason values for ScopeAck. Stable tokens — test runners branch on them.
const (
	// Replay, narrowed. Scoped=true.
	ScopeReasonPoolRestricted = "pool_restricted" // pid==0, single global pool
	ScopeReasonWorkerScoped   = "worker_scoped"   // pid>0, this worker's view only

	// Replay, NOT narrowed. Scoped=false — the whole set is being served.
	ScopeReasonNoMappingTable = "no_mapping_table" // no mappings.yaml, so no scope table was installed
	ScopeReasonUnmappedScope  = "unmapped_scope"   // table installed, this name absent from it
	ScopeReasonEmptyMapping   = "empty_mapping"    // name present but mapped to zero mocks
	// ScopeReasonStrictNoRecording: strict scoping is on and this test has no
	// recording of its own, so it was narrowed to nothing rather than being
	// served the pool. Its first dependency call will miss, which is the point:
	// another test's response is not a valid answer for this one.
	ScopeReasonStrictNoRecording = "strict_no_recording"

	// Record. Nothing is served, so Scoped is always false.
	ScopeReasonRecordWindowOpened = "record_window_opened"
	// ScopeReasonRecordAlreadyOpen: a second begin for a scope that was never
	// ended. The window start is reset, so anything captured before this call
	// is attributed to no test and disappears from mappings.yaml.
	ScopeReasonRecordAlreadyOpen = "record_scope_already_open"

	ScopeReasonEmptyName   = "empty_name"  // no name supplied; the call is a no-op
	ScopeReasonUnsupported = "unsupported" // this agent build has no scope support
)

// ScopeTableReq is the body of POST /agent/scope/table — the replay CLI hands
// the agent the per-test name→mock-names table (from mappings.yaml) so the
// runner's /agent/scope/begin calls can restrict the served pool per test.
type ScopeTableReq struct {
	Mappings map[string][]string `json:"mappings"`
	// ClockBases is, per scope, the start its own mocks were recorded under
	// (ScopeClockBases); the agent answers it as ScopeAck.ClockBase at begin.
	ClockBases map[string]string   `json:"clockBases,omitempty"`
	Root       string              `json:"root,omitempty"`
	Sets       map[string]SetTable `json:"sets,omitempty"`
	// Strict makes a test with no recording serve NOTHING but the mocks no test
	// owns, instead of leaving the whole pool armed. Opt-in: integration testing
	// deliberately lets one test's recording answer another's request, and that
	// must keep working.
	Strict bool `json:"strict,omitempty"`
}

type SetTable struct {
	Boots  map[string][]string `json:"boots,omitempty"`
	Tests  map[string][]Owned  `json:"tests,omitempty"`
	Runner []string            `json:"runner,omitempty"`
	Start  time.Time           `json:"start,omitzero"`
}

type Owned struct {
	Name  string `json:"name"`
	Start string `json:"start,omitempty"`
}

// ErrMockStatsUnsupported reports that an agent cannot answer /agent/mock/stats
// at all — it predates the route, or its service does not implement the reader.
// It lives here, not in the HTTP client, so the replay service can recognise the
// condition without importing the concrete client it otherwise reaches only
// through its Instrumentation interface.
//
// Callers must treat it as "unknown", never as "no mocks stored": a caller that
// conflates the two reads an unreportable agent as a replaced one.
var ErrMockStatsUnsupported = errors.New("agent cannot report mock stats")

// ErrScopeGateUnsupported is returned for an agent that cannot gate which
// tests run (one that predates POST /agent/replay/gate).
var ErrScopeGateUnsupported = errors.New("agent cannot gate which tests run")

// MockStats is the body of GET /agent/mock/stats — a non-draining snapshot of
// the mock session for the runner or the CLI end-of-run summary.
type MockStats struct {
	Loaded   int `json:"loaded"`
	Consumed int `json:"consumed"`
	Missed   int `json:"missed"`
}

type SetMocksReq struct {
	Filtered   []*Mock `json:"filtered"`
	UnFiltered []*Mock `json:"unFiltered"`
}

type StoreMocksReq struct {
	Filtered   []*Mock `json:"filtered"`
	UnFiltered []*Mock `json:"unFiltered"`
}

const StoreMocksStreamContentType = "application/x-gob-stream"

// MockStreamEncodingZstd is the Content-Encoding of a zstd-compressed /storemocks
// stream. An agent that can decode it says so with "Accept-Encoding: zstd" on its
// /health response (RFC 7694), and the client compresses only for such an agent:
// an older one would read the compressed bytes as gob and fail the upload.
const MockStreamEncodingZstd = "zstd"

// MockStreamZstdWindow is the zstd window both ends of a /storemocks stream use.
// The client encodes with it and the agent refuses a stream that needs more, so
// a stream cannot make the agent allocate more than this for its window.
const MockStreamZstdWindow = 8 << 20

// MockStreamHeader is the first gob value on a /storemocks body; the counts
// pre-size the agent's slices and split the following mocks into filtered then
// unfiltered.
type MockStreamHeader struct {
	FilteredCount   int `json:"filteredCount"`
	UnfilteredCount int `json:"unfilteredCount"`
}

type MockFilterParams struct {
	AfterTime time.Time `json:"afterTime,omitempty"`
	// FirstRecordedTestStart is the request time of the EARLIEST RECORDED test
	// in the set being staged, which the replayer knows because it loads test
	// cases sorted by request timestamp. The agent seeds the manager's
	// startup-init cutoff from it so that cutoff follows the set's recorded
	// shape rather than whichever test happens to fire first — a --test-sets
	// selection, an ignored test or the streaming deferral otherwise leave it
	// late, and mocks from a test that never runs get served as bootstrap.
	// Zero means "not supplied"; the cutoff then falls back to the fired
	// windows, which is the pre-existing behaviour.
	FirstRecordedTestStart time.Time `json:"firstRecordedTestStart,omitempty"`
	// RecordedWindows is the [request, response] window of EVERY recorded test
	// of the set being staged, the unselected and ignored ones included. Sent
	// on the staging call only, beside FirstRecordedTestStart; the agent
	// applies it through the proxy's optional SeedRecordedWindows capability,
	// so traffic the test windows do not pace by themselves (a broker's server
	// push, and what the application publishes while handling it) can be
	// released by the recorded window it belongs to (models.WindowSchedule).
	// Empty means "not supplied": everything is then released at once, which
	// is the pre-existing behaviour.
	RecordedWindows []TestWindow `json:"recordedWindows,omitempty"`
	BeforeTime      time.Time    `json:"beforeTime,omitempty"`
	MockMapping     []string     `json:"mockMapping,omitempty"`
	UseMappingBased bool         `json:"useMappingBased"`
	// AgentOwnsConsumed, when true, tells the agent to apply filterOutDeleted
	// from its OWN persistent consumption history instead of the
	// TotalConsumedMocks map the client would otherwise re-send every testcase
	// (which is O(testcases^2) marshaling). Default false = legacy behaviour.
	AgentOwnsConsumed  bool                 `json:"agentOwnsConsumed,omitempty"`
	TotalConsumedMocks map[string]MockState `json:"totalConsumedMocks,omitempty"`
	// MockMappingUniverse is the union of every test's mapped mock names, i.e.
	// every name that appears anywhere in mappings.yaml. A recorded mock whose
	// name is ABSENT from it belongs to no test — traffic captured outside every
	// scope window, such as a Playwright `beforeAll` or a bootstrap handshake.
	//
	// Set alongside MockMapping, it makes those shared recordings visible to a
	// scoped test as OVERFLOW: the test's own mapped mocks are matched first and
	// the shared ones only after they are exhausted. Ordering is load-bearing —
	// see FilterTcsMocksMappingWithShared.
	//
	// nil restores the previous behaviour exactly (mapped-only pool). Only
	// Agent.BeginScope populates it, so the `keploy test` replay path — which
	// also sets UseMappingBased — is unaffected.
	MockMappingUniverse []string `json:"mockMappingUniverse,omitempty"`
	// StrictMockWindow controls whether out-of-window non-config mocks are
	// dropped rather than being promoted into the cross-test config pool.
	// Default TRUE (see config.Test default) — out-of-window per-test
	// mocks get dropped, eliminating cross-test bleed. Prepared
	// statements replay correctly under strict via LifetimeConnection
	// (per-connID pool). Set false to fall back to legacy lax behaviour
	// for older recordings that rely on implicit cross-test sharing.
	// The process-wide env override KEPLOY_STRICT_MOCK_WINDOW is OR-ed
	// in: an enabling value forces strict; an explicit disabling value
	// ("0") forces strict off regardless of the per-call flag.
	StrictMockWindow bool `json:"strictMockWindow,omitempty"`
}

// ConsumedScopeHeader is the /updatemockparams response header in which an agent
// says how it reads MockFilterParams.TotalConsumedMocks. ConsumedScopePerTest
// means only for the per-test mocks it stages, all of which the client handed
// it as the filtered half of StoreMocks; a client may then send just their
// entries. An agent that does not send the header, which includes every agent
// released before it, may read entries of other mocks too and must be sent the
// whole history: agents from v3.0.0-beta1 through v3.3.22 also applied it to the
// session pool.
const (
	ConsumedScopeHeader  = "X-Keploy-Consumed-Scope"
	ConsumedScopePerTest = "per-test"
)

// UnmatchedCallsUnreadHeader is the /mockerrors response header in which an
// agent says the list in the body is incomplete (ErrUnmatchedCallsUnread), with
// why. The body is still the list that was read, so a client that does not know
// the header reads it as before. Only an agent with the proven switch on sends
// it.
const UnmatchedCallsUnreadHeader = "X-Keploy-Unmatched-Calls-Unread"

type UpdateMockParamsReq struct {
	FilterParams MockFilterParams `json:"filterParams"`
}

type BeforeSimulateRequest struct {
	TimeStamp    time.Time `json:"timestamp"`
	TestSetID    string    `json:"testSetID"`
	TestCaseName string    `json:"testCaseName"`
}

type AfterSimulateRequest struct {
	TestSetID    string `json:"testSetID"`
	TestCaseName string `json:"testCaseName"`
}

type BeforeTestRunReq struct {
	TestRunID string `json:"testRunID"`
}

type BeforeTestSetCompose struct {
	TestRunID string `json:"testRunID"`
	// TestSetID is the test set boundary identifier used by the agent
	// to drive per-test-set side effects — currently debug-file
	// rotation, which used to piggyback on BeforeSimulate (per test
	// case) and consequently never produced a per-set log file for
	// test sets that resolved to NO_TESTS_TO_RUN.
	TestSetID string `json:"testSetID"`
}

type AfterTestRunReq struct {
	TestRunID  string       `json:"testRunID"`
	TestSetIDs []string     `json:"testSetIDs"`
	Coverage   TestCoverage `json:"coverage"`
}

type AppStart struct {
	At   time.Time `json:"at"`
	PID  uint32    `json:"pid"`
	Port uint16    `json:"port"`
}

type AppStartReq struct {
	Port int       `json:"port"`
	Pid  int       `json:"pid,omitempty"`
	At   time.Time `json:"at,omitzero"`
}

// BacklogVerdict is what the agent answers when a recording's stop asks
// whether it may still hand over something captured before the stop
// (GET /agent/record/pending, AgentBacklog). It has three values, and a fourth
// for an agent that can never tell: an answer that did not come is never read
// as "nothing pending".
type BacklogVerdict int

const (
	// BacklogCannotTell: no answer came (a timeout, a transport error, an
	// error status, a body that does not decode). The agent may still hold
	// something; the stop keeps waiting while it can. The zero value, so an
	// answer that was never filled in is never taken for BacklogClear.
	BacklogCannotTell BacklogVerdict = iota
	// BacklogPending: the agent may still hand over something captured
	// before the stop.
	BacklogPending
	// BacklogClear: the agent holds nothing more captured before the stop:
	// its capture, parsers and test-case queue are through it. What it has
	// already handed on and the client has not received yet (in its mock
	// manager's channel, the stream's encoder, the socket) is not counted:
	// clear is not delivered.
	BacklogClear
	// BacklogUnsupported: the agent cannot tell, and never will (it predates
	// the route, or its capture has no watermark). The stop goes by its
	// streams' quiet alone.
	BacklogUnsupported
)

func (v BacklogVerdict) String() string {
	switch v {
	case BacklogPending:
		return "pending"
	case BacklogClear:
		return "clear"
	case BacklogUnsupported:
		return "unsupported"
	}
	return "cannot tell"
}

// AgentBacklog is the agent's answer to whether it may still hand over a test
// case or a mock captured before a time.
type AgentBacklog struct {
	Verdict BacklogVerdict
	// Progress is a count that moves whenever the agent's capture gets
	// further: with BacklogPending, a stop tells a backlog being worked
	// through from one that is not moving. Zero from an agent that predates
	// it.
	Progress uint64
	// WaitingOn says what is pending, when the agent can tell.
	WaitingOn string
}

// DrainsAfterStopHeader marks an agent frame stream (test cases, mocks,
// mappings) whose client drains it after a recording's stop and closes it
// before it stops the agent, and is the only thing that stops the agent: it
// started it (natively, or in a container it runs). `keploy record` and
// `keploy mock record` send it then. A stop signal the agent gets meanwhile
// (a `pkill -INT keploy`, or a stop of its container, that reaches it too)
// waits for such streams (utils.SetSignalHold). An agent something else
// stops (docker compose, with the project the client waits on; kubelet, with
// a sidecar's pod) gets no header, and stops on the signal as it always has:
// held for the client, it would only be killed at that stop's timeout.
const DrainsAfterStopHeader = "Keploy-Drains-After-Stop"

type drainsAfterStopKey struct{}

// WithDrainsAfterStop marks the agent frame streams opened on ctx as ones
// their client drains after the stop and closes before it stops the agent.
// The agent client sends DrainsAfterStopHeader for them when it started the
// agent itself. On the agent, the graceful-shutdown ctx carries it while such
// a client holds a stream: that client stops the app it started first.
func WithDrainsAfterStop(ctx context.Context) context.Context {
	return context.WithValue(ctx, drainsAfterStopKey{}, true)
}

// DrainsAfterStop reports whether ctx was marked with WithDrainsAfterStop.
func DrainsAfterStop(ctx context.Context) bool {
	v, _ := ctx.Value(drainsAfterStopKey{}).(bool)
	return v
}
