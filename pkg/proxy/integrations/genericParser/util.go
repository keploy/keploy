package genericparser

import (
	"encoding/base64"
	"fmt"
	"math"
	"sort"

	// "fmt"
	"unicode"

	"go.keploy.io/server/pkg/hooks"
	"go.keploy.io/server/pkg/models"
	"go.keploy.io/server/pkg/proxy/util"
)

func PostgresDecoder(encoded string) ([]byte, error) {
	// decode the base 64 encoded string to buffer ..

	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		// fmt.Println(hooks.Emoji+"failed to decode the data", err)
		return nil, err
	}
	// println("Decoded data is :", string(data))
	return data, nil
}

func fuzzymatch(requestBuffers [][]byte, h *hooks.Hook) (bool, []models.GenericPayload, error) {
	tcsMocks, err := h.GetConfigMocks()
	if err != nil {
		return false, nil, fmt.Errorf("error while getting tcs mocks %v", err)
	}

	sort.Slice(tcsMocks, func(i, j int) bool {
		sortOrderI, okI := tcsMocks[i].TestModeInfo["SortOrder"]
		sortOrderJ, okJ := tcsMocks[j].TestModeInfo["SortOrder"]
		if !okI || !okJ {
			return false
		}
		sortOrderInt64I, isInt64I := sortOrderI.(int64)
		sortOrderInt64J, isInt64J := sortOrderJ.(int64)
		if !isInt64I || !isInt64J {
			return false
		}
		return sortOrderInt64I < sortOrderInt64J
	})

	var filteredMocks []*models.Mock
	var unfilteredMocks []*models.Mock

	for _, mock := range tcsMocks {
		Filter, ok := mock.TestModeInfo["isFiltered"]
		if !ok {
			unfilteredMocks = append(unfilteredMocks, mock)
		}
		isFiltered, ok := Filter.(bool)
		if !ok {
			unfilteredMocks = append(unfilteredMocks, mock)
		}
		if isFiltered {
			filteredMocks = append(filteredMocks, mock)
		} else {
			unfilteredMocks = append(unfilteredMocks, mock)
		}
	}

	index := -1
	for idx, mock := range filteredMocks {
		if len(mock.Spec.GenericRequests) == len(requestBuffers) {
			matched := true // Flag to track if all requests match

			for requestIndex, reqBuff := range requestBuffers {

				bufStr := string(reqBuff)
				if !IsAsciiPrintable(string(reqBuff)) {
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
		filteredMocks[index].TestModeInfo["isFiltered"] = false
		filteredMocks[index].TestModeInfo["SortOrder"] = math.MaxInt64
		h.UpdateConfigMock(filteredMocks[index])
		return true, responseMock, nil
	}

	if index == -1 {
		for idx, mock := range unfilteredMocks {
			if len(mock.Spec.GenericRequests) == len(requestBuffers) {
				matched := true // Flag to track if all requests match

				for requestIndex, reqBuff := range requestBuffers {

					bufStr := string(reqBuff)
					if !IsAsciiPrintable(string(reqBuff)) {
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
	}

	if index != -1 {
		responseMock := make([]models.GenericPayload, len(unfilteredMocks[index].Spec.GenericResponses))
		copy(responseMock, unfilteredMocks[index].Spec.GenericResponses)
		return true, responseMock, nil
	}

	if index == -1 {
		index = findBinaryMatch(filteredMocks, requestBuffers, h)
	}

	if index != -1 {
		responseMock := make([]models.GenericPayload, len(filteredMocks[index].Spec.GenericResponses))
		copy(responseMock, filteredMocks[index].Spec.GenericResponses)
		filteredMocks[index].TestModeInfo["isFiltered"] = false
		filteredMocks[index].TestModeInfo["SortOrder"] = math.MaxInt64
		h.UpdateConfigMock(filteredMocks[index])
		return true, responseMock, nil
	}

	if index == -1 {
		index = findBinaryMatch(unfilteredMocks, requestBuffers, h)
	}

	if index != -1 {
		responseMock := make([]models.GenericPayload, len(unfilteredMocks[index].Spec.GenericResponses))
		copy(responseMock, unfilteredMocks[index].Spec.GenericResponses)
		return true, responseMock, nil
	}

	return false, nil, nil
}

func findBinaryMatch(tcsMocks []*models.Mock, requestBuffers [][]byte, h *hooks.Hook) int {

	// TODO: need find a proper similarity index to set a benchmark for matching or need to find another way to do approximate matching
	mxSim := 0.5
	mxIdx := -1
	for idx, mock := range tcsMocks {
		if len(mock.Spec.GenericRequests) == len(requestBuffers) {
			for requestIndex, reqBuff := range requestBuffers {

				// bufStr := string(reqBuff)
				// if !IsAsciiPrintable(bufStr) {
				_ = base64.StdEncoding.EncodeToString(reqBuff)
				// }
				encoded, _ := PostgresDecoder(mock.Spec.GenericRequests[requestIndex].Message[0].Data)

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

// checks if s is ascii and printable, aka doesn't include tab, backspace, etc.
func IsAsciiPrintable(s string) bool {
	for _, r := range s {
		if r > unicode.MaxASCII || (!unicode.IsPrint(r) && r != '\r' && r != '\n') {
			return false
		}
	}
	return true
}
