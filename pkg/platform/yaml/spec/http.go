package spec

import "go.keploy.io/server/pkg/models"

type HttpSpec struct {
	Metadata   map[string]string   `json:"metadata" yaml:"metadata"`
	Request    models.HttpReq         `json:"req" yaml:"req"`
	Response   models.HttpResp        `json:"resp" yaml:"resp"`
	Objects    []*models.OutputBinary            `json:"objects" yaml:"objects"`
	Assertions map[string][]string `json:"assertions" yaml:"assertions,omitempty"`
	Created    int64               `json:"created" yaml:"created,omitempty"`
}