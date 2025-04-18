//go:build linux

package redisv2

import (
	"context"
	"fmt"
	"math"
	"reflect"

	"go.keploy.io/server/v2/pkg/core/proxy/integrations"
	"go.keploy.io/server/v2/pkg/core/proxy/integrations/util"
	"go.keploy.io/server/v2/pkg/models"
)

func fuzzyMatch(ctx context.Context, reqBuff [][]byte, mockDb integrations.MockMemDb) (bool, []models.RedisResponses, error) {
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
				if mock.Kind != "Redis" {
					continue
				}
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
				responseMock := make([]models.RedisResponses, len(filteredMocks[index].Spec.RedisResponses))
				copy(responseMock, filteredMocks[index].Spec.RedisResponses)
				originalFilteredMock := *filteredMocks[index]
				filteredMocks[index].TestModeInfo.IsFiltered = false
				filteredMocks[index].TestModeInfo.SortOrder = math.MaxInt64
				isUpdated := mockDb.UpdateUnFilteredMock(&originalFilteredMock, filteredMocks[index])
				if !isUpdated {
					continue
				}
				return true, responseMock, nil
			}

			index = findExactMatch(unfilteredMocks, reqBuff)

			if index != -1 {
				responseMock := make([]models.RedisResponses, len(unfilteredMocks[index].Spec.RedisResponses))
				copy(responseMock, unfilteredMocks[index].Spec.RedisResponses)
				return true, responseMock, nil
			}

			totalMocks := append(filteredMocks, unfilteredMocks...)
			index = findBinaryMatch(totalMocks, reqBuff, 0.4)

			if index != -1 {
				responseMock := make([]models.RedisResponses, len(totalMocks[index].Spec.RedisResponses))
				copy(responseMock, totalMocks[index].Spec.RedisResponses)
				originalFilteredMock := *totalMocks[index]
				if totalMocks[index].TestModeInfo.IsFiltered {
					totalMocks[index].TestModeInfo.IsFiltered = false
					totalMocks[index].TestModeInfo.SortOrder = math.MaxInt64
					isUpdated := mockDb.UpdateUnFilteredMock(&originalFilteredMock, totalMocks[index])
					if !isUpdated {
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
	mxIdx := -1
	for idx, mock := range tcsMocks {
		if len(mock.Spec.RedisRequests) == len(reqBuffs) {
			for requestIndex, reqBuff := range reqBuffs {
				data, ok := mock.Spec.RedisRequests[requestIndex].Message[0].Data.(string)
				if !ok {
					continue // or handle gracefully if needed
				}

				mockReq, err := util.DecodeBase64(data)
				if err != nil {
					mockReq = []byte(data)
				}

				similarity := fuzzyCheck(mockReq, reqBuff)
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
		if len(mock.Spec.RedisRequests) != len(reqBuffs) {
			continue
		}
		allMatch := true
		for i, reqBuf := range reqBuffs {
			actual, err := parseRedis(reqBuf) // []models.RedisBodyType
			if err != nil {
				allMatch = false
				break
			}
			expectedRaw := mock.Spec.RedisRequests[i].Message
			expected, err := normalizeBodies(expectedRaw)
			if err != nil {
				fmt.Printf("normalize error: %v\n", err)
				allMatch = false
				break
			}
			if !reflect.DeepEqual(actual, expected) {
				allMatch = false
				break
			}
		}
		if allMatch {
			return idx
		}
	}
	return -1
}

func normalizeBodies(raw []models.RedisBodyType) ([]models.RedisBodyType, error) {
	out := make([]models.RedisBodyType, len(raw))

	for i, b := range raw {
		switch b.Type {

		case "array":
			// Data must be []interface{} where each element is a map[string]interface{}
			arrIface, ok := b.Data.([]interface{})
			if !ok {
				return nil, fmt.Errorf("array.Data is %T, want []interface{}", b.Data)
			}
			nested := make([]models.RedisBodyType, len(arrIface))
			for j, elem := range arrIface {
				m, ok := elem.(map[string]interface{})
				if !ok {
					return nil, fmt.Errorf("array[%d] element is %T, want map[string]interface{}", j, elem)
				}
				// extract fields
				t, _ := m["type"].(string)
				sizeF, _ := m["size"].(int)
				rawData := m["data"]

				// for nested arrays or maps you could recurse here too,
				// but for simplicity we only handle simple types in arrays of arrays
				nested[j] = models.RedisBodyType{
					Type: t,
					Size: sizeF,
					Data: rawData,
				}
			}
			out[i] = models.RedisBodyType{Type: "array", Size: len(nested), Data: nested}

		case "map":
			// Data must be []interface{} of map entries with "Key" and "Value"
			mapIface, ok := b.Data.([]interface{})
			if !ok {
				return nil, fmt.Errorf("map.Data is %T, want []interface{}", b.Data)
			}
			entries := make([]models.RedisMapBody, len(mapIface))
			for j, elem := range mapIface {
				m, ok := elem.(map[string]interface{})
				if !ok {
					return nil, fmt.Errorf("map[%d] element is %T, want map[string]interface{}", j, elem)
				}
				// Extract Key
				keyRaw, ok := m["Key"].(map[string]interface{})
				if !ok {
					return nil, fmt.Errorf("map[%d].Key is %T, want map[string]interface{}", j, m["Key"])
				}
				keyLenF, _ := keyRaw["Length"].(int)
				keyVal := keyRaw["Value"]

				// Extract Value
				valRaw, ok := m["Value"].(map[string]interface{})
				if !ok {
					return nil, fmt.Errorf("map[%d].Value is %T, want map[string]interface{}", j, m["Value"])
				}
				valLenF, _ := valRaw["Length"].(int)
				valVal := valRaw["Value"]

				entries[j] = models.RedisMapBody{
					Key:   models.RedisElement{Length: int(keyLenF), Value: keyVal},
					Value: models.RedisElement{Length: int(valLenF), Value: valVal},
				}
			}
			out[i] = models.RedisBodyType{Type: "map", Size: len(entries), Data: entries}

		default:
			// simple types: just pass through
			out[i] = b
		}
	}

	return out, nil
}
