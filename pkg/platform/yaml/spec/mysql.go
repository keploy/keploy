package spec

import (
	"go.keploy.io/server/pkg/models"
	"gopkg.in/yaml.v3"
)

type MySQLSpec struct {
	Metadata  map[string]string   `json:"metadata" yaml:"metadata"`
	Requests  []MysqlRequestYaml  `json:"requests" yaml:"requests"`
	Response  []MysqlResponseYaml `json:"responses" yaml:"responses"`
	CreatedAt int64               `json:"created" yaml:"created,omitempty"`
}

type MysqlRequestYaml struct {
	Header    *models.MySQLPacketHeader `json:"header,omitempty" yaml:"header"`
	Message   yaml.Node                 `json:"message,omitempty" yaml:"message"`
	ReadDelay int64                     `json:"read_delay,omitempty" yaml:"read_delay,omitempty"`
}

type MysqlResponseYaml struct {
	Header    *models.MySQLPacketHeader `json:"header,omitempty" yaml:"header"`
	Message   yaml.Node                 `json:"message,omitempty" yaml:"message"`
	ReadDelay int64                     `json:"read_delay,omitempty" yaml:"read_delay,omitempty"`
}
