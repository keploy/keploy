package models

import (
	"time"
)

type GenericSchema struct {
	Metadata         map[string]string `json:"metadata" yaml:"metadata"`
	GenericRequests  []Payload         `json:"RequestBin,omitempty"`
	GenericResponses []Payload         `json:"ResponseBin,omitempty"`
	ReqTimestampMock time.Time         `json:"reqTimestampMock,omitempty"`
	ResTimestampMock time.Time         `json:"resTimestampMock,omitempty"`
}
