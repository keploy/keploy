package models

type OutgoingReq struct {
	OutgoingOptions OutgoingOptions `json:"outgoingOptions"`
	AppId           int64           `json:"appId"`
}

type IncomingReq struct {
	IncomingOptions IncomingOptions `json:"incomingOptions"`
	AppId           int64           `json:"appId"`
}

type SetupReq struct {
	SetupOptions SetupOptions `json:"setupOptions"`
	AppId        int64        `json:"appId"`
}

type SetupResp struct {
	AppId      uint64 `json:"appId"` // uuid of the app
	IsRunnning bool   `json:"isRunning"`
}

type RunReq struct {
	RunOptions RunOptions `json:"runOptions"`
	AppId      int64      `json:"appId"`
}
