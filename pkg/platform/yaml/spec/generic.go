package spec

import (
	"time"

	"go.keploy.io/server/pkg/models"
)

type GenericSpec struct {
	Metadata         map[string]string       `json:"metadata" yaml:"metadata"`
	GenericRequests  []models.GenericPayload `json:"RequestBin,omitempty"`
	GenericResponses []models.GenericPayload `json:"ResponseBin,omitempty"`
	ReqTimestampMock time.Time               `json:"reqTimestampMock,omitempty"`
	ResTimestampMock time.Time               `json:"resTimestampMock,omitempty"`
}
