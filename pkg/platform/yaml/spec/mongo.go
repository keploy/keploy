package spec

import (
	"time"

	"go.keploy.io/server/pkg/models"
	"gopkg.in/yaml.v3"
)

type MongoSpec struct {
	Metadata         map[string]string `json:"metadata" yaml:"metadata"`
	Requests         []RequestYaml     `json:"requests" yaml:"requests"`
	Response         []ResponseYaml    `json:"responses" yaml:"responses"`
	CreatedAt        int64             `json:"created" yaml:"created,omitempty"`
	ReqTimestampMock time.Time         `json:"reqTimestampMock" yaml:"reqTimestampMock,omitempty"`
	ResTimestampMock time.Time         `json:"resTimestampMock" yaml:"resTimestampMock,omitempty"`
}

type RequestYaml struct {
	Header    *models.MongoHeader `json:"header,omitempty" yaml:"header"`
	Message   yaml.Node           `json:"message,omitempty" yaml:"message"`
	ReadDelay int64               `json:"read_delay,omitempty" yaml:"read_delay,omitempty"`
}

type ResponseYaml struct {
	Header    *models.MongoHeader `json:"header,omitempty" yaml:"header"`
	Message   yaml.Node           `json:"message,omitempty" yaml:"message"`
	ReadDelay int64               `json:"read_delay,omitempty" yaml:"read_delay,omitempty"`
}
