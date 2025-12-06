package http

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/agnivade/levenshtein"
	"go.keploy.io/server/v3/pkg"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/util"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

type req struct {
	method string
	url    *url.URL
	header http.Header
	body   []byte
	raw    []byte
}

func (h *HTTP) match(ctx context.Context, input *req, mockDb integrations.MockMemDb, headerNoise map[string][]string) (bool, *models.Mock, error) {
	for {
		if ctx.Err() != nil {
			return false, nil, ctx.Err()
		}

		// Fetch and filter HTTP mocks
		mocks, err := mockDb.GetUnFilteredMocks()
		if err != nil {
			utils.LogError(h.Logger, err, "failed to get unfilteredMocks mocks")
			return false, nil, errors.New("error while matching the request with the mocks")
		}
		unfilteredMocks := FilterHTTPMocks(mocks)

		// Log all mock names in a single line for better readability
		mockNames := make([]string, len(unfilteredMocks))
		for i, mock := range unfilteredMocks {
			mockNames[i] = mock.Name
		}
		h.Logger.Debug("mocks under consideration for match function", zap.Strings("mock names", mockNames))

		h.Logger.Debug(fmt.Sprintf("Length of unfilteredMocks:%v", len(unfilteredMocks)))

		// Matching process
		schemaMatched, err := h.SchemaMatch(ctx, input, unfilteredMocks, headerNoise)
		if err != nil {
			return false, nil, err
		}

		if len(schemaMatched) == 0 {
			return false, nil, nil
		}

		// Exact body match
		ok, bestMatch := h.ExactBodyMatch(input.body, schemaMatched)
		if ok {
			h.Logger.Debug("exact body match found", zap.String("mock name", bestMatch.Name))
			if !h.updateMock(ctx, bestMatch, mockDb) {
				continue
			}
			return true, bestMatch, nil
		}

		shortListed := schemaMatched
		// Schema match for JSON bodies
		if pkg.IsJSON(input.body) {
			bodyMatched, err := h.PerformBodyMatch(ctx, schemaMatched, input.body)
			if err != nil {
				return false, nil, err
			}

			if len(bodyMatched) == 0 {
				h.Logger.Debug("No mock found with body schema match")
				return false, nil, nil
			}

			if len(bodyMatched) == 1 {
				h.Logger.Debug("body match found", zap.String("mock name", bodyMatched[0].Name))
				if !h.updateMock(ctx, bodyMatched[0], mockDb) {
					continue
				}
				return true, bodyMatched[0], nil
			}

			// More than one match, perform fuzzy match
			shortListed = bodyMatched
		}

		h.Logger.Debug("Performing fuzzy match for req buffer")
		// Perform fuzzy match on the request
		isMatched, bestMatch := h.PerformFuzzyMatch(shortListed, input.raw)
		if isMatched {
			h.Logger.Debug("fuzzy match found a matching mock", zap.String("mock name", bestMatch.Name))
			if !h.updateMock(ctx, bestMatch, mockDb) {
				continue
			}
			return true, bestMatch, nil
		}
		return false, nil, nil
	}
}

// FilterHTTPMocks Filter mocks to only HTTP mocks
func FilterHTTPMocks(mocks []*models.Mock) []*models.Mock {
	var httpMocks []*models.Mock
	for _, mock := range mocks {
		if mock.Kind != models.Kind(models.HTTP) {
			continue
		}
		httpMocks = append(httpMocks, mock)
	}
	return httpMocks
}

// MatchBodyType Body type match check (content type matching)
func (h *HTTP) MatchBodyType(mockBody string, reqBody []byte) bool {
	if mockBody == "" && string(reqBody) == "" {
		return true
	}
	mockBodyType := pkg.GuessContentType([]byte(mockBody))
	reqBodyType := pkg.GuessContentType(reqBody)
	h.Logger.Debug("mock body type", zap.Any("mock body type", mockBodyType), zap.Any("req body type", reqBodyType))
	return mockBodyType == reqBodyType
}

func (h *HTTP) MatchURLPath(mockURL, reqPath string) bool {
	parsedURL, err := url.Parse(mockURL)
	if err != nil {
		return false
	}
	h.Logger.Debug("parsed URL", zap.Any("parsed URL", parsedURL.Path), zap.Any("req path", reqPath))
	return parsedURL.Path == reqPath
}

