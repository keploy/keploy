package models

import "time"

type OutgoingReq struct {
	OutgoingOptions OutgoingOptions `json:"outgoingOptions"`
}

type IncomingReq struct {
	IncomingOptions IncomingOptions `json:"incomingOptions"`
}

type RegisterReq struct {
	SetupOptions SetupOptions `json:"setupOptions"`
}

type AgentResp struct {
	Error     error  `json:"error"`
	IsSuccess bool   `json:"isSuccess"`
}

type RunReq struct {
	RunOptions RunOptions `json:"runOptions"`
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

type UnregisterReq struct {
	Mode     Mode   `json:"mode"`
}
