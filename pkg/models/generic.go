package models

import (
	"time"
)

type GenericSchema struct {
	Metadata         map[string]string `json:"metadata" yaml:"metadata"`
	GenericRequests  []Payload         `json:"RequestBin,omitempty" yaml:"RequestBin,omitempty"`
	GenericResponses []Payload         `json:"ResponseBin,omitempty" yaml:"ResponseBin,omitempty"`
	ReqTimestampMock time.Time         `json:"reqTimestampMock,omitempty" yaml:"reqTimestampMock,omitempty"`
	ResTimestampMock time.Time         `json:"resTimestampMock,omitempty" yaml:"resTimestampMock,omitempty"`
}
