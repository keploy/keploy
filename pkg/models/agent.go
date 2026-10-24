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
	// StreamEnd marks the end of an agent's mapping stream, written once a
	// recording's client asked it to end its streams (StreamEndPath) and it
	// has put on the stream all it had: no test or mocks of its own.
	StreamEnd bool `json:"stream_end,omitempty" yaml:"-"`
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
	// File and Project qualify the test's ID under the proven wire (TestID):
	// the test file a harness names, and its project (a Jest displayName).
	// A Go test sends neither; its instance's key is the path.
	File    string `json:"file,omitempty"`
	Project string `json:"project,omitempty"`
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
	// V is ScopeProtocolVersion under the proven wire, absent otherwise.
	V      int    `json:"v,omitempty"`
	Status string `json:"status"`
	Action string `json:"action,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// ScopeActionSkip is the ScopeBeginResp.Action telling a harness to skip.
const ScopeActionSkip = "skip"

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
	// Skip is the proven model's gate (design v14 §6.1): the tests keploy
	// skips, by ID, each with the reason its harness skips it with. It is a
	// deny list: every test it does not name runs. Read under the proven
	// switch, where it replaces Run.
	Skip map[string]string `json:"skip,omitempty"`
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
	// Evidence is, under the proven wire, what keploy saw that failed the
	// test at its end in a replay; nil when nothing did.
	Evidence *ScopeEvidence `json:"evidence,omitempty"`
	// Real is, under the proven wire, the mock.real destinations the test
	// used while it was open (design v14 §4): its REAL rows, and in a
	// record, the REAL list its recording carries.
	Real []RealUse `json:"real,omitempty"`
	// Given is, under the proven wire, what the test's harness gave keploy
	// while the test was open in a record (POST /agent/given), in order.
	Given []GivenValue `json:"given,omitempty"`
	// Timers is, under the proven wire, each call of the test its harness
	// saw a repeating timer make while it was open in a record (POST
	// /agent/cause), in order.
	Timers []TimerCall `json:"timers,omitempty"`
	// Claim, under the proven wire, makes this no test's window but a claim:
	// from Start on, the run's test process is the one that claimed it with
	// this key (POST /agent/instance), until the next claim. A call made
	// while no test is open belongs to the pool slice of that key (design
	// v14 §5.2).
	Claim string `json:"claim,omitempty"`
	// Refused, under the proven wire, makes this no test's window but a
	// refusal keploy made at Start (RunRefusal).
	Refused *RunRefusal `json:"refused,omitempty"`
	// Live and Mocked, under the proven wire, make this no test's window
	// but one row of where the run's calls to local listeners went
	// (LiveRow, MockedRow), first seen at Start.
	Live   *LiveRow   `json:"live,omitempty"`
	Mocked *MockedRow `json:"mocked,omitempty"`
}

// PoolKeyNone is the pool slice of calls made before any test process
// claimed the run, and the slice a pool mock recorded without a key serves
// (design v14 U21).
const PoolKeyNone = "-"

// ScopeTableReq is the body of POST /agent/scope/table — the replay CLI hands
// the agent the per-test name→mock-names table (from mappings.yaml) so the
// runner's /agent/scope/begin calls can restrict the served pool per test.
type ScopeTableReq struct {
	Mappings map[string][]string `json:"mappings"`
	Root     string              `json:"root,omitempty"`
	Sets     map[string]SetTable `json:"sets,omitempty"`
	// Pool is, under the proven switch, each key's pool slice: the names of
	// the mocks recorded while no test was open, by the key of the claim
	// then (PoolKeyNone for none, and for a mock recorded without a key).
	// omitzero: an empty pool ({}) is sent, and means "serve no pool mock
	// between tests"; only a nil one is left out (with the switch off).
	Pool map[string][]string `json:"pool,omitzero"`
	// Worker is, under the proven switch, the set's worker scope
	// (Mapping.Worker) of mocks the agent holds: no test owns them, and none
	// is in a pool slice. Each answers at most one call, and only within its
	// stretch (MockEntry.Stretch).
	Worker []MockEntry `json:"worker,omitempty"`
}

// TestBeginsReq is the body of POST /agent/replay/clock, which only the keploy
// CLI sends: when each test of the replayed recording began
// (MappedTestCase.At), by test name. Under the proven switch the agent moves
// the clock of the run's processes to a test's begin while the test is open,
// and back to the run's start time after it (design v14 §3.4).
// GivenValue is a value a test's harness gave keploy while the test was open:
// one the test made itself and puts into its calls (a session token it
// signs, a cookie it installs), named by the harness. Its name pairs the
// value of a replay with the recording's, by the same test and name: the
// harness names each in the order the test installs it, so the pairing is
// declared, never guessed.
type GivenValue struct {
	Name  string `json:"name" yaml:"name" bson:"name"`
	Value string `json:"value" yaml:"value" bson:"value"`
}

// GivenReq is the body of POST /agent/given: a value the open test made
// (GivenValue). In a record it is kept with the test's recording; in a replay
// it is bound as the test's given variable of that name, recorded as the
// recording's value of it (TestGivenReq).
type GivenReq struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// TestGivenReq is the body of POST /agent/replay/given: by test ID, the values
// its harness gave when it was recorded (MappedTestCase.Given), and by claim
// key, those it gave while no test was open (Mapping.KeyGiven).
type TestGivenReq struct {
	Given map[string][]GivenValue `json:"given"`
	Keys  map[string][]GivenValue `json:"keys,omitempty"`
}

// KeyGiven is what a claim key's harness gave while no test was open (a
// group's beforeAll that signs a session for its tests), in a record: the
// key's, like the variables its calls outside every test bind
// (MockVariable.Outside). Its replay binds each, by name, in the key's scope.
type KeyGiven struct {
	Key   string       `json:"key" yaml:"key" bson:"key"`
	Given []GivenValue `json:"given" yaml:"given" bson:"given"`
}

type TestBeginsReq struct {
	Begins map[string]time.Time `json:"begins"`
	// Real is, by test, the mock.real destinations its recording lists: such
	// a test runs on the real clock, not at its recorded begin (design v14
	// §3.4, F3). Absent when no test lists any.
	Real map[string][]string `json:"real,omitempty"`
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

// ErrNoMappingStream is what GetMappings returns for an agent that predates
// test-mock mappings (keploy #3715): it has no /mappings route, so there is no
// mapping stream at all, and a recording goes on without one. It is not a
// failure, and not a stream the agent ended: a recorder that takes it for
// either says a loss that never happened.
var ErrNoMappingStream = errors.New("this keploy agent has no mapping stream")

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
	// ExactMapping makes MockMapping the test's whole recording even when
	// it is empty: a test recorded with no call is served no per-test mock,
	// only the reusable tier. Without it an empty mapping means "none
	// supplied", which leaves the whole pool armed.
	ExactMapping bool `json:"exactMapping,omitempty"`
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

	// Variables are the test variables the test's own request carries (its
	// test case's template values), under the proven switch: a mock filter
	// starts a test, and the agent binds these as the test begins, so a
	// dependency call that uses one is asserted to send that value. Empty for
	// every other caller.
	Variables []VariableBinding `json:"variables,omitempty"`

	// Exact makes the call keploy test's exact serving, under the proven
	// switch (design v14 §5.2): the agent stages nothing per test, and
	// serves each test case exactly its own mocks. nil for every other
	// caller, and always without the switch.
	Exact *ExactServing `json:"exact,omitempty"`
}

// ExactServing is what a keploy test filter asks of exact serving, under the
// proven switch.
type ExactServing struct {
	// Stage is the set's staging call: the agent stages every mock of the
	// set once (the rest of the filter says how, as without exact serving),
	// then serves Pool between test cases.
	Stage bool `json:"stage,omitempty"`
	// Pool is, on the staging call, the set's pool: every mock no test
	// case's mapping lists. It answers between test cases, beside the
	// protocol-phase mocks.
	Pool []string `json:"pool,omitempty"`
	// Test opens this test case, which owns the data-plane mocks of Owned
	// (its mapping). Without Stage, an empty Test serves the pool again.
	Test  string   `json:"test,omitempty"`
	Owned []string `json:"owned,omitempty"`
	// Ahead is, on a call that serves the pool (no Test), the mocks of the
	// next test case's mapping that were in flight when it began: recorded
	// in the gap before its window, answered once it had begun
	// (models.WindowSchedule.InFlightInto). They answer between test cases,
	// each at most once, as that test case's own: one answered then counts
	// as answered when it opens. An agent that predates it ignores it.
	Ahead []string `json:"ahead,omitempty"`
	// Worker is, on the staging call, the set's worker scope
	// (Mapping.Worker): the mocks no test case owns, each with its place
	// (MockEntry.Place). The agent stages them with the pool, and each
	// answers from its place in the set's run on (once the test case it is
	// placed after has begun; from the start for an empty place), whether a
	// test case is open or not, after the open test case's own, at most one
	// request in the staging.
	Worker []MockEntry `json:"worker,omitempty"`
	// Concurrent is, on a call that opens a test case (Test), the
	// recordings it may answer with beside Owned: those recorded while its
	// request and other tests' requests were open together, which keploy
	// record's full round shares between those tests (Mapping.Groups). Each
	// answers at most once in the staging, for whichever of those tests
	// makes its call first, and is never required of the test. Owned answer
	// before them. Sent only to an agent that says it serves them
	// (ExactConcurrentHeader); an agent that predates it ignores it.
	Concurrent []string `json:"concurrent,omitempty"`
}

// ExactConcurrentHeader is the /storemocks response header in which an agent
// with the proven switch on says it serves a test case the recordings it
// shares with the tests that ran beside it (ExactServing.Concurrent).
// ExactConcurrentServes is its value. An agent that does not send it is never
// sent them.
const (
	ExactConcurrentHeader = "Keploy-Exact-Concurrent"
	ExactConcurrentServes = "1"
)

// BeginsTestVariables reports whether, under the proven switch, the agent
// begins a test's variables as it applies this filter: a filter that opens a
// test case, with exact serving or without. Two filters begin none:
//
//   - A test set's staging (StagesSet). The set's variables began at its
//     Mock (MockManager.NewVariableSet), as the learn step's set did
//     (learn.Tracker.NewSet), and what the set's start-up binds before its
//     first test begins is the run's, which every test of the set reads: a
//     scope begun at the staging would take the start-up's bindings as its
//     own, and the first test's would forget them.
//   - Exact serving's pool call. It ends a test case's serving, and the
//     variables its dependency calls bound stay readable until the next test
//     case's filter begins the next scope: keploy test reads them after it.
func (p MockFilterParams) BeginsTestVariables() bool {
	if p.StagesSet() {
		return false
	}
	return p.Exact == nil || p.Exact.Test != ""
}

// StagesSet reports whether the filter is a test set's staging: the set's
// mocks staged before any of its tests runs, from BaseTime and naming no
// test's mapping, as keploy test sends it once per set (exact serving's
// Stage, which it sends on that same call).
func (p MockFilterParams) StagesSet() bool {
	if p.Exact != nil {
		return p.Exact.Stage
	}
	return p.AfterTime.Equal(BaseTime) && len(p.MockMapping) == 0
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

// AgentInstanceHeader is the response header in which an agent names the
// process that answers: an id the agent draws once, as its process starts.
// What a test set is judged on (the calls no mock answered, what its tests
// consumed, what keploy could not capture) is held in that process's memory,
// and an agent started again in its place (kubelet restarting a crashed
// sidecar, docker restarting a container) answers at the same address with
// none of it. Two answers that name different processes say that happened
// between them. Only an agent with the proven switch on sends it.
const AgentInstanceHeader = "X-Keploy-Agent-Instance"

// AgentReplacement is an agent process answering where another answered
// before it: First, the process that answered first since a watch began
// (AgentInstanceWatch), and Next, the first other one that answered after it.
type AgentReplacement struct {
	First string
	Next  string
}

// AgentInstanceWatch watches which agent process answers a client's agent
// address (AgentInstanceHeader): from when it begins, the first process to
// answer is the one the watch keeps to, and any other that answers after it
// replaced it.
type AgentInstanceWatch interface {
	// Replacement reports the first process that answered after the one the
	// watch keeps to, and whether one did.
	Replacement() (AgentReplacement, bool)
	// Accept says the process that answers now was set up again in full
	// before the application ran against it (the docker compose bring-up's
	// retry): nothing the watch keeps to was on the process it replaced, and
	// the watch keeps to this one from now on.
	Accept()
	// Stop ends the watch.
	Stop()
}

type UpdateMockParamsReq struct {
	FilterParams MockFilterParams `json:"filterParams"`
}

type BeforeSimulateRequest struct {
	TimeStamp    time.Time `json:"timestamp"`
	TestSetID    string    `json:"testSetID"`
	TestCaseName string    `json:"testCaseName"`
	// Real, under the proven switch, is the mock.real destinations the test
	// case's recording lists: its request runs on the real clock, not at
	// TimeStamp (design v14 §3.4, F3).
	Real []string `json:"real,omitempty"`
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
	// EndMarkers says the agent counts in its answer the mappings it owes for
	// tests from before the time asked about, and ends its frame streams, each
	// with a marker after all it had, when a recording's client asks it to
	// (StreamEndPath): its client's drains then end on those events, not on a
	// stream's quiet. False from an agent that predates it, whose client goes
	// by quiet as before.
	EndMarkers bool
	// Progress is a count that moves whenever the agent's capture gets
	// further: with BacklogPending, a stop tells a backlog being worked
	// through from one that is not moving. Zero from an agent that predates
	// it.
	Progress uint64
	// WaitingOn says what is pending, when the agent can tell.
	WaitingOn string
}

// StreamEndPath is the agent route a recording's client POSTs to, once the
// agent has said it holds nothing more from before the client's stop, to have
// the agent end that recording's frame streams (test cases, mocks, mappings),
// named by their token (?token=, StreamEndTokenHeader): each puts on the
// stream what it still has, then its end marker (StreamEndKind,
// TestMockMapping.StreamEnd, a "stream-end" part), and ends.
const StreamEndPath = "/record/end"

// StreamEndTokenHeader carries, on each frame stream's request, the token of
// the recording the stream is for: the agent ends the streams of the token a
// client names (StreamEndPath), that recording's alone, whoever started the
// agent (the client, or docker compose).
const StreamEndTokenHeader = "Keploy-Stream-End-Token"

type streamEndTokenKey struct{}

// WithStreamEndToken marks the frame streams opened on ctx as a recording's
// whose token is tok (StreamEndTokenHeader).
func WithStreamEndToken(ctx context.Context, tok string) context.Context {
	return context.WithValue(ctx, streamEndTokenKey{}, tok)
}

// StreamEndToken is the recording token of the frame streams opened on ctx
// (WithStreamEndToken): "" when none.
func StreamEndToken(ctx context.Context) string {
	t, _ := ctx.Value(streamEndTokenKey{}).(string)
	return t
}

// StreamEndKind is the kind of the marker that ends an agent's mock stream
// (StreamEndPath): a control frame, no mock, never recorded, and no
// dependency kind (mock.go's kinds).
const StreamEndKind Kind = "KeployStreamEnd"

// StreamEndPart is the form name of the multipart part that ends an agent's
// test-case stream (StreamEndPath). Its body is the session's StreamEndSummary.
const StreamEndPart = "stream-end"

// StreamEndSummary is the body of the test-case stream's end marker
// (StreamEndPart): what the agent's summary of the session says it lost, for
// its client to say in its own log. An agent docker compose runs logs through
// compose, which can drop its last lines as the stack stops: the client tears
// the stack down once its drains end at the markers.
type StreamEndSummary struct {
	TestCasesSent       int `json:"tcs_sent_to_cli"`
	TestCasesSuppressed int `json:"tcs_suppressed_total"`
	// LeftOutInFlight: test cases left out because each stayed in flight
	// while mocks it may have made had to be let go.
	LeftOutInFlight uint64 `json:"tcs_left_out_in_flight"`
	// Unsettled: test cases saved before the agent had parsed all the
	// traffic they overlap.
	Unsettled int `json:"tcs_released_unsettled"`
	// MassSuppressed: a large share of the session's test cases was
	// suppressed (the agent's summary says so at WARN).
	MassSuppressed bool `json:"mass_suppressed"`
}

// FrameStream names one of an agent's frame streams to a recording's client.
type FrameStream string

const (
	TestCaseStream FrameStream = "incoming"
	MockStream     FrameStream = "outgoing"
	MappingStream  FrameStream = "mappings"
)

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

// DrainsOnAgentStopHeader marks an agent frame stream (test cases, mocks,
// mappings) whose client drains it once the agent says a stop signal reached
// it (StopNoticePath), and closes it then: something else stops this agent
// (docker compose, with the app's project, also when the app exits on its
// own), and the client is not told otherwise before the agent goes. A stop
// signal the agent gets while such a stream has carried a frame, or the
// agent holds one for it, waits for the stream to close, as for a
// DrainsAfterStopHeader stream: the client, told, drains what the agent
// captured before the stop. An agent with nothing to hand over does not wait.
// `keploy record` sends it under docker compose, and watches StopNoticePath.
const DrainsOnAgentStopHeader = "Keploy-Drains-On-Agent-Stop"

// StopNoticePath is the agent route (GET) a recording's client of an agent
// something else stops waits on (DrainsOnAgentStopHeader): it answers 204 once
// a stop signal has reached the agent and the agent waits for the client to
// drain its streams. 404 from an agent that predates it.
const StopNoticePath = "/record/stopping"

type agentStopNoticeKey struct{}

// WithAgentStopNotice marks the agent frame streams opened on ctx as ones
// their client drains when the agent says a stop reached it
// (DrainsOnAgentStopHeader): the client watches StopNoticePath meanwhile.
func WithAgentStopNotice(ctx context.Context) context.Context {
	return context.WithValue(ctx, agentStopNoticeKey{}, true)
}

// AgentStopNotice reports whether ctx was marked with WithAgentStopNotice.
func AgentStopNotice(ctx context.Context) bool {
	v, _ := ctx.Value(agentStopNoticeKey{}).(bool)
	return v
}
