//go:build linux

package http

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
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

type req struct {
	method string
	url    *url.URL
	header http.Header
	body   []byte
	raw    []byte
}

func (h *HTTP) match(ctx context.Context, logger *zap.Logger, input *req, mockDb integrations.MockMemDb) (bool, *models.Mock, error) {
	for {
		if ctx.Err() != nil {
			return false, nil, ctx.Err()
		}

		// Fetch and filter HTTP mocks
		mocks, err := mockDb.GetUnFilteredMocks()

		if err != nil {
			utils.LogError(logger, err, "failed to get unfilteredMocks mocks")
			return false, nil, errors.New("error while matching the request with the mocks")
		}
		unfilteredMocks := FilterHTTPMocks(mocks)

		// Matching process
		schemaMatched, err := h.filterAndMatchMocks(ctx, logger, input, unfilteredMocks)
		if err != nil {
			return false, nil, err
		}

		if len(schemaMatched) == 0 {
			return false, nil, nil
		}

		// Exact body match
		ok, bestMatch := h.ExactBodyMatch(input.body, schemaMatched)
		if ok {
			return h.handleMatchedMock(ctx, logger, bestMatch, mockDb)
		}

		// Schema match for JSON bodies
		if h.IsJSON(input.body) {
			bodyMatched, err := h.PerformBodyMatch(ctx, logger, schemaMatched, input.body)
			if err != nil {
				return false, nil, err
			}

			if len(bodyMatched) == 0 {
				return false, nil, nil
			}

			if len(bodyMatched) == 1 {
				return h.handleMatchedMock(ctx, logger, bodyMatched[0], mockDb)
			}

			// More than one match, perform fuzzy match
			schemaMatched = bodyMatched
		}

		// Perform fuzzy match on the request
		isMatched, bestMatch, err := h.PerformFuzzyMatch(ctx, logger, schemaMatched, input.raw, mockDb)
		if err != nil {
			logger.Error("failed to perform fuzzy match", zap.Error(err))
		}
		if isMatched {
			return h.handleMatchedMock(ctx, logger, bestMatch, mockDb)
		}
	}
}

// FilterHTTPMocks Filter mocks to only HTTP mocks
func FilterHTTPMocks(mocks []*models.Mock) []*models.Mock {
	var unfilteredMocks []*models.Mock
	for _, mock := range mocks {
		if mock.Kind == "Http" {
			unfilteredMocks = append(unfilteredMocks, mock)
		}
	}
	return unfilteredMocks
}

func (h *HTTP) MatchURLPath(mockURL, reqPath string) bool {
	parsedURL, err := url.Parse(mockURL)
	if err != nil {
		return false
	}
	return parsedURL.Path == reqPath
}

