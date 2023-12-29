package grpcparser

import (
	"fmt"

	"go.keploy.io/server/pkg/hooks"
	"go.keploy.io/server/pkg/models"
)

const (
	KLabelForAuthority = ":authority"
	KLabelForMethod    = ":method"
	KLabelForPath      = ":path"
	KLabelForScheme    = ":http"

	KLabelForContentType = "content-type"
)

func FilterMocksRelatedToGrpc(mocks []*models.Mock) []*models.Mock {
	var res []*models.Mock
	for _, mock := range mocks {
		if mock != nil && mock.Kind == models.GRPC_EXPORT && mock.Spec.GRPCReq != nil && mock.Spec.GRPCResp != nil {
			res = append(res, mock)
		}
	}
	return res
}

func FilterMocksBasedOnGrpcRequest(grpcReq models.GrpcReq, hook *hooks.Hook) (*models.Mock, error) {
	for {
		mocks, err := hook.GetTcsMocks()
		if err != nil {
			return nil, fmt.Errorf("error while getting tsc mocks %v", err)
		}

		var matchedMock *models.Mock
		var isMatched bool

		grpcMocks := FilterMocksRelatedToGrpc(mocks)
		for _, mock := range grpcMocks {
			have := mock.Spec.GRPCReq
			// Investigate pseudo headers.
			if have.Headers.PseudoHeaders[KLabelForAuthority] != grpcReq.Headers.PseudoHeaders[KLabelForAuthority] {
				continue
			}
			if have.Headers.PseudoHeaders[KLabelForMethod] != grpcReq.Headers.PseudoHeaders[KLabelForMethod] {
				continue
			}
			if have.Headers.PseudoHeaders[KLabelForPath] != grpcReq.Headers.PseudoHeaders[KLabelForPath] {
				continue
			}
			if have.Headers.PseudoHeaders[KLabelForScheme] != grpcReq.Headers.PseudoHeaders[KLabelForScheme] {
				continue
			}

			// Investigate ordinary headers.
			if have.Headers.OrdinaryHeaders[KLabelForContentType] != grpcReq.Headers.OrdinaryHeaders[KLabelForContentType] {
				continue
			}

			// Investigate the compression flag.
			if have.Body.CompressionFlag != grpcReq.Body.CompressionFlag {
				continue
			}

			// Investigate the body.
			if have.Body.DecodedData != grpcReq.Body.DecodedData {
				continue
			}

			matchedMock = mock
			isMatched = true
			break
		}

		if isMatched {
			isDeleted, err := hook.DeleteTcsMock(matchedMock)
			if err != nil {
				return nil, fmt.Errorf("error while deleting tcs mock: %v", err)
			}
			if !isDeleted {
				continue
			}
		}
		return nil, nil
	}
}
