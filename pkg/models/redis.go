package models

import (
	"time"
)

type RedisSchema struct {
	Metadata         map[string]string `json:"metadata" yaml:"metadata"`
	RedisRequests    []Payload         `json:"RequestBin,omitempty"`
	RedisResponses   []Payload         `json:"ResponseBin,omitempty"`
	ReqTimestampMock time.Time         `json:"reqTimestampMock,omitempty"`
	ResTimestampMock time.Time         `json:"resTimestampMock,omitempty"`
}
