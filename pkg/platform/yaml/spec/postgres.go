package spec

import "go.keploy.io/server/pkg/models"

type PostgresSpec struct {

	Metadata map[string]string `json:"metadata" yaml:"metadata"`

	// Objects  []*models.OutputBinary          `json:"objects" yaml:"objects"`
	PostgresRequests  []models.Backend `json:"RequestBin,omitempty"`
	PostgresResponses []models.Frontend `json:"ResponseBin,omitempty"`


}
