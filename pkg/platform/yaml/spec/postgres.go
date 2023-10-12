package spec

import "go.keploy.io/server/pkg/models"

type PostgresSpec struct {

	Metadata map[string]string `json:"metadata" yaml:"metadata"`
	PostgresRequests  []models.GenericPayload `json:"RequestBin,omitempty"`
	PostgresResponses []models.GenericPayload `json:"ResponseBin,omitempty"`

}
