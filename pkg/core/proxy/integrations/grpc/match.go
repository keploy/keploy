package grpc

import (
	"context"
	"fmt"

	"go.keploy.io/server/v2/pkg/core/proxy/integrations"
	"go.uber.org/zap"

	"go.keploy.io/server/v2/pkg/models"
)

// constants for the pseudo headers.
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

func FilterMocksBasedOnGrpcRequest(ctx context.Context, _ *zap.Logger, grpcReq models.GrpcReq, mockDb integrations.MockMemDb) (*models.Mock, error) {
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
			mocks, err := mockDb.GetFilteredMocks()
			if err != nil {
				return nil, fmt.Errorf("error while getting tsc mocks %v", err)
			}

			var matchedMock *models.Mock
			var isMatched bool

			grpcMocks := FilterMocksRelatedToGrpc(mocks)
			for _, mock := range grpcMocks {
				if ctx.Err() != nil {
					return nil, ctx.Err()
				}
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
				isDeleted := mockDb.DeleteFilteredMock(*matchedMock)
				if !isDeleted {
					continue
				}
				return matchedMock, nil
			}
			return nil, nil
		}
	}
}
