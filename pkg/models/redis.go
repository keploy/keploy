package models

import (
	"time"
)

type RedisSchema struct {
	Metadata         map[string]string `json:"metadata" yaml:"metadata"`
	RedisRequests    []Payload         `json:"RequestBin,omitempty" yaml:"RequestBin,omitempty"`
	RedisResponses   []Payload         `json:"ResponseBin,omitempty" yaml:"ResponseBin,omitempty"`
	ReqTimestampMock time.Time         `json:"reqTimestampMock,omitempty" yaml:"reqTimestampMock,omitempty"`
	ResTimestampMock time.Time         `json:"resTimestampMock,omitempty" yaml:"resTimestampMock,omitempty"`
}

type KafkaSchema struct {
	Metadata         map[string]string `json:"metadata" yaml:"metadata"`
	KafkaRequests    []Payload         `json:"RequestBin,omitempty" yaml:"RequestBin,omitempty"`
	KafkaResponses   []Payload         `json:"ResponseBin,omitempty" yaml:"ResponseBin,omitempty"`
	ReqTimestampMock time.Time         `json:"reqTimestampMock,omitempty" yaml:"reqTimestampMock,omitempty"`
	ResTimestampMock time.Time         `json:"resTimestampMock,omitempty" yaml:"resTimestampMock,omitempty"`
}
