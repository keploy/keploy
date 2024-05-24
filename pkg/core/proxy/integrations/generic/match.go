package generic

import (
	"context"
	"encoding/base64"
	"fmt"
	"math"

	"go.keploy.io/server/v2/pkg/core/proxy/integrations"

	"go.keploy.io/server/v2/pkg/core/proxy/integrations/util"
	"go.keploy.io/server/v2/pkg/models"
)

// fuzzyMatch performs a fuzzy matching algorithm to find the best matching mock for the given request.
// It takes a context, a request buffer, and a mock database as input parameters.
// The function iterates over the mocks in the database and applies the fuzzy matching algorithm to find the best match.
// If a match is found, it returns the corresponding response mock and a boolean value indicating success.
// If no match is found, it returns false and a nil response.
// If an error occurs during the matching process, it returns an error.
func fuzzyMatch(ctx context.Context, reqBuff [][]byte, mockDb integrations.MockMemDb) (bool, []models.GenericPayload, error) {
	for {
		select {
		case <-ctx.Done():
			return false, nil, ctx.Err()
		default:
			mocks, err := mockDb.GetUnFilteredMocks()
			if err != nil {
				return false, nil, fmt.Errorf("error while getting unfiltered mocks %v", err)
			}

			var filteredMocks []*models.Mock
			var unfilteredMocks []*models.Mock

			for _, mock := range mocks {
				if mock.TestModeInfo.IsFiltered {
					filteredMocks = append(filteredMocks, mock)
				} else {
					unfilteredMocks = append(unfilteredMocks, mock)
				}
			}

			index := findExactMatch(filteredMocks, reqBuff)

			if index == -1 {
				index = findBinaryMatch(filteredMocks, reqBuff, 0.9)
			}

			if index != -1 {
				responseMock := make([]models.GenericPayload, len(filteredMocks[index].Spec.GenericResponses))
				copy(responseMock, filteredMocks[index].Spec.GenericResponses)
				originalFilteredMock := *filteredMocks[index]
				filteredMocks[index].TestModeInfo.IsFiltered = false
				filteredMocks[index].TestModeInfo.SortOrder = math.MaxInt64
				isUpdated := mockDb.UpdateUnFilteredMock(&originalFilteredMock, filteredMocks[index])
				if isUpdated {
					continue
				}
				return true, responseMock, nil
			}

			index = findExactMatch(unfilteredMocks, reqBuff)

			if index != -1 {
				responseMock := make([]models.GenericPayload, len(unfilteredMocks[index].Spec.GenericResponses))
				copy(responseMock, unfilteredMocks[index].Spec.GenericResponses)
				return true, responseMock, nil
			}

			totalMocks := append(filteredMocks, unfilteredMocks...)
			index = findBinaryMatch(totalMocks, reqBuff, 0.4)

			if index != -1 {
				responseMock := make([]models.GenericPayload, len(totalMocks[index].Spec.GenericResponses))
				copy(responseMock, totalMocks[index].Spec.GenericResponses)
				originalFilteredMock := *totalMocks[index]
				if totalMocks[index].TestModeInfo.IsFiltered {
					totalMocks[index].TestModeInfo.IsFiltered = false
					totalMocks[index].TestModeInfo.SortOrder = math.MaxInt64
					isUpdated := mockDb.UpdateUnFilteredMock(&originalFilteredMock, totalMocks[index])
					if isUpdated {
						continue
					}
				}
				return true, responseMock, nil
			}
			return false, nil, nil
		}
	}
}

// TODO: need to generalize this function for different types of integrations.
func findBinaryMatch(tcsMocks []*models.Mock, reqBuffs [][]byte, mxSim float64) int {
	// TODO: need find a proper similarity index to set a benchmark for matching or need to find another way to do approximate matching
	mxIdx := -1
	for idx, mock := range tcsMocks {
		if len(mock.Spec.GenericRequests) == len(reqBuffs) {
			for requestIndex, reqBuff := range reqBuffs {
				_ = base64.StdEncoding.EncodeToString(reqBuff)
				encoded, _ := util.DecodeBase64(mock.Spec.GenericRequests[requestIndex].Message[0].Data)

				similarity := fuzzyCheck(encoded, reqBuff)

				if mxSim < similarity {
					mxSim = similarity
					mxIdx = idx
				}
			}
		}
	}
	return mxIdx
}

func fuzzyCheck(encoded, reqBuf []byte) float64 {
	k := util.AdaptiveK(len(reqBuf), 3, 8, 5)
	shingles1 := util.CreateShingles(encoded, k)
	shingles2 := util.CreateShingles(reqBuf, k)
	similarity := util.JaccardSimilarity(shingles1, shingles2)
	return similarity
}

func findExactMatch(tcsMocks []*models.Mock, reqBuffs [][]byte) int {
	for idx, mock := range tcsMocks {
		if len(mock.Spec.GenericRequests) == len(reqBuffs) {
			matched := true // Flag to track if all requests match

			for requestIndex, reqBuff := range reqBuffs {

				bufStr := string(reqBuff)
				if !util.IsASCII(string(reqBuff)) {
					bufStr = util.EncodeBase64(reqBuff)
				}

				// Compare the encoded data
				if mock.Spec.GenericRequests[requestIndex].Message[0].Data != bufStr {
					matched = false
					break // Exit the loop if any request doesn't match
				}
			}

			if matched {
				return idx
			}
		}
	}
	return -1
}
