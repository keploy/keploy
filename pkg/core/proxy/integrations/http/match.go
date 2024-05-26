package http

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strings"

	"github.com/agnivade/levenshtein"
	"go.keploy.io/server/v2/pkg/core/proxy/integrations"
	"go.keploy.io/server/v2/pkg/core/proxy/integrations/util"
	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
)

type req struct {
	method string
	url    *url.URL
	header http.Header
	body   []byte
	raw    []byte
}

func match(ctx context.Context, logger *zap.Logger, input *req, mockDb integrations.MockMemDb) (bool, *models.Mock, error) {
	for {
		if ctx.Err() != nil {
			return false, nil, ctx.Err()
		}

		mocks, err := mockDb.GetUnFilteredMocks()

		if err != nil {
			utils.LogError(logger, err, "failed to get unfilteredMocks mocks")
			return false, nil, errors.New("error while matching the request with the mocks")
		}

		logger.Debug(fmt.Sprintf("Length of unfilteredMocks:%v", len(mocks)))

		var schemaMatched []*models.Mock

		for _, mock := range mocks {
			if ctx.Err() != nil {
				return false, nil, ctx.Err()
			}
			if mock.Kind != models.HTTP {
				continue
			}

			//if the content type is present in http request then we need to check for the same type in the mock
			if input.header.Get("Content-Type") != "" {
				if input.header.Get("Content-Type") != mock.Spec.HTTPReq.Header["Content-Type"] {
					logger.Debug("The content type of mock and request aren't the same")
					continue
				}
			}

			// check the type of the body if content type is not present
			if !matchBodyType(mock.Spec.HTTPReq.Body, input.body) {
				logger.Debug("The body of mock and request aren't of same type")
				continue
			}

			//parse request body url
			parsedURL, err := url.Parse(mock.Spec.HTTPReq.URL)
			if err != nil {
				utils.LogError(logger, err, "failed to parse mock url")
				continue
			}

			//Check if the path matches
			if parsedURL.Path != input.url.Path {
				//If it is not the same, continue
				logger.Debug("The url path of mock and request aren't the same")
				continue
			}

			//Check if the method matches
			if mock.Spec.HTTPReq.Method != models.Method(input.method) {
				//If it is not the same, continue
				logger.Debug("The method of mock and request aren't the same")
				continue
			}

			// Check if the header keys match
			if !mapsHaveSameKeys(mock.Spec.HTTPReq.Header, input.header) {
				// Different headers, so not a match
				logger.Debug("The header keys of mock and request aren't the same")
				continue
			}

			if !mapsHaveSameKeys(mock.Spec.HTTPReq.URLParams, input.url.Query()) {
				// Different query params, so not a match
				logger.Debug("The query params of mock and request aren't the same")
				continue
			}
			schemaMatched = append(schemaMatched, mock)
		}

		if len(schemaMatched) == 0 {
			// basic schema is not matched with any mock hence returning false
			return false, nil, nil
		}

		// do exact body match
		ok, bestMatch := exactBodyMatch(input.body, schemaMatched)
		if ok {
			if !updateMock(ctx, logger, bestMatch, mockDb) {
				continue
			}
			return true, bestMatch, nil
		}

		shortlisted := schemaMatched
		// If the body is JSON we do a schema match. we can add more custom type matching
		if isJSON(input.body) {
			var bodyMatched []*models.Mock

			logger.Debug("Performing schema match for body")
			for _, mock := range schemaMatched {
				if ctx.Err() != nil {
					return false, nil, ctx.Err()
				}

				ok, err := bodyMatch(logger, []byte(mock.Spec.HTTPReq.Body), input.body)
				if err != nil {
					logger.Error("failed to do schema matching on request body", zap.Error(err))
					break
				}

				if ok {
					bodyMatched = append(bodyMatched, mock)
					logger.Debug("found a mock with body schema match")
				}
			}

			if len(bodyMatched) == 0 {
				logger.Debug("couldn't find any mock with body schema match")
				return false, nil, nil
			}

			//if we have only one schema matched mock, we return it
			if len(bodyMatched) == 1 {
				if !updateMock(ctx, logger, bodyMatched[0], mockDb) {
					continue
				}
				return true, bodyMatched[0], nil
			}

			//if more than one schema matched mocks are present, we perform fuzzy match on rest of the mocks
			shortlisted = bodyMatched
		}

		// we should perform fuzzy match if body type is not JSON
		// or if we have more than one json schema matched mocks. (useful in case of async http requests)
		logger.Debug("Performing fuzzy match for req buffer")
		isMatched, bestMatch := fuzzyMatch(shortlisted, input.raw)
		if isMatched {
			if !updateMock(ctx, logger, bestMatch, mockDb) {
				continue
			}
			return true, bestMatch, nil
		}
		return false, nil, nil
	}

}

func exactBodyMatch(body []byte, schemaMatched []*models.Mock) (bool, *models.Mock) {
	for _, mock := range schemaMatched {
		if mock.Spec.HTTPReq.Body == string(body) {
			return true, mock
		}
	}
	return false, nil
}

