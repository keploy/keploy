package idempotencydb

import (
	"fmt"
	"strings"

	"go.keploy.io/server/v2/pkg"
	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/pkg/platform/yaml/testdb"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
)

func CompareResponses(httpResponses []models.HTTPResp, logger *zap.Logger) IRRDetectedNoise {
	irrDetectedNoise := IRRDetectedNoise{
		NoiseFields: map[string][]string{},
	}
	responsesMapList := []map[string][]string{}
	diffMap := map[string]int{}

	for _, resp := range httpResponses {
		m, err := testdb.FlattenHTTPResponse(pkg.ToHTTPHeader(resp.Header), resp.Body)
		if err != nil {
			msg := "error in flattening http response"
			utils.LogError(logger, err, msg)
		}
		responsesMapList = append(responsesMapList, m)
	}

	// fmt.Println("responsesMapList:\n", responsesMapList)
	// n * n * number of fields in responses ---> complexity

	// compare each response with every other response
	for i := 0; i < len(responsesMapList); i++ {
		for j := 0; j < len(responsesMapList); j++ {
			if i == j {
				continue
			}
			for key, value := range responsesMapList[i] {
				// if responsesMapList[j][key] != value {
				if !compareSlices(responsesMapList[j][key], value) {
					diffMap[key]++
				}
			}
		}
	}

	// fmt.Println("diffMap:\n", diffMap)

	n := len(httpResponses)
	for key, value := range diffMap {
		if value == n*(n-1) {
			irrDetectedNoise.NoiseFields[key] = []string{}
			if strings.Contains(key, "body") {
				irrDetectedNoise.NoiseFields["header.Content-Length"] = []string{}
			}
		} else if IsInCommonNoiseFields(key) {
			irrDetectedNoise.NoiseFields[key] = []string{}
			if strings.Contains(key, "body") {
				irrDetectedNoise.NoiseFields["header.Content-Length"] = []string{}
			}
		} else {
			if key != "header.Content-Length" {
				logger.Warn("Inconsistancy detected in IRR, you may consider marking them as noise.", zap.String("field", key), zap.String("count", fmt.Sprint(value)))
			}
		}
	}

	return irrDetectedNoise
}

func compareSlices(slice1, slice2 []string) bool {
	if len(slice1) != len(slice2) {
		return false
	}
	for i := range slice1 {
		if slice1[i] != slice2[i] {
			return false
		}
	}
	return true
}

func IsInCommonNoiseFields(key string) bool {
	for _, field := range CommonNoiseFields {
		if key == field {
			return true
		}
	}
	return false
}
