package models

type TestBenchReq struct {
	KtclientID uint64 `json:"ktclientID"`
	KtPid      uint32 `json:"ktPid"`
	KaPid      uint32 `json:"kaPid"`
}

type TestBenchResp struct {
	IsSuccess bool   `json:"isSuccess"`
	Error     string `json:"error"`
}
