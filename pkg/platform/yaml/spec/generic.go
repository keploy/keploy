package spec

import "go.keploy.io/server/pkg/models"

type GenericSpec struct {
	Metadata map[string]string `json:"metadata" yaml:"metadata"`
	Objects  []*models.OutputBinary          `json:"objects" yaml:"objects"`
}