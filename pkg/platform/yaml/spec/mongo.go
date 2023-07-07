package spec

import (
	"go.keploy.io/server/pkg/models"
	"gopkg.in/yaml.v3"
)

type MongoSpec struct {
	Metadata  map[string]string `json:"metadata" yaml:"metadata"`
	Requests  []RequestYaml     `json:"requests" yaml:"requests"`
	Response  []ResponseYaml    `json:"responses" yaml:"responses"`
	CreatedAt int64             `json:"created" yaml:"created,omitempty"`
	// RequestHeader  models.MongoHeader `json:"request_mongo_header" yaml:"request_mongo_header"`
	// ResponseHeader models.MongoHeader `json:"response_mongo_header" yaml:"response_mongo_header"`
	// Request        yaml.Node          `json:"mongo_request" yaml:"mongo_request"`
	// Response       yaml.Node          `json:"mongo_response" yaml:"mongo_response"`
}

type RequestYaml struct {
	Header    *models.MongoHeader `json:"header,omitempty" yaml:"header"`
	Message   yaml.Node           `json:"message,omitempty" yaml:"message"`
	ReadDelay int64               `json:"read_delay,omitempty" yaml:"read_delay,omitempty"`
}

type ResponseYaml struct {
	Header  *models.MongoHeader `json:"header,omitempty" yaml:"header"`
	Message yaml.Node           `json:"message,omitempty" yaml:"message"`
	ReadDelay int64               `json:"read_delay,omitempty" yaml:"read_delay,omitempty"`
}