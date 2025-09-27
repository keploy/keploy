package models

import "time"

type OutgoingReq struct {
	OutgoingOptions OutgoingOptions `json:"outgoingOptions"`
	ClientID        uint64          `json:"clientId"`
}

type IncomingReq struct {
	IncomingOptions IncomingOptions `json:"incomingOptions"`
	ClientID        uint64          `json:"clientId"`
}

type RegisterReq struct {
	SetupOptions SetupOptions `json:"setupOptions"`
}

type AgentResp struct {
	ClientID  uint64 `json:"clientID"` // uuid of the app
	Error     error  `json:"error"`
	IsSuccess bool   `json:"isSuccess"`
}

type RunReq struct {
	RunOptions RunOptions `json:"runOptions"`
	ClientID   uint64     `json:"clientId"`
}

type SetMocksReq struct {
	Filtered   []*Mock `json:"filtered"`
	UnFiltered []*Mock `json:"unFiltered"`
	ClientID   uint64  `json:"clientId"`
}

type StoreMocksReq struct {
	Filtered   []*Mock `json:"filtered"`
	UnFiltered []*Mock `json:"unFiltered"`
	ClientID   uint64  `json:"clientId"`
}

type MockFilterParams struct {
	AfterTime          time.Time            `json:"afterTime,omitempty"`
	BeforeTime         time.Time            `json:"beforeTime,omitempty"`
	MockMapping        []string             `json:"mockMapping,omitempty"`
	UseMappingBased    bool                 `json:"useMappingBased"`
	TotalConsumedMocks map[string]MockState `json:"totalConsumedMocks,omitempty"`
}

type UpdateMockParamsReq struct {
	ClientID     uint64           `json:"clientId"`
	FilterParams MockFilterParams `json:"filterParams"`
}

type UnregisterReq struct {
	ClientID uint64 `json:"clientId"`
	Mode     Mode   `json:"mode"`
}
