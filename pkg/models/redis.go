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

type PulsarSchema struct {
	Metadata         map[string]string `json:"metadata" yaml:"metadata"`
	PulsarRequests   []Payload         `json:"RequestBin,omitempty" yaml:"RequestBin,omitempty"`
	PulsarResponses  []Payload         `json:"ResponseBin,omitempty" yaml:"ResponseBin,omitempty"`
	ReqTimestampMock time.Time         `json:"reqTimestampMock,omitempty" yaml:"reqTimestampMock,omitempty"`
	ResTimestampMock time.Time         `json:"resTimestampMock,omitempty" yaml:"resTimestampMock,omitempty"`
}

type AerospikeSchema struct {
	Metadata             map[string]string `json:"metadata" yaml:"metadata"`
	AerospikeRequests    []Payload         `json:"RequestBin,omitempty" yaml:"RequestBin,omitempty"`
	AerospikeResponses   []Payload         `json:"ResponseBin,omitempty" yaml:"ResponseBin,omitempty"`
	ReqTimestampMock     time.Time         `json:"reqTimestampMock,omitempty" yaml:"reqTimestampMock,omitempty"`
	ResTimestampMock     time.Time         `json:"resTimestampMock,omitempty" yaml:"resTimestampMock,omitempty"`
}
