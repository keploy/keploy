package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"text/template"
	"time"

	"github.com/7sDream/geko"
	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
)

func (t *Tools) Templatize(ctx context.Context) error {

	testSets := t.config.Templatize.TestSets
	if len(testSets) == 0 {
		all, err := t.testDB.GetAllTestSetIDs(ctx)
		if err != nil {
			utils.LogError(t.logger, err, "failed to get all test sets")
			return err
		}
		testSets = all
	}

	if len(testSets) == 0 {
		t.logger.Warn("No test sets found to templatize")
		return nil
	}

	for _, testSetID := range testSets {

		testSet, err := t.testSetConf.Read(ctx, testSetID)
		if err == nil && (testSet != nil && testSet.Template != nil) {
			utils.TemplatizedValues = testSet.Template
		} else {
			utils.TemplatizedValues = make(map[string]interface{})
		}

		if err == nil && (testSet != nil && testSet.Secret != nil) {
			utils.SecretValues = testSet.Secret
		} else {
			utils.SecretValues = make(map[string]interface{})
		}

		// Get test cases from the database
		tcs, err := t.testDB.GetTestCases(ctx, testSetID)
		if err != nil {
			utils.LogError(t.logger, err, "failed to get test cases")
			return err
		}

		if len(tcs) == 0 {
			t.logger.Warn("The test set is empty. Please record some test cases to templatize.", zap.String("testSet", testSetID))
			continue
		}
		fmt.Println("Processing test cases for test set:", testSetID)
		err = t.ProcessTestCases(ctx, tcs, testSetID)
		if err != nil {
			utils.LogError(t.logger, err, "failed to process test cases")
			return err
		}
	}
	return nil
}

// instrumentation metrics and caches
var (
	templMetrics struct {
		addTemplatesCalls        uint64
		addTemplates1Calls       uint64
		addTemplates1CacheHits   uint64
		addTemplates1CacheMisses uint64
		parseCacheHits           uint64
		parseCacheMisses         uint64
	}
	addTemplates1Cache   = make(map[string]at1CacheVal)
	addTemplates1CacheMu sync.RWMutex
	parseCache           = make(map[string]interface{})
	parseCacheMu         sync.RWMutex
)

func resetTemplMetrics() {
	atomic.StoreUint64(&templMetrics.addTemplatesCalls, 0)
	atomic.StoreUint64(&templMetrics.addTemplates1Calls, 0)
	atomic.StoreUint64(&templMetrics.addTemplates1CacheHits, 0)
	atomic.StoreUint64(&templMetrics.addTemplates1CacheMisses, 0)
	atomic.StoreUint64(&templMetrics.parseCacheHits, 0)
	atomic.StoreUint64(&templMetrics.parseCacheMisses, 0)
	addTemplates1CacheMu.Lock()
	addTemplates1Cache = make(map[string]at1CacheVal)
	addTemplates1CacheMu.Unlock()
	parseCacheMu.Lock()
	parseCache = make(map[string]interface{})
	parseCacheMu.Unlock()
}

