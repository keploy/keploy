package spec

import (
	"time"

	"go.keploy.io/server/pkg/models"
)

type HttpSpec struct {
	Metadata         map[string]string      `json:"metadata" yaml:"metadata"`
	Request          models.HttpReq         `json:"req" yaml:"req"`
	Response         models.HttpResp        `json:"resp" yaml:"resp"`
	Objects          []*models.OutputBinary `json:"objects" yaml:"objects"`
	Assertions       map[string]map[string][]string   `json:"assertions" yaml:"assertions,omitempty"`
	Created          int64                  `json:"created" yaml:"created,omitempty"`
	ReqTimestampMock time.Time              `json:"reqTimestampMock" yaml:"reqTimestampMock,omitempty"`
	ResTimestampMock time.Time              `json:"resTimestampMock" yaml:"resTimestampMock,omitempty"`
}