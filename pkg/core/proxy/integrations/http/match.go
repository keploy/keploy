//go:build linux

package http

import (
	"context"
	"encoding/json"
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

func (h *HTTP) match(ctx context.Context, input *req, mockDb integrations.MockMemDb) (bool, *models.Mock, error) {
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

		h.Logger.Debug(fmt.Sprintf("Length of unfilteredMocks:%v", len(unfilteredMocks)))

		// Matching process
		schemaMatched, err := h.SchemaMatch(ctx, input, unfilteredMocks)
		if err != nil {
			return false, nil, err
		}

		if len(schemaMatched) == 0 {
			return false, nil, nil
		}

		// Exact body match
		ok, bestMatch := h.ExactBodyMatch(input.body, schemaMatched)
		if ok {
			if !h.updateMock(ctx, bestMatch, mockDb) {
				continue
			}
			return true, bestMatch, nil
		}

		shortListed := schemaMatched
		// Schema match for JSON bodies
		if IsJSON(input.body) {
			bodyMatched, err := h.PerformBodyMatch(ctx, schemaMatched, input.body)
			if err != nil {
				return false, nil, err
			}

			if len(bodyMatched) == 0 {
				h.Logger.Debug("No mock found with body schema match")
				return false, nil, nil
			}

			if len(bodyMatched) == 1 {
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
		if mock.Kind != models.Kind(integrations.HTTP) {
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
	mockBodyType := guessContentType([]byte(mockBody))
	reqBodyType := guessContentType(reqBody)
	return mockBodyType == reqBodyType
}

func (h *HTTP) MatchURLPath(mockURL, reqPath string) bool {
	parsedURL, err := url.Parse(mockURL)
	if err != nil {
		return false
	}
	return parsedURL.Path == reqPath
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

// match mocks
func (h *HTTP) SchemaMatch(ctx context.Context, input *req, unfilteredMocks []*models.Mock) ([]*models.Mock, error) {
	var schemaMatched []*models.Mock

	for _, mock := range unfilteredMocks {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}

		// Content type check
		if input.header.Get("Content-Type") != "" {
			if input.header.Get("Content-Type") != mock.Spec.HTTPReq.Header["Content-Type"] {
				h.Logger.Debug("The content type of mock and request aren't the same")
				continue
			}
		}
		// Body type check
		if !h.MatchBodyType(mock.Spec.HTTPReq.Body, input.body) {
			h.Logger.Debug("The body of mock and request aren't of same type")
			continue
		}

		// URL path match
		if !h.MatchURLPath(mock.Spec.HTTPReq.URL, input.url.Path) {
			h.Logger.Debug("The url path of mock and request aren't the same")
			continue
		}

		// HTTP method match
		if mock.Spec.HTTPReq.Method != models.Method(input.method) {
			h.Logger.Debug("The method of mock and request aren't the same")
			continue
		}

		// Header key match
		if !h.MapsHaveSameKeys(mock.Spec.HTTPReq.Header, input.header) {
			h.Logger.Debug("The header keys of mock and request aren't the same")
			continue
		}

		// Query parameter match
		if !h.MapsHaveSameKeys(mock.Spec.HTTPReq.URLParams, input.url.Query()) {
			h.Logger.Debug("The query params of mock and request aren't the same")
			continue
		}

		schemaMatched = append(schemaMatched, mock)
	}

	return schemaMatched, nil
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
			h.Logger.Debug("found a mock with body schema match")
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

// Fuzzy matching function
func (h *HTTP) PerformFuzzyMatch(tcsMocks []*models.Mock, reqBuff []byte) (bool, *models.Mock) {
	encodedReq := encode(reqBuff)
	for _, mock := range tcsMocks {
		encodedMock, _ := decode(mock.Spec.HTTPReq.Body)
		if string(encodedMock) == string(reqBuff) || mock.Spec.HTTPReq.Body == encodedReq {
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
			return true, tcsMocks[idx]
		}
	}
	idx := h.findBinaryMatch(tcsMocks, reqBuff)
	if idx != -1 {
		return true, tcsMocks[idx]
	}
	return false, nil
}

// Update the matched mock (delete or update)
func (h *HTTP) updateMock(_ context.Context, matchedMock *models.Mock, mockDb integrations.MockMemDb) bool {
	if matchedMock.TestModeInfo.IsFiltered {
		originalMatchedMock := *matchedMock
		matchedMock.TestModeInfo.IsFiltered = false
		matchedMock.TestModeInfo.SortOrder = math.MaxInt
		updated := mockDb.UpdateUnFilteredMock(&originalMatchedMock, matchedMock)
		return updated
	}
	err := mockDb.FlagMockAsUsed(*matchedMock)
	if err != nil {
		h.Logger.Error("failed to flag mock as used", zap.Error(err))
	}
	return true
}