func (t *Tools) ProcessTestCases(ctx context.Context, tcs []*models.TestCase, testSetID string) error {
	resetTemplMetrics()
	allStart := time.Now()

	// Fast path for large test sets: avoid O(N^2) pairwise comparisons and deep recursion.
	const largeSetThreshold = 600 // heuristic; adjust based on profiling.
	if len(tcs) >= largeSetThreshold {
		t.logger.Info("using large-set optimized templatization path", zap.Int("testcases", len(tcs)))
		if err := t.fastTemplatizeLargeSet(ctx, tcs, testSetID); err != nil {
			utils.LogError(t.logger, err, "fast templatization failed; falling back to legacy path")
		} else {
			// persist and return early
			for _, tc := range tcs {
				tc.HTTPReq.Body = removeQuotesInTemplates(tc.HTTPReq.Body)
				tc.HTTPResp.Body = removeQuotesInTemplates(tc.HTTPResp.Body)
				if err := t.testDB.UpdateTestCase(ctx, tc, testSetID, false); err != nil {
					utils.LogError(t.logger, err, "failed to update test case after fast templating")
				}
			}
			utils.RemoveDoubleQuotes(utils.TemplatizedValues)
			existingTestSet, _ := t.testSetConf.Read(ctx, testSetID)
			var existingMetadata map[string]interface{}
			if existingTestSet != nil {
				existingMetadata = existingTestSet.Metadata
			}
			if err := t.testSetConf.Write(ctx, testSetID, &models.TestSet{Template: utils.TemplatizedValues, Metadata: existingMetadata}); err != nil {
				utils.LogError(t.logger, err, "failed to write test set (fast path)")
			}
			t.logger.Info("completed fast templatization", zap.Duration("duration", time.Since(allStart)),
				zap.Int("finalTemplateVars", len(utils.TemplatizedValues)))
			return nil
		}
	}

	// In test cases, we often use placeholders like {{float .id}} for templatized variables. Ideally, we should wrap
	// them in double quotes, i.e., "{{float .id}}", to prevent errors during JSON unmarshaling. However, we avoid doing
	// this to prevent user confusion. If a user sees "{{float .id}}", they might wonder whether it's a string or a float.
	//
	// To maintain clarity, we remove these placeholders during marshalling and reintroduce them during unmarshalling.
	//
	// Note: This conversion is applied only to `reqBody` and `respBody` because all other fields are strings, and
	// templatized variables in those cases are simply concatenated.
	//
	// Example:
	//
	// Request:
	//   method: GET
	//   url: http://localhost:8080/api/employees/{{string .id}}
	//
	// Response:
	//   status_code: 200
	//   header:
	//     Content-Type: application/json
	//     Date: Fri, 19 Jan 2024 06:06:03 GMT
	//   body: '{"id":{{float .id}},"firstName":"0","lastName":"0","email":"0"}'
	//
	// Notice that even if we omit quotes in the URL, marshalling does not fail. However, when unmarshalling `respBody`,
	// it will throw an error if placeholders like `{{float .id}}` are not properly handled.
	for _, tc := range tcs {
		tc.HTTPReq.Body = addQuotesInTemplates(tc.HTTPReq.Body)
		tc.HTTPResp.Body = addQuotesInTemplates(tc.HTTPResp.Body)
	}

	fmt.Println("Inside process testcases .. Templatizing test cases for test set:", testSetID)

	// Process test cases for different scenarios and update the tcs and utils.TemplatizedValues
	// Case 1: Response Body of one test case to Request Headers of other test cases
	// (use case: Authorization token)
	stageStart := time.Now()
	t.processRespBodyToReqHeader(ctx, tcs)
	t.logger.Debug("templating stage done", zap.String("stage", "RespBodyToReqHeader"), zap.Duration("duration", time.Since(stageStart)))

	// Case 2: Request Headers of one test case to Request Headers of other test cases
	// (use case: Authorization token if Login API is not present in the test set)
	stageStart = time.Now()
	t.processReqHeadersToReqHeader(ctx, tcs)
	t.logger.Debug("templating stage done", zap.String("stage", "ReqHeadersToReqHeader"), zap.Duration("duration", time.Since(stageStart)))

	// Case 3: Response Body of one test case to Response Headers of other
	// (use case: POST - GET scenario)
	stageStart = time.Now()
	t.processRespBodyToReqURL(ctx, tcs)
	t.logger.Debug("templating stage done", zap.String("stage", "RespBodyToReqURL"), zap.Duration("duration", time.Since(stageStart)))

	// Case 4: Compare the req and resp body of one to other.
	// (use case: POST - PUT scenario)
	stageStart = time.Now()
	t.processRespBodyToReqBody(ctx, tcs)
	t.logger.Debug("templating stage done", zap.String("stage", "RespBodyToReqBody"), zap.Duration("duration", time.Since(stageStart)))

	// Case 5: Compare the req and resp for same test case for any common fields.
	// (use case: POST) request and response both have same fields.
	stageStart = time.Now()
	t.processBody(ctx, tcs)
	t.logger.Debug("templating stage done", zap.String("stage", "BodySameTest"), zap.Duration("duration", time.Since(stageStart)))

	// Case 6: Compare the req url with the response body of same test for any common fields.
	// (use case: GET) URL might container same fields as response body.
	stageStart = time.Now()
	t.processReqURLToRespBodySameTest(ctx, tcs)
	t.logger.Debug("templating stage done", zap.String("stage", "ReqURLToRespBodySameTest"), zap.Duration("duration", time.Since(stageStart)))

	// case 7: Compare the resp body of one test with the response body of other tests for any common fields.
	// (use case: POST - GET scenario)
	stageStart = time.Now()
	t.processRespBodyToRespBody(ctx, tcs)
	t.logger.Debug("templating stage done", zap.String("stage", "RespBodyToRespBody"), zap.Duration("duration", time.Since(stageStart)))

	// case 7: Compare the req body of one test with the response body of other tests for any common fields.
	// (use case: POST - GET scenario)
	stageStart = time.Now()
	t.processReqBodyToRespBody(ctx, tcs)
	t.logger.Debug("templating stage done", zap.String("stage", "ReqBodyToRespBody"), zap.Duration("duration", time.Since(stageStart)))

	// case 8: Compare the req body of one test with the req URL of other tests for any common fields.
	// (use case: POST - GET scenario)
	stageStart = time.Now()
	t.processReqBodyToReqURL(ctx, tcs)
	t.logger.Debug("templating stage done", zap.String("stage", "ReqBodyToReqURL"), zap.Duration("duration", time.Since(stageStart)))

	// case 9: Compare the req body of one test with the req body of other tests for any common fields.
	// (use case: POST - PUT scenario)
	stageStart = time.Now()
	t.processReqBodyToReqBody(ctx, tcs)
	t.logger.Debug("templating stage done", zap.String("stage", "ReqBodyToReqBody"), zap.Duration("duration", time.Since(stageStart)))

	// case 10: Compare the req URL of one test with the req body of other tests for any common fields.
	// (use case: GET - PUT scenario)
	stageStart = time.Now()
	t.processReqURLToReqBody(ctx, tcs)
	t.logger.Debug("templating stage done", zap.String("stage", "ReqURLToReqBody"), zap.Duration("duration", time.Since(stageStart)))

	// case 11: Compare the req URL of one test with the req URL of other tests for any common fields
	// (use case: GET - PUT scenario)
	stageStart = time.Now()
	t.processReqURLToReqURL(ctx, tcs)
	t.logger.Debug("templating stage done", zap.String("stage", "ReqURLToReqURL"), zap.Duration("duration", time.Since(stageStart)))

	// case 12: Compare the req URL of one test with the resp Body of other tests for any common fields
	// (use case: GET - PUT scenario)
	stageStart = time.Now()
	t.processReqURLToRespBody(ctx, tcs)
	t.logger.Debug("templating stage done", zap.String("stage", "ReqURLToRespBody"), zap.Duration("duration", time.Since(stageStart)))

	for _, tc := range tcs {
		tc.HTTPReq.Body = removeQuotesInTemplates(tc.HTTPReq.Body)
		tc.HTTPResp.Body = removeQuotesInTemplates(tc.HTTPResp.Body)
		err := t.testDB.UpdateTestCase(ctx, tc, testSetID, false)
		if err != nil {
			utils.LogError(t.logger, err, "failed to update test case")
			return err
		}
	}

	utils.RemoveDoubleQuotes(utils.TemplatizedValues)

	var existingMetadata map[string]interface{}
	existingTestSet, err := t.testSetConf.Read(ctx, testSetID)
	if err == nil && existingTestSet != nil && existingTestSet.Metadata != nil {
		existingMetadata = existingTestSet.Metadata
	}

	err = t.testSetConf.Write(ctx, testSetID, &models.TestSet{
		PreScript:  "",
		PostScript: "",
		Template:   utils.TemplatizedValues,
		Metadata:   existingMetadata,
	})
	if err != nil {
		utils.LogError(t.logger, err, "failed to write test set")
		return err
	}

	if len(utils.SecretValues) > 0 {
		err = utils.AddToGitIgnore(t.logger, t.config.Path, "/*/secret.yaml")
		if err != nil {
			t.logger.Warn("Failed to add secret files to .gitignore", zap.Error(err))
		}
	}

	t.logger.Info("templating summary",
		zap.String("testSet", testSetID),
		zap.Duration("totalDuration", time.Since(allStart)),
		zap.Uint64("addTemplates_calls", atomic.LoadUint64(&templMetrics.addTemplatesCalls)),
		zap.Uint64("addTemplates1_calls", atomic.LoadUint64(&templMetrics.addTemplates1Calls)),
		zap.Uint64("addTemplates1_cache_hits", atomic.LoadUint64(&templMetrics.addTemplates1CacheHits)),
		zap.Uint64("addTemplates1_cache_misses", atomic.LoadUint64(&templMetrics.addTemplates1CacheMisses)),
		zap.Uint64("parse_cache_hits", atomic.LoadUint64(&templMetrics.parseCacheHits)),
		zap.Uint64("parse_cache_misses", atomic.LoadUint64(&templMetrics.parseCacheMisses)),
	)
	return nil
}

