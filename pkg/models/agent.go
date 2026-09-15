package models

import (
	"encoding/json"
	"errors"
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
	PID uint32 `json:"pid,omitempty"`
}

// ScopeTableReq is the body of POST /agent/scope/table — the replay CLI hands
// the agent the per-test name→mock-names table (from mappings.yaml) so the
// runner's /agent/scope/begin calls can restrict the served pool per test.
type ScopeTableReq struct {
	Mappings map[string][]string `json:"mappings"`
}

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
	BeforeTime             time.Time `json:"beforeTime,omitempty"`
	MockMapping            []string  `json:"mockMapping,omitempty"`
	UseMappingBased        bool      `json:"useMappingBased"`
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
}

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
