//go:build linux

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
				logger.Warn("No grpc mocks found in the db")
				return nil, nil
			}

			logger.Warn("Here are the grpc mocks in the db", zap.Int("len", len(grpcMocks)))

			schemaMatched, err := schemaMatch(ctx, grpcReq, grpcMocks)
			if err != nil {
				return nil, err
			}

			if len(schemaMatched) == 0 {
				logger.Warn("No mock found with schema match")
				return nil, nil
			}

			logger.Warn("Here are the grpc mocks with schema match", zap.Int("len", len(schemaMatched)))

			// Exact body Match
			ok, matchedMock := exactBodyMatch(grpcReq.Body, schemaMatched)
			if ok {
				logger.Warn("Exact body match found")
				if !mockDb.DeleteFilteredMock(*matchedMock) {
					continue
				}
				return matchedMock, nil
			}

			// apply fuzzy match for body with schemaMatched mocks

			logger.Warn("Performing fuzzy match for decoded data in body")
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

		// the pseudo headers should defintely match.
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

		// additionally check for the compression flag here only
		if mockReq.Body.CompressionFlag != req.Body.CompressionFlag {
			continue
		}

		schemaMatched = append(schemaMatched, mock)
	}

	return schemaMatched, nil
}

// Check if two maps have the same keys
func compareMapKeys(m1, m2 map[string]string) bool {
	if len(m1) > len(m2) {
		for k := range m2 {
			if _, ok := m1[k]; !ok {
				return false
			}
		}
	} else {
		for k := range m1 {
			if _, ok := m2[k]; !ok {
				return false
			}
		}
	}
	return true
}

// Check if two maps are identical
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
		if mock.Spec.GRPCReq.Body.MessageLength == body.MessageLength && mock.Spec.GRPCReq.Body.DecodedData == body.DecodedData {
			return true, mock
		}
	}
	return false, nil
}

// Fuzzy match helper for string matching
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

// TODO: generalize the function to work with any type of integration
func findBinaryMatch(mocks []*models.Mock, reqBuff []byte) int {

	mxSim := -1.0
	mxIdx := -1
	// find the fuzzy hash of the mocks
	for idx, mock := range mocks {
		encoded := []byte(mock.Spec.GRPCReq.Body.DecodedData)
		k := util.AdaptiveK(len(reqBuff), 3, 8, 5)
		shingles1 := util.CreateShingles(encoded, k)
		shingles2 := util.CreateShingles(reqBuff, k)
		similarity := util.JaccardSimilarity(shingles1, shingles2)

		// log.Debugf("Jaccard Similarity:%f\n", similarity)

		if mxSim < similarity {
			mxSim = similarity
			mxIdx = idx
		}
	}
	return mxIdx
}

// fuzzy match on the request
func fuzzyMatch(tcsMocks []*models.Mock, reqBuff string) (bool, *models.Mock) {

	// String-based fuzzy matching
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