// occurrence represents a single location of a primitive value in the test corpus.
type occurrence struct {
	testIdx     int
	locType     string // header_req, header_resp, req_body, resp_body, url_path_seg, url_query
	key         string // header key or body key
	isAuthToken bool
	valueType   string // string|int|float
}

// fastTemplatizeLargeSet performs scalable templatization by building an index of repeated values instead of pairwise scanning.
func (t *Tools) fastTemplatizeLargeSet(ctx context.Context, tcs []*models.TestCase, testSetID string) error {
	start := time.Now()
	// Step 1: parse bodies concurrently and build value index.
	type bodyData struct {
		req  map[string]interface{}
		resp map[string]interface{}
	}
	bodies := make([]bodyData, len(tcs))
	valueIndex := make(map[string][]occurrence) // canonical value string -> occurrences
	type valOcc struct {
		val string
		occ occurrence
	}
	occCh := make(chan valOcc, 10000)
	var aggWg sync.WaitGroup
	aggWg.Add(1)
	go func() {
		defer aggWg.Done()
		for vo := range occCh {
			if vo.val == "" {
				continue
			}
			if len(vo.val) > 512 {
				continue
			}
			valueIndex[vo.val] = append(valueIndex[vo.val], vo.occ)
		}
	}()
	sendOcc := func(valStr string, occ occurrence) {
		select {
		case occCh <- valOcc{val: valStr, occ: occ}:
		default:
			// drop if channel saturated to avoid blocking; acceptable tradeoff for large sets
		}
	}

	// Worker pool
	workerCount := 8
	if len(tcs) < workerCount {
		workerCount = len(tcs)
	}
	jobs := make(chan int, len(tcs))
	var wg sync.WaitGroup
	parseErrCh := make(chan error, workerCount)
	for w := 0; w < workerCount; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for idx := range jobs {
				if ctx.Err() != nil {
					return
				}
				tc := tcs[idx]
				// Headers (request)
				for hk, hv := range tc.HTTPReq.Header {
					if hk == "Date" || hk == "User-Agent" {
						continue
					}
					// Authorization: keep scheme separate
					if strings.EqualFold(hk, "Authorization") && strings.Contains(hv, " ") {
						parts := strings.SplitN(hv, " ", 2)
						sendOcc(parts[1], occurrence{testIdx: idx, locType: "header_req", key: hk, isAuthToken: true, valueType: "string"})
						continue
					}
					sendOcc(hv, occurrence{testIdx: idx, locType: "header_req", key: hk, valueType: "string"})
				}
				// Parse request body
				if bd := tc.HTTPReq.Body; bd != "" && strings.HasPrefix(strings.TrimSpace(bd), "{") {
					if m, err := parseIntoJSON(bd); err == nil && m != nil {
						if obj, ok := m.(map[string]interface{}); ok {
							bodies[idx].req = obj
							for k, v := range obj {
								switch val := v.(type) {
								case string:
									sendOcc(val, occurrence{testIdx: idx, locType: "req_body", key: k, valueType: "string"})
								case float64:
									sendOcc(fmt.Sprint(val), occurrence{testIdx: idx, locType: "req_body", key: k, valueType: "float"})
								case int, int64:
									sendOcc(fmt.Sprint(val), occurrence{testIdx: idx, locType: "req_body", key: k, valueType: "int"})
								}
							}
						}
					}
				}
				// Parse response body
				if bd := tc.HTTPResp.Body; bd != "" && strings.HasPrefix(strings.TrimSpace(bd), "{") {
					if m, err := parseIntoJSON(bd); err == nil && m != nil {
						if obj, ok := m.(map[string]interface{}); ok {
							bodies[idx].resp = obj
							for k, v := range obj {
								switch val := v.(type) {
								case string:
									sendOcc(val, occurrence{testIdx: idx, locType: "resp_body", key: k, valueType: "string"})
								case float64:
									sendOcc(fmt.Sprint(val), occurrence{testIdx: idx, locType: "resp_body", key: k, valueType: "float"})
								case int, int64:
									sendOcc(fmt.Sprint(val), occurrence{testIdx: idx, locType: "resp_body", key: k, valueType: "int"})
								}
							}
						}
					}
				}
			}
		}()
	}
	for i := range tcs {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	close(occCh)
	aggWg.Wait()
	close(parseErrCh)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	t.logger.Debug("index built", zap.Int("uniqueValues", len(valueIndex)), zap.Duration("t", time.Since(start)))

	// Step 2: choose values to templatize (appearing >=2 times and not purely constant trivial tokens).
	templateMap := make(map[string]string) // value -> varName
	usedNames := make(map[string]struct{})
	nextID := 0
	pickName := func(base string) string {
		base = strings.TrimSpace(base)
		if base == "" {
			base = "var"
		}
		// sanitize
		base = regexp.MustCompile(`[^a-zA-Z0-9_]`).ReplaceAllString(base, "")
		if base == "" {
			base = "var"
		}
		name := base
		for {
			if _, ok := usedNames[name]; !ok {
				usedNames[name] = struct{}{}
				return name
			}
			nextID++
			name = fmt.Sprintf("%s%d", base, nextID)
		}
	}

	for valStr, occs := range valueIndex {
		if len(occs) < 2 {
			continue
		}
		// skip numbers that look like timestamps ( > 10 digits ) to reduce noise.
		if len(valStr) > 12 && regexp.MustCompile(`^[0-9]+$`).MatchString(valStr) {
			continue
		}
		// derive base name from first occurrence key; for Authorization tokens pick access_token.
		first := occs[0]
		base := first.key
		if first.isAuthToken {
			base = "access_token"
		}
		if base == "" {
			base = "val"
		}
		// unify some common header names
		base = strings.ReplaceAll(strings.ToLower(base), "-", "_")
		templateMap[valStr] = pickName(base)
	}
	t.logger.Debug("selected values for templating", zap.Int("count", len(templateMap)))

	// Step 3: populate utils.TemplatizedValues and apply replacements.
	for valStr, name := range templateMap {
		// Determine sample occurrence to infer type.
		occ := valueIndex[valStr][0]
		switch occ.valueType {
		case "int":
			if i, err := strconv.Atoi(valStr); err == nil {
				utils.TemplatizedValues[name] = i
			} else {
				utils.TemplatizedValues[name] = valStr
			}
		case "float":
			if f, err := strconv.ParseFloat(valStr, 64); err == nil {
				utils.TemplatizedValues[name] = f
			} else {
				utils.TemplatizedValues[name] = valStr
			}
		default:
			utils.TemplatizedValues[name] = valStr
		}
	}

	// Apply replacements
	for valStr, occs := range valueIndex {
		varName, ok := templateMap[valStr]
		if !ok {
			continue
		}
		tmplStr := func(vType string) string {
			switch vType {
			case "int":
				return fmt.Sprintf("{{int .%s }}", varName)
			case "float":
				return fmt.Sprintf("{{float .%s }}", varName)
			default:
				return fmt.Sprintf("{{string .%s }}", varName)
			}
		}
		for _, occ := range occs {
			tc := tcs[occ.testIdx]
			switch occ.locType {
			case "header_req":
				if occ.isAuthToken {
					// reconstruct with scheme
					parts := strings.SplitN(tc.HTTPReq.Header[occ.key], " ", 2)
					if len(parts) == 2 {
						tc.HTTPReq.Header[occ.key] = parts[0] + " " + tmplStr("string")
					} else {
						tc.HTTPReq.Header[occ.key] = tmplStr("string")
					}
				} else {
					tc.HTTPReq.Header[occ.key] = tmplStr(occ.valueType)
				}
			case "req_body":
				if bodies[occ.testIdx].req != nil {
					bodies[occ.testIdx].req[occ.key] = wrapTemplateRaw(occ.valueType, varName)
				}
			case "resp_body":
				if bodies[occ.testIdx].resp != nil {
					bodies[occ.testIdx].resp[occ.key] = wrapTemplateRaw(occ.valueType, varName)
				}
			}
		}
	}

	// Marshal modified bodies back
	for i := range tcs {
		if bodies[i].req != nil {
			if b, err := json.Marshal(bodies[i].req); err == nil {
				tcs[i].HTTPReq.Body = string(b)
			}
		}
		if bodies[i].resp != nil {
			if b, err := json.Marshal(bodies[i].resp); err == nil {
				tcs[i].HTTPResp.Body = string(b)
			}
		}
	}

	t.logger.Info("fast templating result", zap.Int("templateVars", len(utils.TemplatizedValues)), zap.Duration("duration", time.Since(start)))
	return nil
}

