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

type KafkaSchema struct {
	Metadata         map[string]string `json:"metadata" yaml:"metadata"`
	KafkaRequests    []Payload         `json:"RequestBin,omitempty"`
	KafkaResponses   []Payload         `json:"ResponseBin,omitempty"`
	ReqTimestampMock time.Time         `json:"reqTimestampMock,omitempty"`
	ResTimestampMock time.Time         `json:"resTimestampMock,omitempty"`
}

type AerospikeSchema struct {
	Metadata             map[string]string `json:"metadata" yaml:"metadata"`
	AerospikeRequests    []Payload         `json:"aerospikeRequests,omitempty" yaml:"aerospikeRequests,omitempty"`
	AerospikeResponses   []Payload         `json:"aerospikeResponses,omitempty" yaml:"aerospikeResponses,omitempty"`
	ReqTimestampMock     time.Time         `json:"reqTimestampMock,omitempty" yaml:"reqTimestampMock,omitempty"`
	ResTimestampMock     time.Time         `json:"resTimestampMock,omitempty" yaml:"resTimestampMock,omitempty"`
}
