package spec

import (
	"time"

	"go.keploy.io/server/v2/pkg/models"
)

type GrpcSpec struct {
	GrpcReq          models.GrpcReq  `json:"grpcReq" yaml:"grpcReq"`
	GrpcResp         models.GrpcResp `json:"grpcResp" yaml:"grpcResp"`
	ReqTimestampMock time.Time       `json:"reqTimestampMock" yaml:"reqTimestampMock,omitempty"`
	ResTimestampMock time.Time       `json:"resTimestampMock" yaml:"resTimestampMock,omitempty"`
}