// wrapTemplateRaw returns the raw placeholder without quotes for numeric types so later removal logic can adjust.
func wrapTemplateRaw(valType, name string) interface{} {
	switch valType {
	case "int":
		return fmt.Sprintf("{{int .%s }}", name)
	case "float":
		return fmt.Sprintf("{{float .%s }}", name)
	default:
		return fmt.Sprintf("{{string .%s }}", name)
	}
}

func (t *Tools) processRespBodyToReqHeader(ctx context.Context, tcs []*models.TestCase) {
	for i := 0; i < len(tcs)-1; i++ {
		jsonResponse, err := parseIntoJSON(tcs[i].HTTPResp.Body)
		if err != nil {
			t.logger.Error("failed to parse response body, skipping RespBodyToReqHeader Template processing", zap.Any("testcase", tcs[i].Name), zap.Error(err))
			continue
		}
		if jsonResponse == nil {
			t.logger.Warn("Skipping RespBodyToReqHeader Template processing for test case", zap.Any("testcase", tcs[i].Name), zap.Error(err))
			continue
		}
		for j := i + 1; j < len(tcs); j++ {
			select {
			case <-ctx.Done():
				return
			default:
			}
			addTemplates(t.logger, tcs[j].HTTPReq.Header, jsonResponse)
		}
		tcs[i].HTTPResp.Body = marshalJSON(jsonResponse, t.logger)
	}
}

func (t *Tools) processReqHeadersToReqHeader(ctx context.Context, tcs []*models.TestCase) {
	for i := 0; i < len(tcs)-1; i++ {
		for j := i + 1; j < len(tcs); j++ {
			select {
			case <-ctx.Done():
				return
			default:
			}
			compareReqHeaders(t.logger, tcs[j].HTTPReq.Header, tcs[i].HTTPReq.Header)
		}
	}
}

func (t *Tools) processRespBodyToReqURL(ctx context.Context, tcs []*models.TestCase) {
	for i := 0; i < len(tcs)-1; i++ {
		jsonResponse, err := parseIntoJSON(tcs[i].HTTPResp.Body)
		if err != nil || jsonResponse == nil {
			t.logger.Debug("Skipping response to URL processing for test case", zap.Any("testcase", tcs[i].Name), zap.Error(err))
			continue
		}
		for j := i + 1; j < len(tcs); j++ {
			select {
			case <-ctx.Done():
				return
			default:
			}
			addTemplates(t.logger, &tcs[j].HTTPReq.URL, jsonResponse)
		}
		tcs[i].HTTPResp.Body = marshalJSON(jsonResponse, t.logger)
	}
}

func (t *Tools) processRespBodyToReqBody(ctx context.Context, tcs []*models.TestCase) {
	for i := 0; i < len(tcs)-1; i++ {
		jsonResponse, err := parseIntoJSON(tcs[i].HTTPResp.Body)
		if err != nil || jsonResponse == nil {
			t.logger.Debug("Skipping response to request body processing for test case", zap.Any("testcase", tcs[i].Name), zap.Error(err))
			continue
		}
		for j := i + 1; j < len(tcs); j++ {
			select {
			case <-ctx.Done():
				return
			default:
			}
			jsonRequest, err := parseIntoJSON(tcs[j].HTTPReq.Body)
			if err != nil || jsonRequest == nil {
				t.logger.Debug("Skipping request body processing for test case", zap.Any("testcase", tcs[j].Name), zap.Error(err))
				continue
			}
			addTemplates(t.logger, jsonRequest, jsonResponse)
			tcs[j].HTTPReq.Body = marshalJSON(jsonRequest, t.logger)
		}
		tcs[i].HTTPResp.Body = marshalJSON(jsonResponse, t.logger)
	}
}

func (t *Tools) processBody(ctx context.Context, tcs []*models.TestCase) {
	for i := 0; i < len(tcs); i++ {
		select {
		case <-ctx.Done():
			return
		default:
		}
		jsonResponse, err := parseIntoJSON(tcs[i].HTTPResp.Body)
		if err != nil || jsonResponse == nil {
			t.logger.Debug("Skipping response to request body processing for test case", zap.Any("testcase", tcs[i].Name), zap.Error(err))
			continue
		}
		jsonRequest, err := parseIntoJSON(tcs[i].HTTPReq.Body)
		if err != nil || jsonRequest == nil {
			t.logger.Debug("Skipping request body processing for test case", zap.Any("testcase", tcs[i].Name), zap.Error(err))
			continue
		}
		addTemplates(t.logger, jsonResponse, jsonRequest)
		tcs[i].HTTPReq.Body = marshalJSON(jsonRequest, t.logger)
		tcs[i].HTTPResp.Body = marshalJSON(jsonResponse, t.logger)
	}
}