// relaxed header key matcher (presence-only)
func (h *HTTP) HeadersContainKeys(expected map[string]string, actual http.Header, headerNoise map[string][]string) bool {
	shouldIgnore := func(k string) bool {
		lk := strings.ToLower(k)
		// Ignore keploy headers
		if strings.HasPrefix(lk, "keploy") {
			return true
		}
		// Ignore headers that are in noise configuration
		if headerNoise != nil {
			if _, exists := headerNoise[lk]; exists {
				return true
			}
		}
		return false
	}

	// Build a case-insensitive set of actual header keys
	actualKeys := make(map[string]struct{}, len(actual))
	for k := range actual {
		actualKeys[strings.ToLower(k)] = struct{}{}
	}

	// Ensure every non-ignored expected key exists in the request
	for k := range expected {
		if shouldIgnore(k) {
			h.Logger.Debug("header key is ignored", zap.String("header key", k))
			continue
		}
		if _, ok := actualKeys[strings.ToLower(k)]; !ok {
			return false
		}
	}
	return true
}

func (h *HTTP) MapsHaveSameKeys(map1 map[string]string, map2 map[string][]string) bool {
	// Helper function to check if a header should be ignored
	shouldIgnoreHeader := func(key string) bool {
		lkey := strings.ToLower(key)
		return strings.HasPrefix(lkey, "keploy")
	}

	// Count non-ignored keys in map1
	map1Count := 0
	for key := range map1 {
		if !shouldIgnoreHeader(key) {
			map1Count++
		}
	}

	// Count non-ignored keys in map2
	map2Count := 0
	for key := range map2 {
		if !shouldIgnoreHeader(key) {
			map2Count++
		}
	}

	// Check if counts match
	if map1Count != map2Count {
		return false
	}

	// Check if all non-ignored keys in map1 exist in map2
	for key := range map1 {
		if shouldIgnoreHeader(key) {
			continue
		}
		if _, exists := map2[key]; !exists {
			return false
		}
	}

	// Check if all non-ignored keys in map2 exist in map1
	for key := range map2 {
		if shouldIgnoreHeader(key) {
			continue
		}
		if _, exists := map1[key]; !exists {
			return false
		}
	}

	return true
}

// SchemaMatch match the schema of the request with the mocks
func (h *HTTP) SchemaMatch(ctx context.Context, input *req, unfilteredMocks []*models.Mock, headerNoise map[string][]string) ([]*models.Mock, error) {
	var schemaMatched []*models.Mock

	for _, mock := range unfilteredMocks {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}

		// Content type check
		if input.header.Get("Content-Type") != "" {
			if !pkg.CompareMultiValueHeaders(mock.Spec.HTTPReq.Header["Content-Type"], input.header.Values("Content-Type")) {
				h.Logger.Debug("The content type of mock and request aren't the same", zap.String("mock name", mock.Name), zap.Any("input header", input.header.Values("Content-Type")), zap.Any("mock header content-type", mock.Spec.HTTPReq.Header["Content-Type"]))
				continue
			}
		}
		// Body type check
		if !h.MatchBodyType(mock.Spec.HTTPReq.Body, input.body) {
			h.Logger.Debug("The body of mock and request aren't of same type", zap.String("mock name", mock.Name))
			continue
		}

		// URL path match
		if !h.MatchURLPath(mock.Spec.HTTPReq.URL, input.url.Path) {
			h.Logger.Debug("The url path of mock and request aren't the same", zap.String("mock name", mock.Name), zap.Any("input url", input.url.Path), zap.Any("mock url", mock.Spec.HTTPReq.URL))
			continue
		}

		// HTTP method match
		if mock.Spec.HTTPReq.Method != models.Method(input.method) {
			h.Logger.Debug("The method of mock and request aren't the same", zap.String("mock name", mock.Name))
			continue
		}

		// Header key match (presence-only; extra request headers allowed)
		if !h.HeadersContainKeys(mock.Spec.HTTPReq.Header, input.header, headerNoise) {
			h.Logger.Debug("headers missing required keys for mock name",
				zap.String("mock name", mock.Name),
				zap.Any("expected header keys", mock.Spec.HTTPReq.Header),
				zap.Any("input header", input.header))
			continue
		}

		// Query parameter match
		if !h.MapsHaveSameKeys(mock.Spec.HTTPReq.URLParams, input.url.Query()) {
			h.Logger.Debug("The query params of mock and request aren't the same", zap.String("mock name", mock.Name))
			continue
		}

		schemaMatched = append(schemaMatched, mock)
	}

	return schemaMatched, nil
}