// Filter and match mocks
func (h *HTTP) filterAndMatchMocks(ctx context.Context, logger *zap.Logger, input *req, unfilteredMocks []*models.Mock) ([]*models.Mock, error) {
	var schemaMatched []*models.Mock

	for _, mock := range unfilteredMocks {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}

		if mock.Kind != models.HTTP {
			continue
		}

		// Content type check
		if input.header.Get("Content-Type") != "" {
			if input.header.Get("Content-Type") != mock.Spec.HTTPReq.Header["Content-Type"] {
				logger.Debug("The content type of mock and request aren't the same")
				continue
			}
		}
		// Body type check
		if !h.MatchBodyType(mock.Spec.HTTPReq.Body, input.body) {
			logger.Debug("The body of mock and request aren't of same type")
			continue
		}

		// URL path match
		if !h.MatchURLPath(mock.Spec.HTTPReq.URL, input.url.Path) {
			logger.Debug("The url path of mock and request aren't the same")
			continue
		}

		// HTTP method match
		if mock.Spec.HTTPReq.Method != models.Method(input.method) {
			logger.Debug("The method of mock and request aren't the same")
			continue
		}

		// Header key match
		if !h.MapsHaveSameKeys(mock.Spec.HTTPReq.Header, input.header) {
			logger.Debug("The header keys of mock and request aren't the same")
			continue
		}

		// Query parameter match
		if !h.MapsHaveSameKeys(mock.Spec.HTTPReq.URLParams, input.url.Query()) {
			logger.Debug("The query params of mock and request aren't the same")
			continue
		}

		schemaMatched = append(schemaMatched, mock)
	}

	return schemaMatched, nil
}
func (h *HTTP) bodyMatch(logger *zap.Logger, mockBody, reqBody []byte) (bool, error) {

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

func (h *HTTP) MapsHaveSameKeys(map1 map[string]string, map2 map[string][]string) bool {
	if len(map1) != len(map2) {
		return false
	}

	for key := range map1 {
		lkey := strings.ToLower(key)
		if lkey == "keploy-test-id" || lkey == "keploy-test-set-id" {
			continue
		}
		if _, exists := map2[key]; !exists {
			return false
		}
	}

	for key := range map2 {
		lkey := strings.ToLower(key)
		if lkey == "keploy-test-id" || lkey == "keploy-test-set-id" {
			continue
		}
		if _, exists := map1[key]; !exists {
			return false
		}
	}

	return true
}

// TODO: generalize the function to work with any type of integration
func (h *HTTP) findBinaryMatch(mocks []*models.Mock, reqBuff []byte) int {

	mxSim := -1.0
	mxIdx := -1
	// find the fuzzy hash of the mocks
	for idx, mock := range mocks {
		encoded, _ := h.decode(mock.Spec.HTTPReq.Body)
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

func (h *HTTP) encode(buffer []byte) string {
	//Encode the buffer to string
	encoded := string(buffer)
	return encoded
}
func (h *HTTP) decode(encoded string) ([]byte, error) {
	// decode the string to a buffer.
	data := []byte(encoded)
	return data, nil
}

// ExactBodyMatch Exact body match
func (h *HTTP) ExactBodyMatch(body []byte, schemaMatched []*models.Mock) (bool, *models.Mock) {
	for _, mock := range schemaMatched {
		if mock.Spec.HTTPReq.Body == string(body) {
			return true, mock
		}
	}
	return false, nil
}

// Handle matched mock: update mock or delete based on SQS request type
func (h *HTTP) handleMatchedMock(ctx context.Context, logger *zap.Logger, bestMatch *models.Mock, mockDb integrations.MockMemDb) (bool, *models.Mock, error) {
	if !h.updateMock(ctx, logger, bestMatch, mockDb) {
		return false, nil, nil
	}
	return true, bestMatch, nil
}

// PerformBodyMatch Perform body match for JSON data
func (h *HTTP) PerformBodyMatch(ctx context.Context, logger *zap.Logger, schemaMatched []*models.Mock, reqBody []byte) ([]*models.Mock, error) {
	var bodyMatched []*models.Mock
	for _, mock := range schemaMatched {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}

		ok, err := h.bodyMatch(logger, []byte(mock.Spec.HTTPReq.Body), reqBody)
		if err != nil {
			return nil, err
		}

		if ok {
			bodyMatched = append(bodyMatched, mock)
			logger.Debug("found a mock with body schema match")
		}
	}
	return bodyMatched, nil
}

// PerformFuzzyMatch Perform fuzzy match on the request buffer
func (h *HTTP) PerformFuzzyMatch(ctx context.Context, logger *zap.Logger, schemaMatched []*models.Mock, reqBuff []byte, mockDb integrations.MockMemDb) (bool, *models.Mock, error) {
	isMatched, bestMatch := h.fuzzyMatch(schemaMatched, reqBuff)
	if isMatched {
		return h.handleMatchedMock(ctx, logger, bestMatch, mockDb)
	}
	return false, nil, nil
}

// MatchBodyType Body type match check (content type matching)
func (h *HTTP) MatchBodyType(mockBody string, reqBody []byte) bool {
	if mockBody == "" && string(reqBody) == "" {
		return true
	}
	mockBodyType := h.guessContentType([]byte(mockBody))
	reqBodyType := h.guessContentType(reqBody)
	return mockBodyType == reqBodyType
}

// Fuzzy match helper for string matching
func (h *HTTP) findStringMatch(req string, mockStrings []string) int {
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

// Fuzzy matching function
func (h *HTTP) fuzzyMatch(tcsMocks []*models.Mock, reqBuff []byte) (bool, *models.Mock) {
	encodedReq := h.encode(reqBuff)
	for _, mock := range tcsMocks {
		encodedMock, _ := h.decode(mock.Spec.HTTPReq.Body)
		if string(encodedMock) == string(reqBuff) || mock.Spec.HTTPReq.Body == encodedReq {
			return true, mock
		}
	}
	// String-based fuzzy matching
	mockStrings := make([]string, len(tcsMocks))
	for i := 0; i < len(tcsMocks); i++ {
		mockStrings[i] = tcsMocks[i].Spec.HTTPReq.Body
	}
	if util.IsASCII(string(reqBuff)) {
		idx := h.findStringMatch(string(reqBuff), mockStrings)
		if idx != -1 {
			return true, tcsMocks[idx]
		}
	}
	idx := h.findBinaryMatch(tcsMocks, reqBuff)
	if idx != -1 {
		return true, tcsMocks[idx]
	}
	return false, nil
}

func (h *HTTP) isXML(data []byte) bool {
	var xm xml.Name
	return xml.Unmarshal(data, &xm) == nil
}

// isCSV checks if data can be parsed as CSV by looking for common characteristics
func (h *HTTP) isCSV(data []byte) bool {
	// Very simple CSV check: look for commas in the first line
	content := string(data)
	if lines := strings.Split(content, "\n"); len(lines) > 0 {
		return strings.Contains(lines[0], ",")
	}
	return false
}

func (h *HTTP) guessContentType(data []byte) ContentType {
	// Use net/http library's DetectContentType for basic MIME type detection
	mimeType := http.DetectContentType(data)

	// Additional checks to further specify the content type
	switch {
	case h.IsJSON(data):
		return JSON
	case h.isXML(data):
		return XML
	case strings.Contains(mimeType, "text/html"):
		return HTML
	case strings.Contains(mimeType, "text/plain"):
		if h.isCSV(data) {
			return CSV
		}
		return TextPlain
	}

	return Unknown
}

// Update the matched mock (delete or update)
func (h *HTTP) updateMock(_ context.Context, logger *zap.Logger, matchedMock *models.Mock, mockDb integrations.MockMemDb) bool {
	if matchedMock.TestModeInfo.IsFiltered {
		originalMatchedMock := *matchedMock
		matchedMock.TestModeInfo.IsFiltered = false
		matchedMock.TestModeInfo.SortOrder = math.MaxInt
		updated := mockDb.UpdateUnFilteredMock(&originalMatchedMock, matchedMock)
		return updated
	}
	err := mockDb.FlagMockAsUsed(*matchedMock)
	if err != nil {
		logger.Error("failed to flag mock as used", zap.Error(err))
	}
	return true
}
func (h *HTTP) IsJSON(body []byte) bool {
	var js interface{}
	return json.Unmarshal(body, &js) == nil
}