func (t *Tools) processReqURLToRespBodySameTest(ctx context.Context, tcs []*models.TestCase) {
	for i := 0; i < len(tcs); i++ {
		select {
		case <-ctx.Done():
			return
		default:
		}
		jsonResponse, err := parseIntoJSON(tcs[i].HTTPResp.Body)
		if err != nil || jsonResponse == nil {
			t.logger.Debug("Skipping response to URL processing for test case", zap.Any("testcase", tcs[i].Name), zap.Error(err))
			continue
		}
		addTemplates(t.logger, &tcs[i].HTTPReq.URL, jsonResponse)
		tcs[i].HTTPResp.Body = marshalJSON(jsonResponse, t.logger)
	}
}

func (t *Tools) processRespBodyToRespBody(ctx context.Context, tcs []*models.TestCase) {
	for i := 0; i < len(tcs)-1; i++ {
		jsonResponse, err := parseIntoJSON(tcs[i].HTTPResp.Body)
		if err != nil || jsonResponse == nil {
			t.logger.Debug("Skipping response to request body processing for test case", zap.Any("testcase", tcs[i].Name), zap.Error(err))
			continue
		}
		for j := i + 1; j < len(tcs); j++ {
			select {
			case <-ctx.Done():
				return
			default:
			}
			jsonResponse2, err := parseIntoJSON(tcs[j].HTTPResp.Body)
			if err != nil || jsonResponse2 == nil {
				t.logger.Debug("Skipping request body processing for test case", zap.Any("testcase", tcs[j].Name), zap.Error(err))
				continue
			}
			addTemplates(t.logger, jsonResponse2, jsonResponse)
			tcs[j].HTTPResp.Body = marshalJSON(jsonResponse2, t.logger)
		}
		tcs[i].HTTPResp.Body = marshalJSON(jsonResponse, t.logger)
	}
}

func (t *Tools) processReqBodyToRespBody(ctx context.Context, tcs []*models.TestCase) {
	for i := 0; i < len(tcs)-1; i++ {
		jsonRequest, err := parseIntoJSON(tcs[i].HTTPReq.Body)
		if err != nil || jsonRequest == nil {
			t.logger.Debug("Skipping response to request body processing for test case", zap.Any("testcase", tcs[i].Name), zap.Error(err))
			continue
		}
		for j := i + 1; j < len(tcs); j++ {
			select {
			case <-ctx.Done():
				return
			default:
			}
			jsonResponse, err := parseIntoJSON(tcs[j].HTTPResp.Body)
			if err != nil || jsonResponse == nil {
				t.logger.Debug("Skipping request body processing for test case", zap.Any("testcase", tcs[j].Name), zap.Error(err))
				continue
			}
			addTemplates(t.logger, jsonResponse, jsonRequest)
			tcs[j].HTTPResp.Body = marshalJSON(jsonResponse, t.logger)
		}
		tcs[i].HTTPReq.Body = marshalJSON(jsonRequest, t.logger)
	}
}

func (t *Tools) processReqBodyToReqURL(ctx context.Context, tcs []*models.TestCase) {
	for i := 0; i < len(tcs)-1; i++ {
		jsonRequest, err := parseIntoJSON(tcs[i].HTTPReq.Body)
		if err != nil || jsonRequest == nil {
			t.logger.Debug("Skipping response to URL processing for test case", zap.Any("testcase", tcs[i].Name), zap.Error(err))
			continue
		}
		for j := i + 1; j < len(tcs); j++ {
			select {
			case <-ctx.Done():
				return
			default:
			}
			addTemplates(t.logger, &tcs[j].HTTPReq.URL, jsonRequest)
		}
		tcs[i].HTTPReq.Body = marshalJSON(jsonRequest, t.logger)
	}
}

func (t *Tools) processReqBodyToReqBody(ctx context.Context, tcs []*models.TestCase) {
	for i := 0; i < len(tcs)-1; i++ {
		jsonRequest, err := parseIntoJSON(tcs[i].HTTPReq.Body)
		if err != nil || jsonRequest == nil {
			t.logger.Debug("Skipping response to request body processing for test case", zap.Any("testcase", tcs[i].Name), zap.Error(err))
			continue
		}
		for j := i + 1; j < len(tcs); j++ {
			select {
			case <-ctx.Done():
				return
			default:
			}
			jsonRequest2, err := parseIntoJSON(tcs[j].HTTPReq.Body)
			if err != nil || jsonRequest2 == nil {
				t.logger.Debug("Skipping request body processing for test case", zap.Any("testcase", tcs[j].Name), zap.Error(err))
				continue
			}
			addTemplates(t.logger, jsonRequest2, jsonRequest)
			tcs[j].HTTPReq.Body = marshalJSON(jsonRequest2, t.logger)
		}
		tcs[i].HTTPReq.Body = marshalJSON(jsonRequest, t.logger)
	}
}

func (t *Tools) processReqURLToReqBody(ctx context.Context, tcs []*models.TestCase) {
	for i := 0; i < len(tcs)-1; i++ {
		for j := i + 1; j < len(tcs); j++ {
			select {
			case <-ctx.Done():
				return
			default:
			}
			jsonRequest, err := parseIntoJSON(tcs[j].HTTPReq.Body)
			if err != nil || jsonRequest == nil {
				t.logger.Debug("Skipping request body processing for test case", zap.Any("testcase", tcs[j].Name), zap.Error(err))
				continue
			}
			addTemplates(t.logger, jsonRequest, &tcs[i].HTTPReq.URL)
			tcs[j].HTTPReq.Body = marshalJSON(jsonRequest, t.logger)
		}
	}
}

func (t *Tools) processReqURLToRespBody(ctx context.Context, tcs []*models.TestCase) {
	for i := 0; i < len(tcs)-1; i++ {
		for j := 0; j < len(tcs); j++ {
			select {
			case <-ctx.Done():
				return
			default:
			}
			jsonResponse, err := parseIntoJSON(tcs[j].HTTPResp.Body)
			if err != nil || jsonResponse == nil {
				t.logger.Debug("Skipping request body processing for test case", zap.Any("testcase", tcs[j].Name), zap.Error(err))
				continue
			}
			addTemplates(t.logger, jsonResponse, &tcs[i].HTTPReq.URL)
			tcs[j].HTTPResp.Body = marshalJSON(jsonResponse, t.logger)
		}
	}
}

func (t *Tools) processReqURLToReqURL(ctx context.Context, tcs []*models.TestCase) {
	for i := 0; i < len(tcs)-1; i++ {
		for j := i + 1; j < len(tcs); j++ {
			select {
			case <-ctx.Done():
				return
			default:
			}
			addTemplates(t.logger, &tcs[j].HTTPReq.URL, &tcs[i].HTTPReq.URL)
		}
	}
}

