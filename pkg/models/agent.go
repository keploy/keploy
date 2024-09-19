package models

type OutgoingReq struct {
	OutgoingOptions OutgoingOptions `json:"outgoingOptions"`
	ClientId        int64           `json:"clientId"`
}

type IncomingReq struct {
	IncomingOptions IncomingOptions `json:"incomingOptions"`
	ClientId        int64           `json:"clientId"`
}

type RegisterReq struct {
	SetupOptions SetupOptions `json:"setupOptions"`
}

type RegisterResp struct {
	ClientId int64  `json:"clientId"` // uuid of the app
	Error    error `json:"error"`
}

type RunReq struct {
	RunOptions RunOptions `json:"runOptions"`
	ClientId   int64      `json:"clientId"`
}

type SetMocksReq struct {
	Filtered   []*Mock `json:"filtered"`
	UnFiltered []*Mock `json:"unFiltered"`
	AppId      int64   `json:"appId"`
}
