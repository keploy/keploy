package generic

import (
	"context"
	"encoding/base64"
	"fmt"
	"math"

	"go.keploy.io/server/v2/pkg/core/hooks"
	"go.keploy.io/server/v2/pkg/core/proxy/integrations/util"
	"go.keploy.io/server/v2/pkg/models"
)

func fuzzymatch(ctx context.Context, reqBuff [][]byte, h *hooks.Hook) (bool, []models.GenericPayload, error) {
	for {
		tcsMocks, err := h.GetConfigMocks()
		if err != nil {
			return false, nil, fmt.Errorf("error while getting tcs mocks %v", err)
		}

		var filteredMocks []*models.Mock
		var unfilteredMocks []*models.Mock

		for _, mock := range tcsMocks {
			if mock.TestModeInfo.IsFiltered {
				filteredMocks = append(filteredMocks, mock)
			} else {
				unfilteredMocks = append(unfilteredMocks, mock)
			}
		}

		index := -1
		for idx, mock := range filteredMocks {
			if len(mock.Spec.GenericRequests) == len(reqBuff) {
				matched := true // Flag to track if all requests match

				for requestIndex, reqBuff := range reqBuff {

					bufStr := string(reqBuff)
					if !util.IsAsciiPrintable(string(reqBuff)) {
						bufStr = base64.StdEncoding.EncodeToString(reqBuff)
					}

					// Compare the encoded data
					if mock.Spec.GenericRequests[requestIndex].Message[0].Data != bufStr {
						matched = false
						break // Exit the loop if any request doesn't match
					}
				}
				if matched {
					index = idx
					break
				}
			}
		}

		if index != -1 {
			responseMock := make([]models.GenericPayload, len(filteredMocks[index].Spec.GenericResponses))
			copy(responseMock, filteredMocks[index].Spec.GenericResponses)
			originalFilteredMock := *filteredMocks[index]
			filteredMocks[index].TestModeInfo.IsFiltered = false
			filteredMocks[index].TestModeInfo.SortOrder = math.MaxInt64
			isUpdated := h.UpdateConfigMock(&originalFilteredMock, filteredMocks[index])
			if !isUpdated {
				continue
			}
			return true, responseMock, nil
		}

		if index == -1 {
			index = findBinaryMatch(filteredMocks, reqBuff)
		}

		if index != -1 {
			responseMock := make([]models.GenericPayload, len(filteredMocks[index].Spec.GenericResponses))
			copy(responseMock, filteredMocks[index].Spec.GenericResponses)
			originalFilteredMock := *filteredMocks[index]
			filteredMocks[index].TestModeInfo.IsFiltered = false
			filteredMocks[index].TestModeInfo.SortOrder = math.MaxInt64
			isUpdated := h.UpdateConfigMock(&originalFilteredMock, filteredMocks[index])
			if isUpdated {
				continue
			}
			return true, responseMock, nil
		}

		if index == -1 {
			index = findBinaryMatch(unfilteredMocks, requestBuffers)
		}

		if index != -1 {
			responseMock := make([]models.GenericPayload, len(unfilteredMocks[index].Spec.GenericResponses))
			copy(responseMock, unfilteredMocks[index].Spec.GenericResponses)
			return true, responseMock, nil
		}
		break
	}
	return false, nil, nil
}

func findBinaryMatch(tcsMocks []*models.Mock, reqBuffs [][]byte) int {
	// TODO: need find a proper similarity index to set a benchmark for matching or need to find another way to do approximate matching
	mxSim := 0.5
	mxIdx := -1
	for idx, mock := range tcsMocks {
		if len(mock.Spec.GenericRequests) == len(reqBuffs) {
			for requestIndex, reqBuff := range reqBuffs {
				_ = base64.StdEncoding.EncodeToString(reqBuff)
				encoded, _ := util.DecodeBase64(mock.Spec.GenericRequests[requestIndex].Message[0].Data)

				k := util.AdaptiveK(len(reqBuff), 3, 8, 5)
				shingles1 := util.CreateShingles(encoded, k)
				shingles2 := util.CreateShingles(reqBuff, k)
				similarity := util.JaccardSimilarity(shingles1, shingles2)

				if mxSim < similarity {
					mxSim = similarity
					mxIdx = idx
				}
			}
		}
	}
	return mxIdx
}