// Utility function to safely marshal JSON and log errors
func marshalJSON(data interface{}, logger *zap.Logger) string {
	jsonData, err := json.Marshal(data)
	if err != nil {
		utils.LogError(logger, err, "failed to marshal JSON data")
		return ""
	}
	return string(jsonData)
}

func parseIntoJSON(response string) (interface{}, error) {
	if response == "" {
		return nil, nil
	}
	parseCacheMu.RLock()
	if v, ok := parseCache[response]; ok {
		parseCacheMu.RUnlock()
		atomic.AddUint64(&templMetrics.parseCacheHits, 1)
		return v, nil
	}
	parseCacheMu.RUnlock()
	atomic.AddUint64(&templMetrics.parseCacheMisses, 1)
	result, err := geko.JSONUnmarshal([]byte(response))
	if err != nil {
		return nil, fmt.Errorf("failed to unmarshal the response: %v", err)
	}
	parseCacheMu.Lock()
	parseCache[response] = result
	parseCacheMu.Unlock()
	return result, nil
}

func RenderIfTemplatized(val interface{}) (bool, interface{}, error) {
	stringVal, ok := val.(string)
	if !ok {
		return false, val, nil
	}

	// Check if the value is a template.
	// Applied this nolint to ignore the staticcheck error here because of readability
	// nolint:staticcheck
	if !(strings.Contains(stringVal, "{{") && strings.Contains(stringVal, "}}")) {
		return false, val, nil
	}

	// Get the value from the template.
	val, err := render(stringVal)
	if err != nil {
		return false, val, err
	}

	return true, val, nil
}

// addTemplates performs recursive template inference while guarding against deep or cyclic structures.
// It previously recursed without any depth/visited protection which could lead to excessive CPU time on
// large / repetitive inputs. We add a small state object to short‑circuit repeats and a max depth.
func addTemplates(logger *zap.Logger, interface1 interface{}, interface2 interface{}) bool {
	atomic.AddUint64(&templMetrics.addTemplatesCalls, 1)
	const maxDepth = 25
	visited := make(map[uintptr]struct{})
	var recur func(cur interface{}, peer interface{}, depth int) bool

	mark := func(x interface{}) (uintptr, bool) {
		// We only track pointers, maps, slices (reference types) to avoid infinite revisits.
		rv := reflect.ValueOf(x)
		switch rv.Kind() {
		case reflect.Ptr, reflect.Map, reflect.Slice:
			if rv.IsNil() {
				return 0, false
			}
			ptr := rv.Pointer()
			if _, ok := visited[ptr]; ok {
				return ptr, true
			}
			visited[ptr] = struct{}{}
			return ptr, false
		default:
			return 0, false
		}
	}

	changed := false
	recur = func(cur interface{}, peer interface{}, depth int) bool {
		atomic.AddUint64(&templMetrics.addTemplatesCalls, 1)
		if depth > maxDepth {
			logger.Debug("templating depth limit reached, skipping deeper traversal", zap.Int("depth", depth))
			return false
		}
		if _, already := mark(cur); already {
			return false
		}
		switch v := cur.(type) {
		case geko.ObjectItems:
			keys := v.Keys()
			vals := v.Values()
			for i := range keys {
				var err error
				var isTemplatized bool
				original := vals[i]
				isTemplatized, vals[i], err = RenderIfTemplatized(vals[i])
				if err != nil {
					return changed
				}
				switch vals[i].(type) {
				case string:
					x := vals[i].(string)
					if recur(&x, peer, depth+1) {
						vals[i] = x
					}
				case float32, float64, int, int64:
					x := interface{}(vals[i])
					if recur(&x, peer, depth+1) {
						vals[i] = x
					}
				default:
					recur(vals[i], peer, depth+1)
				}
				if isTemplatized {
					v.SetValueByIndex(i, original)
				} else {
					v.SetValueByIndex(i, vals[i])
				}
			}
		case geko.Array:
			for i, val := range v.List {
				var err error
				var isTemplatized bool
				original := val
				isTemplatized, val, err = RenderIfTemplatized(val)
				if err != nil {
					return changed
				}
				switch x := val.(type) {
				case string:
					if recur(&x, peer, depth+1) {
						v.List[i] = x
					} else {
						v.List[i] = x
					}
				case float32, float64, int, int64:
					tmp := interface{}(x)
					recur(&tmp, peer, depth+1)
					v.List[i] = tmp
				default:
					recur(v.List[i], peer, depth+1)
				}
				if isTemplatized {
					v.Set(i, original)
				} else {
					v.Set(i, v.List[i])
				}
			}
		case map[string]string:
			for key, val := range v {
				var isTemplatized bool
				original := val
				isTemplatized, val1, err := RenderIfTemplatized(val)
				if err != nil {
					utils.LogError(logger, err, "failed to render for template")
					continue
				}
				sval, ok := val1.(string)
				if !ok {
					continue
				}
				authType := ""
				if key == "Authorization" && len(strings.Split(sval, " ")) > 1 {
					parts := strings.Split(sval, " ")
					authType = parts[0]
					sval = parts[1]
				}
				if addTemplates1(logger, &sval, peer) {
					changed = true
				}
				if authType != "" {
					sval = authType + " " + sval
				}
				if isTemplatized {
					v[key] = original
				} else {
					v[key] = sval
				}
			}
		case *string:
			original := *v
			isTemplatized, tempVal, err := RenderIfTemplatized(*v)
			if err != nil {
				utils.LogError(logger, err, "failed to render for template")
				return changed
			}
			sval, ok := tempVal.(string)
			if !ok {
				return changed
			}
			*v = sval
			if addTemplates1(logger, v, peer) { // direct body reuse
				changed = true
				return true
			}
			originalURL, err := url.Parse(original)
			if err != nil {
				return changed
			}
			parsed, err := url.Parse(*v)
			if err != nil || parsed.Scheme == "" || parsed.Host == "" {
				return changed
			}
			urlParts := strings.Split(parsed.Path, "/")
			originalURLParts := strings.Split(originalURL.Path, "/")
			if len(urlParts) > 0 {
				last := urlParts[len(urlParts)-1]
				if addTemplates1(logger, &last, peer) {
					changed = true
				}
				if isTemplatized && len(originalURLParts) == len(urlParts) {
					last = originalURLParts[len(originalURLParts)-1]
				}
				urlParts[len(urlParts)-1] = last
			}
			parsed.Path = strings.Join(urlParts, "/")
			if parsed.RawQuery != "" {
				queryParams := strings.Split(parsed.RawQuery, "&")
				for i, param := range queryParams {
					parts := strings.SplitN(param, "=", 2)
					if len(parts) != 2 {
						continue
					}
					val := parts[1]
					addTemplates1(logger, &val, peer)
					queryParams[i] = parts[0] + "=" + val
				}
				parsed.RawQuery = strings.Join(queryParams, "&")
				*v = fmt.Sprintf("%s://%s%s?%s", parsed.Scheme, parsed.Host, parsed.Path, parsed.RawQuery)
				return true
			}
			*v = fmt.Sprintf("%s://%s%s", parsed.Scheme, parsed.Host, parsed.Path)
		case *interface{}:
			switch w := (*v).(type) {
			case float64, int64, int, float32:
				var val string
				switch x := w.(type) {
				case float64:
					val = utils.ToString(x)
				case int64:
					val = utils.ToString(x)
				case int:
					val = utils.ToString(x)
				case float32:
					val = utils.ToString(x)
				}
				if addTemplates1(logger, &val, peer) {
					parts := strings.Split(val, " ")
					if len(parts) > 1 {
						parts1 := strings.Split(parts[0], "{{")
						if len(parts1) > 1 {
							val = parts1[0] + "{{" + getType(w) + " " + parts[1] + "}}"
						}
						*v = val
						changed = true
					}
				}
			default:
				logger.Debug("unsupported type while templatizing", zap.Any("type", w))
			}
		}
		return changed
	}

	return recur(interface1, interface2, 0)
}

