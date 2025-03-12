package idempotencydb

import (
	"fmt"
	"os"
	"strings"

	"go.keploy.io/server/v2/pkg"
	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/pkg/platform/yaml/testdb"
	"go.uber.org/zap"
	yamlLib "gopkg.in/yaml.v3"
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
			logger.Error(msg, zap.Error(err))
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
		} else if key == "header.Date" {
			irrDetectedNoise.NoiseFields[key] = []string{}
		} else {
			if key != "header.Content-Length" {
				logger.Warn("Inconsistancy detected in IRR, you may consider marking them as noise.", zap.String("field", key), zap.String("count", fmt.Sprint(value)))
			}
		}
	}
	return irrDetectedNoise
}

func SaveIRRReport(irrReport *[]IRRTestCase, idemReporFiletPath string, logger *zap.Logger) {
	f, err := os.OpenFile(idemReporFiletPath, os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		logger.Error("IRR: error in opening idempotency report file", zap.Error(err))
		return
	}
	defer f.Close()

	data, err := yamlLib.Marshal(&irrReport)
	if err != nil {
		logger.Error("IRR: error in marshalling the updated test case", zap.Error(err))
		return
	}

	_, err = f.Write(data)
	if err != nil {
		logger.Error("IRR: error writing updated test case", zap.Error(err))
	}
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

//--------------------------------------------------------------------------------------------------------------------------------------

func SaveConfig(irrconfig IRRConfig, configPath string, logger *zap.Logger) {
	// make a one config file for IgnoredFields, SessionTokens and DynamicHeaders
	// where every field has a value, these values can be "headers-values" or "ignored".

	_, err := os.Stat(configPath)
	if os.IsNotExist(err) {
		_, err = os.Create(configPath)
		if err != nil {
			logger.Error("IRR: error in creating irrconfig file", zap.Error(err))
			return
		}
	} else if err != nil {
		logger.Error("IRR: error checking if irrconfig file exists", zap.Error(err))
		return
	}

	f, err := os.OpenFile(configPath, os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		logger.Error("IRR: error in opening irrconfig file", zap.Error(err))
		return
	}
	defer f.Close()

	data, err := yamlLib.Marshal(&irrconfig)
	if err != nil {
		logger.Error("IRR: error in marshalling irrconfig", zap.Error(err))
		return
	}

	_, err = f.Write(data)
	if err != nil {
		logger.Error("IRR: error writing irrconfig", zap.Error(err))
	}
}

func LoadConfig(irrconfig IRRConfig, configPath string, logger *zap.Logger) {
	// load the config file into IgnoredFields, SessionTokens and DynamicHeaders.
}
