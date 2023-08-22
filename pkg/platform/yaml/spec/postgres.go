package spec

import "go.keploy.io/server/pkg/models"

type PostgresSpec struct {
	PostgresReq models.Backend `json:"postgresReq" yaml:"postgresReq"`
	PostgresResp models.Frontend `json:"postgresResp" yaml:"postgresResp"`
}
