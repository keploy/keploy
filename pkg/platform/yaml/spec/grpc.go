package spec

import "go.keploy.io/server/pkg/models"

type GrpcSpec struct {
	GrpcReq  models.GrpcReq  `json:"grpcReq" yaml:"grpcReq"`
	GrpcResp models.GrpcResp `json:"grpcResp" yaml:"grpcResp"`
}
