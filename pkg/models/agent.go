package models

import (
	"time"
)

type OutgoingReq struct {
	OutgoingOptions OutgoingOptions `json:"outgoingOptions"`
}

type IncomingReq struct {
	IncomingOptions IncomingOptions `json:"incomingOptions"`
}

type AgentResp struct {
	Error     error `json:"error"`
	IsSuccess bool  `json:"isSuccess"`
}

type TestMockMapping struct {
	TestName string   `json:"test_name"`
	MockIDs  []string `json:"mock_ids"`
}

type SetMocksReq struct {
	Filtered   []*Mock `json:"filtered"`
	UnFiltered []*Mock `json:"unFiltered"`
}

type StoreMocksReq struct {
	Filtered   []*Mock `json:"filtered"`
	UnFiltered []*Mock `json:"unFiltered"`
}

// StoreMocksStreamContentType labels the /storemocks request body: a gob
// MockStreamHeader followed by one gob-encoded Mock per frame. The agent and
// the client that talks to it ship in lockstep (OSS bump → agent release →
// k8s-proxy release), so this is the ONLY StoreMocks wire format — there is no
// legacy whole-dump fallback and no capability negotiation.
const StoreMocksStreamContentType = "application/x-gob-stream"

// MockStreamHeader is the first gob value on a /storemocks body. The counts let
// the agent pre-size its slices exactly (peak ≈ 1× corpus, no append-doubling)
// and derive bucket membership positionally: the first FilteredCount mock
// values are filtered, the next UnfilteredCount are unfiltered. EOF ends it.
type MockStreamHeader struct {
	FilteredCount   int `json:"filteredCount"`
	UnfilteredCount int `json:"unfilteredCount"`
}

type MockFilterParams struct {
	AfterTime          time.Time            `json:"afterTime,omitempty"`
	BeforeTime         time.Time            `json:"beforeTime,omitempty"`
	MockMapping        []string             `json:"mockMapping,omitempty"`
	UseMappingBased    bool                 `json:"useMappingBased"`
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