// ExactBodyMatch Exact body match
func (h *HTTP) ExactBodyMatch(body []byte, schemaMatched []*models.Mock) (bool, *models.Mock) {
	// Log all mock names in a single line for better readability
	mockNames := make([]string, len(schemaMatched))
	for i, mock := range schemaMatched {
		mockNames[i] = mock.Name
	}
	h.Logger.Debug("mocks under consideration for exact body match", zap.Strings("mock names", mockNames), zap.String("req body", string(body)))

	for _, mock := range schemaMatched {
		if mock.Spec.HTTPReq.Body == string(body) {
			return true, mock
		}
	}
	return false, nil
}

func (h *HTTP) bodyMatch(mockBody, reqBody []byte) (bool, error) {

	var mockData map[string]any
	var reqData map[string]any
	err := json.Unmarshal(mockBody, &mockData)
	if err != nil {
		utils.LogError(h.Logger, err, "failed to unmarshal the mock request body", zap.String("Req", string(mockBody)))
		return false, err
	}
	err = json.Unmarshal(reqBody, &reqData)
	if err != nil {
		utils.LogError(h.Logger, err, "failed to unmarshal the request body", zap.String("Req", string(reqBody)))
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

// PerformBodyMatch Perform body match for JSON data
func (h *HTTP) PerformBodyMatch(ctx context.Context, schemaMatched []*models.Mock, reqBody []byte) ([]*models.Mock, error) {
	h.Logger.Debug("Performing schema match for body")

	// Log all mock names in a single line for better readability
	mockNames := make([]string, len(schemaMatched))
	for i, mock := range schemaMatched {
		mockNames[i] = mock.Name
	}
	h.Logger.Debug("mocks under consideration for PerformBodyMatch function", zap.Strings("mock names", mockNames))

	var bodyMatched []*models.Mock
	for _, mock := range schemaMatched {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}

		ok, err := h.bodyMatch([]byte(mock.Spec.HTTPReq.Body), reqBody)
		if err != nil {
			h.Logger.Error("failed to do schema matching on request body", zap.Error(err))
			break
		}

		if ok {
			bodyMatched = append(bodyMatched, mock)
			h.Logger.Debug("found a mock with body schema match", zap.String("mock name", mock.Name))
		}
	}
	return bodyMatched, nil
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

// TODO: generalize the function to work with any type of integration
func (h *HTTP) findBinaryMatch(mocks []*models.Mock, reqBuff []byte) int {

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

// PerformFuzzyMatch Perform fuzzy match on the request
func (h *HTTP) PerformFuzzyMatch(tcsMocks []*models.Mock, reqBuff []byte) (bool, *models.Mock) {
	// Log all mock names in a single line for better readability
	mockNames := make([]string, len(tcsMocks))
	for i, mock := range tcsMocks {
		mockNames[i] = mock.Name
	}
	h.Logger.Debug("mocks under consideration for performfuzzyMatch function", zap.Strings("mock names", mockNames))

	encodedReq := encode(reqBuff)
	for _, mock := range tcsMocks {
		encodedMock, _ := decode(mock.Spec.HTTPReq.Body)
		if string(encodedMock) == string(reqBuff) || mock.Spec.HTTPReq.Body == encodedReq {
			h.Logger.Debug("exact match found", zap.String("mock name", mock.Name))
			return true, mock
		}
	}
	// String-based fuzzy matching
	mockStrings := make([]string, len(tcsMocks))
	for i := range tcsMocks {
		mockStrings[i] = tcsMocks[i].Spec.HTTPReq.Body
	}
	if util.IsASCII(string(reqBuff)) {
		idx := h.findStringMatch(string(reqBuff), mockStrings)
		if idx != -1 {
			h.Logger.Debug("string match found", zap.String("mock name", tcsMocks[idx].Name))
			return true, tcsMocks[idx]
		}
	}
	idx := h.findBinaryMatch(tcsMocks, reqBuff)
	if idx != -1 {
		h.Logger.Debug("binary match found", zap.String("mock name", tcsMocks[idx].Name))
		return true, tcsMocks[idx]
	}
	return false, nil
}

// Update the matched mock (delete or update)
func (h *HTTP) updateMock(_ context.Context, matchedMock *models.Mock, mockDb integrations.MockMemDb) bool {
	originalMatchedMock := *matchedMock
	matchedMock.TestModeInfo.IsFiltered = false
	matchedMock.TestModeInfo.SortOrder = pkg.GetNextSortNum()
	updated := mockDb.UpdateUnFilteredMock(&originalMatchedMock, matchedMock)
	return updated
}
