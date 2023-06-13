package spec

import (
	"go.keploy.io/server/pkg/models"
	"gopkg.in/yaml.v3"
)

type MongoSpec struct {
	Metadata map[string]string `json:"metadata" yaml:"metadata"`
	RequestHeader   models.MongoHeader       `json:"request_mongo_header" yaml:"request_mongo_header"`
	ResponseHeader   models.MongoHeader       `json:"response_mongo_header" yaml:"response_mongo_header"`
	Request  yaml.Node         `json:"mongo_request" yaml:"mongo_request"`
	Response yaml.Node         `json:"mongo_response" yaml:"mongo_response"`
}