func bodyMatch(logger *zap.Logger, mockBody, reqBody []byte) (bool, error) {

	var mockData map[string]interface{}
	var reqData map[string]interface{}
	err := json.Unmarshal(mockBody, &mockData)
	if err != nil {
		utils.LogError(logger, err, "failed to unmarshal the mock request body", zap.String("Req", string(mockBody)))
		return false, err
	}
	err = json.Unmarshal(reqBody, &reqData)
	if err != nil {
		utils.LogError(logger, err, "failed to unmarshal the request body", zap.String("Req", string(reqBody)))
		return false, err
	}

	for key := range mockData {
		_, exists := reqData[key]
		if !exists {
			return false, nil
		}
	}
	return true, nil
}

func mapsHaveSameKeys(map1 map[string]string, map2 map[string][]string) bool {
	if len(map1) != len(map2) {
		return false
	}

	for key := range map1 {
		if _, exists := map2[key]; !exists {
			return false
		}
	}

	for key := range map2 {
		if _, exists := map1[key]; !exists {
			return false
		}
	}

	return true
}

func findStringMatch(_ string, mockString []string) int {
	minDist := int(^uint(0) >> 1) // Initialize with max int value
	bestMatch := -1
	for idx, req := range mockString {
		if !util.IsASCII(mockString[idx]) {
			continue
		}

		dist := levenshtein.ComputeDistance(req, mockString[idx])
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
		encoded, _ := decode(mock.Spec.HTTPReq.Body)
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

func encode(buffer []byte) string {
	//Encode the buffer to string
	encoded := string(buffer)
	return encoded
}
func decode(encoded string) ([]byte, error) {
	// decode the string to a buffer.
	data := []byte(encoded)
	return data, nil
}

func fuzzyMatch(tcsMocks []*models.Mock, reqBuff []byte) (bool, *models.Mock) {
	com := encode(reqBuff)
	for _, mock := range tcsMocks {
		encoded, _ := decode(mock.Spec.HTTPReq.Body)
		if string(encoded) == string(reqBuff) || mock.Spec.HTTPReq.Body == com {
			return true, mock
		}
	}
	// convert all the configmocks to string array
	mockString := make([]string, len(tcsMocks))
	for i := 0; i < len(tcsMocks); i++ {
		mockString[i] = tcsMocks[i].Spec.HTTPReq.Body
	}
	// find the closest match
	if util.IsASCII(string(reqBuff)) {
		idx := findStringMatch(string(reqBuff), mockString)
		if idx != -1 {
			return true, tcsMocks[idx]
		}
	}
	idx := findBinaryMatch(tcsMocks, reqBuff)
	if idx != -1 {
		return true, tcsMocks[idx]
	}
	return false, &models.Mock{}
}

func matchBodyType(mockBody string, reqBody []byte) bool {
	if mockBody == "" && string(reqBody) == "" {
		return true
	}

	mockBodyType := guessContentType([]byte(mockBody))
	reqBodyType := guessContentType(reqBody)

	return mockBodyType == reqBodyType
}

type ContentType string

// Constants for different content types.
const (
	Unknown   ContentType = "Unknown"
	JSON      ContentType = "JSON"
	XML       ContentType = "XML"
	CSV       ContentType = "CSV"
	HTML      ContentType = "HTML"
	TextPlain ContentType = "TextPlain"
)

// guessContentType attempts to determine the content type of the provided byte slice.
func guessContentType(data []byte) ContentType {
	// Use net/http library's DetectContentType for basic MIME type detection
	mimeType := http.DetectContentType(data)

	// Additional checks to further specify the content type
	switch {
	case isJSON(data):
		return JSON
	case isXML(data):
		return XML
	case strings.Contains(mimeType, "text/html"):
		return HTML
	case strings.Contains(mimeType, "text/plain"):
		if isCSV(data) {
			return CSV
		}
		return TextPlain
	}

	return Unknown
}

// isXML tries to unmarshal data into a generic XML struct to check if it's valid XML
func isXML(data []byte) bool {
	var xm xml.Name
	return xml.Unmarshal(data, &xm) == nil
}

// isCSV checks if data can be parsed as CSV by looking for common characteristics
func isCSV(data []byte) bool {
	// Very simple CSV check: look for commas in the first line
	content := string(data)
	if lines := strings.Split(content, "\n"); len(lines) > 0 {
		return strings.Contains(lines[0], ",")
	}
	return false
}

// updateMock processes the matched mock based on its filtered status.
func updateMock(_ context.Context, logger *zap.Logger, matchedMock *models.Mock, mockDb integrations.MockMemDb) bool {
	if matchedMock.TestModeInfo.IsFiltered {
		originalMatchedMock := *matchedMock
		matchedMock.TestModeInfo.IsFiltered = false
		matchedMock.TestModeInfo.SortOrder = math.MaxInt
		//UpdateUnFilteredMock also marks the mock as used
		updated := mockDb.UpdateUnFilteredMock(&originalMatchedMock, matchedMock)
		return updated
	}

	// we don't update the mock if the IsFiltered is false
	err := mockDb.FlagMockAsUsed(matchedMock)
	if err != nil {
		logger.Error("failed to flag mock as used", zap.Error(err))
	}

	return true
}
