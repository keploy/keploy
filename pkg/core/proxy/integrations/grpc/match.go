package grpc

import (
	"context"
	"fmt"

	"github.com/agnivade/levenshtein"
	"go.keploy.io/server/v2/pkg/core/proxy/integrations"
	"go.keploy.io/server/v2/pkg/core/proxy/integrations/util"
	"go.uber.org/zap"

	"go.keploy.io/server/v2/pkg/models"
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

func FilterMocksBasedOnGrpcRequest(ctx context.Context, logger *zap.Logger, grpcReq models.GrpcReq, mockDb integrations.MockMemDb) (*models.Mock, error) {
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

			if len(grpcMocks) == 0 {
				logger.Debug("No grpc mocks found in the db")
				return nil, nil
			}

			logger.Debug("Here are the grpc mocks in the db", zap.Int("len", len(grpcMocks)))

			schemaMatched, err := schemaMatch(ctx, grpcReq, grpcMocks)
			if err != nil {
				return nil, err
			}

			if len(schemaMatched) == 0 {
				logger.Debug("No mock found with schema match")
				return nil, nil
			}

			logger.Debug("Here are the grpc mocks with schema match", zap.Int("len", len(schemaMatched)))

			// Exact body Match
			ok, matchedMock := exactBodyMatch(grpcReq.Body, schemaMatched)
			if ok {
				logger.Debug("Exact body match found", zap.Any("matchedMock", matchedMock))
				if !mockDb.DeleteFilteredMock(*matchedMock) {
					continue
				}
				return matchedMock, nil
			}

			// apply fuzzy match for body with schemaMatched mocks

			logger.Debug("Performing fuzzy match for decoded data in body")
			// Perform fuzzy match on the request
			isMatched, bestMatch := fuzzyMatch(schemaMatched, grpcReq.Body.DecodedData)
			if isMatched {
				if !mockDb.DeleteFilteredMock(*bestMatch) {
					continue
				}
				return bestMatch, nil
			}
			return nil, nil
		}
	}
}

func schemaMatch(ctx context.Context, req models.GrpcReq, mocks []*models.Mock) ([]*models.Mock, error) {
	var schemaMatched []*models.Mock

	for _, mock := range mocks {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		mockReq := mock.Spec.GRPCReq

		// the pseudo headers should definitely match (:method, :path, etc.).
		if !compareMap(mockReq.Headers.PseudoHeaders, req.Headers.PseudoHeaders) {
			continue
		}

		// the ordinary headers keys should match.
		if !compareMapKeys(mockReq.Headers.OrdinaryHeaders, req.Headers.OrdinaryHeaders) {
			continue
		}

		// the content type should match.
		if mockReq.Headers.OrdinaryHeaders["content-type"] != req.Headers.OrdinaryHeaders["content-type"] {
			continue
		}

		schemaMatched = append(schemaMatched, mock)
	}

	return schemaMatched, nil
}

// Check if two maps have the same keys, ignoring values.
func compareMapKeys(m1, m2 map[string]string) bool {
	if len(m1) != len(m2) {
		return false
	}
	for k := range m1 {
		if _, ok := m2[k]; !ok {
			return false
		}
	}
	return true
}

// Check if two maps are identical.
func compareMap(m1, m2 map[string]string) bool {
	if len(m1) != len(m2) {
		return false
	}
	for k, v := range m1 {
		if v2, ok := m2[k]; !ok || v != v2 {
			return false
		}
	}
	return true
}

func exactBodyMatch(body models.GrpcLengthPrefixedMessage, schemaMatched []*models.Mock) (bool, *models.Mock) {
	for _, mock := range schemaMatched {
		// The new implementation might not reconstruct the exact original prefix,
		// so we match on the decoded data which is more reliable.
		if mock.Spec.GRPCReq.Body.DecodedData == body.DecodedData {
			return true, mock
		}
	}
	return false, nil
}

// fuzzyMatch logic remains the same.
func findStringMatch(req string, mockStrings []string) int {
	minDist := int(^uint(0) >> 1)
	bestMatch := -1
	for idx, mock := range mockStrings {
		if !util.IsASCII(mock) {
			continue
		}
		dist := levenshtein.ComputeDistance(req, mock)
		if dist == 0 {
			return 0
		}
		if dist < minDist {
			minDist = dist
			bestMatch = idx
		}
	}
	return bestMatch
}

func findBinaryMatch(mocks []*models.Mock, reqBuff []byte) int {
	mxSim := -1.0
	mxIdx := -1
	for idx, mock := range mocks {
		encoded := []byte(mock.Spec.GRPCReq.Body.DecodedData)
		k := util.AdaptiveK(len(reqBuff), 3, 8, 5)
		shingles1 := util.CreateShingles(encoded, k)
		shingles2 := util.CreateShingles(reqBuff, k)
		similarity := util.JaccardSimilarity(shingles1, shingles2)

		if mxSim < similarity {
			mxSim = similarity
			mxIdx = idx
		}
	}
	return mxIdx
}

func fuzzyMatch(tcsMocks []*models.Mock, reqBuff string) (bool, *models.Mock) {
	mockStrings := make([]string, len(tcsMocks))
	for i := range tcsMocks {
		mockStrings[i] = tcsMocks[i].Spec.GRPCReq.Body.DecodedData
	}

	if util.IsASCII(reqBuff) {
		idx := findStringMatch(string(reqBuff), mockStrings)
		if idx != -1 {
			return true, tcsMocks[idx]
		}
	}

	idx := findBinaryMatch(tcsMocks, []byte(reqBuff))
	if idx != -1 {
		return true, tcsMocks[idx]
	}
	return false, nil
}
