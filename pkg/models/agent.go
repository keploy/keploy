package models

import (
	"time"
)

type OutgoingReq struct {
	OutgoingOptions OutgoingOptions `json:"outgoingOptions"`
}

// MockFrame carries a recorded mock with its active session context.
// ScopeFilePath stores the current sandbox scope file path.
// It is transient stream metadata and is not persisted into mock files.
type MockFrame struct {
	ScopeFilePath string `json:"scopeFilePath,omitempty"`
	Mock          *Mock  `json:"mock"`
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

type MockFilterParams struct {
	AfterTime          time.Time            `json:"afterTime,omitempty"`
	BeforeTime         time.Time            `json:"beforeTime,omitempty"`
	MockMapping        []string             `json:"mockMapping,omitempty"`
	UseMappingBased    bool                 `json:"useMappingBased"`
	TotalConsumedMocks map[string]MockState `json:"totalConsumedMocks,omitempty"`
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

type SandboxScopeRequest struct {
	// Location is the directory where sandbox file will be written/read.
	Location string `json:"location"`
	// Name is the sandbox file name prefix (without extension).
	Name string `json:"name"`
}

type BeforeTestRunReq struct {
	TestRunID string `json:"testRunID"`
}

type BeforeTestSetCompose struct {
	TestRunID string `json:"testRunID"`
}

type AfterTestRunReq struct {
	TestRunID  string       `json:"testRunID"`
	TestSetIDs []string     `json:"testSetIDs"`
	Coverage   TestCoverage `json:"coverage"`
}
