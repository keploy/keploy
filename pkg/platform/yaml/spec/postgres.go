package spec

import (
	"time"

	"go.keploy.io/server/pkg/models"
)

type PostgresSpec struct {
	Metadata map[string]string `json:"metadata" yaml:"metadata"`

	// Objects  []*models.OutputBinary          `json:"objects" yaml:"objects"`
	PostgresRequests  []models.Backend  `json:"RequestBin,omitempty"`
	PostgresResponses []models.Frontend `json:"ResponseBin,omitempty"`

	ReqTimestampMock time.Time `json:"ReqTimestampMock,omitempty"`
	ResTimestampMock time.Time `json:"ResTimestampMock,omitempty"`
}