// TODO: add better comment here and rename this function.
// Here we simplify the second interface and finally add the templates.
// caching wrapper for addTemplates1Core
type at1CacheVal struct {
	changed bool
	newVal  string
}

func buildAT1Key(val string, body interface{}) string {
	return fmt.Sprintf("%T|%s|%d", body, val, len(utils.TemplatizedValues))
}

func addTemplates1(logger *zap.Logger, val1 *string, body interface{}) bool {
	atomic.AddUint64(&templMetrics.addTemplates1Calls, 1)
	key := buildAT1Key(*val1, body)
	addTemplates1CacheMu.RLock()
	if cv, ok := addTemplates1Cache[key]; ok {
		addTemplates1CacheMu.RUnlock()
		atomic.AddUint64(&templMetrics.addTemplates1CacheHits, 1)
		if cv.changed {
			*val1 = cv.newVal
		}
		return cv.changed
	}
	addTemplates1CacheMu.RUnlock()
	atomic.AddUint64(&templMetrics.addTemplates1CacheMisses, 1)
	orig := *val1
	changed := addTemplates1Core(logger, val1, body)
	addTemplates1CacheMu.Lock()
	addTemplates1Cache[key] = at1CacheVal{changed: changed, newVal: *val1}
	addTemplates1CacheMu.Unlock()
	if !changed {
		*val1 = orig
	}
	return changed
}

// addTemplates1Core contains the original logic (slightly refactored) so that we can memoize the wrapper above.
func addTemplates1Core(logger *zap.Logger, val1 *string, body interface{}) bool {
	switch b := body.(type) {
	case geko.ObjectItems:
		keys := b.Keys()
		vals := b.Values()
		for i, key := range keys {
			var err error
			var isTemplatized bool
			original := vals[i]
			isTemplatized, vals[i], err = RenderIfTemplatized(vals[i])
			if err != nil {
				utils.LogError(logger, err, "failed to render for template")
				return false
			}
			var ok bool
			switch vals[i].(type) {
			case string:
				x := vals[i].(string)
				ok = addTemplates1(logger, val1, &x)
				vals[i] = x
			case float32:
				x := vals[i].(float32)
				ok = addTemplates1(logger, val1, &x)
				vals[i] = x
			case int:
				x := vals[i].(int)
				ok = addTemplates1(logger, val1, &x)
				vals[i] = x
			case int64:
				x := vals[i].(int64)
				ok = addTemplates1(logger, val1, &x)
				vals[i] = x
			case float64:
				x := vals[i].(float64)
				ok = addTemplates1(logger, val1, &x)
				vals[i] = x
			default:
				ok = addTemplates1(logger, val1, vals[i])
			}
			// we can't change if the type of vals[i] is also object item.
			if ok && reflect.TypeOf(vals[i]) != reflect.TypeOf(b) {
				newKey := insertUnique(key, *val1, utils.TemplatizedValues)
				vals[i] = fmt.Sprintf("{{%s .%v }}", getType(vals[i]), newKey)
				// Now change the value of the key in the object.
				b.SetValueByIndex(i, vals[i])
				*val1 = fmt.Sprintf("{{%s .%v }}", getType(*val1), newKey)
				return true
			}
			if isTemplatized {
				vals[i] = original
			}

		}
	case geko.Array:
		for i, v := range b.List {
			switch x := v.(type) {
			case string:
				addTemplates1(logger, val1, &x)
				b.List[i] = x
			case float32:
				addTemplates1(logger, val1, &x)
				b.List[i] = x
			case int:
				addTemplates1(logger, val1, &x)
				b.List[i] = x
			case int64:
				addTemplates1(logger, val1, &x)
				b.List[i] = x
			case float64:
				addTemplates1(logger, val1, &x)
				b.List[i] = x
			default:
				addTemplates1(logger, val1, b.List[i])
			}
			b.Set(i, b.List[i])
		}
	case map[string]string:
		for key, val2 := range b {
			var isTemplatized bool
			original := val2
			isTemplatized, tempVal, err := RenderIfTemplatized(val2)
			if err != nil {
				utils.LogError(logger, err, "failed to render for template")
				return false
			}
			val2, ok := (tempVal).(string)
			if !ok {
				continue
			}
			if *val1 == val2 {
				newKey := insertUnique(key, val2, utils.TemplatizedValues)
				b[key] = fmt.Sprintf("{{%s .%v }}", getType(val2), newKey)
				*val1 = fmt.Sprintf("{{%s .%v }}", getType(*val1), newKey)
				return true
			}
			if isTemplatized {
				b[key] = original
			}

		}
		return false
	case *string:
		_, tempVal, err := RenderIfTemplatized(b)
		if err != nil {
			utils.LogError(logger, err, "failed to render for template")
			return false
		}
		b, ok := (tempVal).(*string)
		if !ok {
			return false
		}
		if *val1 == *b {
			return true
		}
	case map[string]interface{}:
		for key, val2 := range b {
			var err error
			var isTemplatized bool
			original := val2
			isTemplatized, val2, err = RenderIfTemplatized(val2)
			if err != nil {
				utils.LogError(logger, err, "failed to render for template")
				return false
			}
			var ok bool
			switch x := val2.(type) {
			case string:
				ok = addTemplates1(logger, val1, &x)
			case float32:
				ok = addTemplates1(logger, val1, &x)
			case int:
				ok = addTemplates1(logger, val1, &x)
			case int64:
				ok = addTemplates1(logger, val1, &x)
			case float64:
				ok = addTemplates1(logger, val1, &x)
			default:
				ok = addTemplates1(logger, val1, val2)
			}

			if ok {
				newKey := insertUnique(key, *val1, utils.TemplatizedValues)
				if newKey == "" {
					newKey = key
				}
				b[key] = fmt.Sprintf("{{%s .%v}}", getType(b[key]), newKey)
				*val1 = fmt.Sprintf("{{%s .%v}}", getType(*val1), newKey)
			} else {
				if isTemplatized {
					b[key] = original
				}
			}
		}
	case *float64, *int64, *int, *float32:
		var val string
		switch x := b.(type) {
		case *float64:
			val = utils.ToString(*x)
		case *int64:
			val = utils.ToString(*x)
		case *int:
			val = utils.ToString(*x)
		case *float32:
			val = utils.ToString(*x)
		}
		if *val1 == val {
			return true
		}
	case []interface{}:
		for i, val := range b {
			switch x := val.(type) {
			case string:
				addTemplates1(logger, val1, &x)
				b[i] = x
			case float32:
				addTemplates1(logger, val1, &x)
				b[i] = x
			case int:
				addTemplates1(logger, val1, &x)
				b[i] = x
			case int64:
				addTemplates1(logger, val1, &x)
				b[i] = x
			case float64:
				addTemplates1(logger, val1, &x)
				b[i] = x
			default:
				addTemplates1(logger, val1, b[i])
			}
			b[i] = val
		}
	}
	return false
}

