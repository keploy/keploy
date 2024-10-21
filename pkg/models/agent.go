package models

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
type UnregisterReq struct {
	ClientID uint64 `json:"clientId"`
}
