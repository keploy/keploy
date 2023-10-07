package spec

import "go.keploy.io/server/pkg/models"

type PostgresSpec struct {

	// PostgresReq models.Backend `json:"postgresReq" yaml:"postgresReq"`
	// PostgresResp models.Frontend `json:"postgresResp" yaml:"postgresResp"`
	Metadata map[string]string `json:"metadata" yaml:"metadata"`
	// Objects  []*models.OutputBinary          `json:"objects" yaml:"objects"`
	PostgresRequests  []models.Backend `json:"RequestBin,omitempty"`
	PostgresResponses []models.Frontend `json:"ResponseBin,omitempty"`

}
