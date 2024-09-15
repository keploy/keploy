package models

type OutgoingReq struct {
	OutgoingOptions OutgoingOptions `json:"outgoingOptions"`
	AppId           int64           `json:"appId"`
}

type IncomingReq struct {
	IncomingOptions IncomingOptions `json:"incomingOptions"`
	AppId           int64           `json:"appId"`
}

type RegisterReq struct {
	SetupOptions SetupOptions `json:"setupOptions"`
}

type RegisterResp struct {
	AppId      uint64 `json:"appId"` // uuid of the app
	IsRunnning bool   `json:"isRunning"`
}

type RunReq struct {
	RunOptions RunOptions `json:"runOptions"`
	AppId      int64      `json:"appId"`
}

type SetMocksReq struct {
	Filtered   []*Mock `json:"filtered"`
	UnFiltered []*Mock `json:"unFiltered"`
	AppId      int64   `json:"appId"`
}