func getType(val interface{}) string {
	switch val.(type) {
	case string, *string:
		return "string"
	case int64, int, int32, *int64, *int, *int32:
		return "int"
	case float64, float32, *float64, *float32:
		return "float"
	}
	//TODO: handle the default case properly, return some errot.
	return ""
}

// This function returns a unique key for each value, for instance if id already exists, it will return id1.
func insertUnique(baseKey, value string, myMap map[string]interface{}) string {
	// If the key has more than one word seperated by a delimiter, remove the delimiter and add the key to the map.
	if strings.Contains(baseKey, "-") {
		baseKey = strings.ReplaceAll(baseKey, "-", "")
	}
	if myMap[baseKey] == value {
		return baseKey
	}
	key := baseKey
	i := 0
	for myMap[key] != value {
		if _, exists := myMap[key]; !exists {
			myMap[key] = value
			break
		}
		i++
		key = baseKey + strconv.Itoa(i)
	}
	return key
}

// TODO: Make this function generic for one value of string containing more than one template value.
// Duplicate function is being used in Simulate function as well.

// render function gives the value of the templatized field.
func render(val string) (interface{}, error) {
	// This is a map of helper functions that is used to convert the values to their appropriate types.
	funcMap := template.FuncMap{
		"int":    utils.ToInt,
		"string": utils.ToString,
		"float":  utils.ToFloat,
	}

	tmpl, err := template.New("template").Funcs(funcMap).Parse(val)
	if err != nil {
		return val, fmt.Errorf("failed to parse the testcase using template %v", zap.Error(err))
	}

	data := make(map[string]interface{})

	for k, v := range utils.TemplatizedValues {
		data[k] = v
	}
	fmt.Println("Templatized Values: ", utils.TemplatizedValues)
	if len(utils.SecretValues) > 0 {
		data["secret"] = utils.SecretValues
	}

	var output bytes.Buffer
	err = tmpl.Execute(&output, data)
	if err != nil {
		return val, fmt.Errorf("failed to execute the template %v", zap.Error(err))
	}

	if strings.Contains(val, "string") {
		return output.String(), nil
	}

	// Remove the double quotes from the output for rest of the values. (int, float)
	outputString := strings.Trim(output.String(), `"`)

	// TODO: why do we need this when we have already declared the funcMap.
	// Convert this to the appropriate type and return.
	switch {
	case strings.Contains(val, "int"):
		return utils.ToInt(output.String()), nil
	case strings.Contains(val, "float"):
		return utils.ToFloat(output.String()), nil
	}

	return outputString, nil
}

// Compare the headers of 2 utils.TemplatizedValues requests and add the templates.
func compareReqHeaders(logger *zap.Logger, req1 map[string]string, req2 map[string]string) {
	for key, val1 := range req1 {
		// Check if the value is already present in the templatized values.
		var isTemplatized1 bool
		original1 := val1
		isTemplatized1, tempVal, err := RenderIfTemplatized(val1)
		if err != nil {
			utils.LogError(logger, err, "failed to render for template")
			return
		}
		val, ok := (tempVal).(string)
		if !ok {
			continue
		}
		val1 = val
		if val2, ok := req2[key]; ok {
			var isTemplatized2 bool
			original2 := val2
			isTemplatized2, tempVal, err := RenderIfTemplatized(val2)
			if err != nil {
				utils.LogError(logger, err, "failed to render for template")
				return
			}
			val, ok = (tempVal).(string)
			if !ok {
				continue
			}
			val2 = val
			if val1 == val2 {
				// Trim the extra space in the string.
				val2 = strings.Trim(val2, " ")
				newKey := insertUnique(key, val2, utils.TemplatizedValues)
				if newKey == "" {
					newKey = key
				}
				req2[key] = fmt.Sprintf("{{%s .%v }}", getType(val2), newKey)
				req1[key] = fmt.Sprintf("{{%s .%v }}", getType(val2), newKey)
			} else {
				if isTemplatized2 {
					req2[key] = original2
				}
				if isTemplatized1 {
					req1[key] = original1
				}
			}
		}
	}
}

// Removing quotes in templates for non string types like float, int, etc. Because they interfere with the templating engine.
func removeQuotesInTemplates(jsonStr string) string {
	// Regular expression to find patterns with {{ and }}
	re := regexp.MustCompile(`"\{\{[^{}]*\}\}"`)
	// Function to replace matches by removing surrounding quotes
	result := re.ReplaceAllStringFunc(jsonStr, func(match string) string {
		if strings.Contains(match, "{{string") {
			return match
		}
		// Remove the surrounding quotes
		return strings.Trim(match, `"`)
	})

	return result
}

// Add quotes to the template if it is not of the type string. eg: "{{float .key}}" ,{{int .key}}
func addQuotesInTemplates(jsonStr string) string {
	// Regular expression to find patterns with {{ and }}
	re := regexp.MustCompile(`\{\{[^{}]*\}\}`)
	// Function to replace matches by removing surrounding quotes
	result := re.ReplaceAllStringFunc(jsonStr, func(match string) string {
		if strings.Contains(match, "{{string") {
			return match
		}
		//Add the surrounding quotes.
		return `"` + match + `"`
	})
	return result
}
