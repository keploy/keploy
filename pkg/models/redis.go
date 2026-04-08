package models

import (
	"time"
)

type RedisSchema struct {
	Metadata         map[string]string `json:"metadata" yaml:"metadata"`
	Noise            []string          `json:"noise,omitempty" yaml:"noise,omitempty"`
	RedisRequests    []Payload         `json:"RequestBin,omitempty"`
	RedisResponses   []Payload         `json:"ResponseBin,omitempty"`
	ReqTimestampMock time.Time         `json:"reqTimestampMock,omitempty"`
	ResTimestampMock time.Time         `json:"resTimestampMock,omitempty"`
}

type KafkaSchema struct {
	Metadata         map[string]string `json:"metadata" yaml:"metadata"`
	Noise            []string          `json:"noise,omitempty" yaml:"noise,omitempty"`
	KafkaRequests    []Payload         `json:"RequestBin,omitempty"`
	KafkaResponses   []Payload         `json:"ResponseBin,omitempty"`
	ReqTimestampMock time.Time         `json:"reqTimestampMock,omitempty"`
	ResTimestampMock time.Time         `json:"resTimestampMock,omitempty"`
}
