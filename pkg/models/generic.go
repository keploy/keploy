package models

import (
	"net/http"
	"time"

	"go.uber.org/zap"
)

type GenericSchema struct {
	Metadata         map[string]string `json:"metadata" yaml:"metadata"`
	GenericRequests  []GenericPayload  `json:"RequestBin,omitempty"`
	GenericResponses []GenericPayload  `json:"ResponseBin,omitempty"`
	ReqTimestampMock time.Time         `json:"reqTimestampMock,omitempty"`
	ResTimestampMock time.Time         `json:"resTimestampMock,omitempty"`
}

type Telemetry struct {
	Enabled        bool
	OffMode        bool
	logger         *zap.Logger
	InstallationID string
	store          FS
	KeployVersion  string
	GlobalMap      map[string]interface{}
	client         *http.Client
}
