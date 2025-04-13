// Package postmanimport implements the import of a Postman collection to Keploy tests.
package postmanimport

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"go.keploy.io/server/v2/pkg"
	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/pkg/platform/yaml"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
	yamlLib "gopkg.in/yaml.v3"
)

const (
	postmanSchemaVersion = "https://schema.getpostman.com/json/collection/v2.0.0/collection.json"
	testSetNamePrefix    = "test-set-"
)

type PostmanImporter struct {
	logger    *zap.Logger
	ctx       context.Context
	toCapture bool
}

func NewPostmanImporter(ctx context.Context, logger *zap.Logger) *PostmanImporter {
	return &PostmanImporter{
		logger:    logger,
		ctx:       ctx,
		toCapture: true,
	}
}

func (pi *PostmanImporter) Import(collectionPath, basePath string) error {
	if err := pi.validateCollectionPath(collectionPath); err != nil {
		return err
	}

	collectionBytes, err := os.ReadFile(collectionPath)
	if err != nil {
		return fmt.Errorf("failed to read Postman collection file: %w", err)
	}

	postmanCollection, err := pi.parsePostmanCollection(collectionBytes)
	if err != nil {
		return err
	}

	pi.validatePostmanSchema(postmanCollection.Info.Schema)

	globalVariables := pi.extractGlobalVariables(postmanCollection.Variables)

	// Check for empty responses if basePath is not provided
	emptyResponsesExist := pi.scanForEmptyResponses(postmanCollection)
	if emptyResponsesExist {
		if !pi.promptUserForCapture() {
			pi.toCapture = false
			pi.logger.Warn("Few test cases will be skipped as responses are missing from the collection")
		}
	}

	if err := pi.importTestSets(postmanCollection, globalVariables, basePath); err != nil {
		return err
	}

	pi.logger.Info("✅ Postman Collection Successfully Imported To Keploy Tests 🎉")
	return nil
}

func (pi *PostmanImporter) validateCollectionPath(path string) error {
	if path == "" {
		return errors.New("path to Postman collection cannot be empty")
	}

	if !strings.HasSuffix(path, ".json") {
		return errors.New("invalid file type: expected .json Postman collection")
	}

	return nil
}

func (pi *PostmanImporter) parsePostmanCollection(collectionBytes []byte) (*PostmanCollectionStruct, error) {
	var postmanCollection PostmanCollectionStruct
	if err := json.Unmarshal(collectionBytes, &postmanCollection); err != nil {
		return nil, fmt.Errorf("failed to unmarshal Postman collection JSON: %w", err)
	}
	return &postmanCollection, nil
}

func (pi *PostmanImporter) validatePostmanSchema(schema string) {
	if schema != postmanSchemaVersion {
		pi.logger.Warn("Postman Collection schema mismatch", zap.String("expected", postmanSchemaVersion), zap.String("actual", schema))
	}
}

func (pi *PostmanImporter) extractGlobalVariables(variables []map[string]interface{}) map[string]string {
	globalVariables := make(map[string]string)

	for _, variable := range variables {
		// Skip disabled variables
		if variable["disabled"] != nil && variable["disabled"].(bool) {
			continue
		}

		// Extract and validate variable key
		key, ok := variable["key"].(string)
		if !ok {
			pi.logger.Error("Global variable key is not a string", zap.Any("key", variable["key"]))
			continue
		}

		// Extract and validate variable value
		value, ok := variable["value"].(string)
		if !ok {
			pi.logger.Error("Global variable value is not a string", zap.Any("value", variable["value"]))
			continue
		}

		globalVariables[key] = value
	}

	// Resolve variable dependencies
	for key, value := range globalVariables {
		globalVariables[key] = replaceTemplateVars(value, globalVariables)
	}

	return globalVariables
}

func (pi *PostmanImporter) sendRequest(req models.HTTPReq, basePath string) (models.HTTPResp, error) {

	var err error
	if basePath != "" {
		req.URL, err = utils.ReplaceBaseURL(req.URL, basePath)
		if err != nil {
			pi.logger.Error("failed to replace hostname", zap.Error(err))
			return models.HTTPResp{}, err
		}
	}

	httpReq, err := http.NewRequest(string(req.Method), req.URL, strings.NewReader(req.Body))
	if err != nil {
		pi.logger.Error("failed to create HTTP request", zap.Error(err))
		return models.HTTPResp{}, err
	}

	for key, value := range req.Header {
		httpReq.Header.Set(key, value)
	}

	// add timeout to the request
	client := &http.Client{
		Timeout: 60 * time.Second,
	}

	if req.ProtoMajor != 0 || req.ProtoMinor != 0 {
		httpReq.ProtoMajor = req.ProtoMajor
		httpReq.ProtoMinor = req.ProtoMinor
	}

	resp, err := client.Do(httpReq)
	if err != nil {
		pi.logger.Error("failed to send HTTP request", zap.Error(err))
		return models.HTTPResp{}, err
	}

	defer func() {
		if cerr := resp.Body.Close(); cerr != nil {
			pi.logger.Error("failed to close response body", zap.Error(cerr))
		}
	}()

	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		pi.logger.Error("failed to read response body", zap.Error(err))
		return models.HTTPResp{}, err
	}

	response := models.HTTPResp{
		StatusCode:    resp.StatusCode,
		StatusMessage: resp.Status,
		Header:        make(map[string]string),
		Body:          string(responseBody),
	}

	for key, value := range resp.Header {
		response.Header[key] = strings.Join(value, ",")
	}
	// Use the response.Body field to avoid unused write error
	pi.logger.Info("Response Body", zap.String("body", response.Body))

	return response, nil
}

func (pi *PostmanImporter) importTestSets(collection *PostmanCollectionStruct, globalVariables map[string]string, basePath string) error {
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("failed to get current working directory: %w", err)
	}

	testCounter := 0
	var itemsToProcess []TestData

	if len(collection.Items.PostmanItems) > 0 {
		for _, item := range collection.Items.PostmanItems {
			if len(item.Item) == 0 {
				continue
			}

			testSet := item.Name
			if item.Name == "" {
				testSet = pi.generateTestSetName()
			}

			testSetPath := filepath.Join(cwd, "keploy", testSet)
			testsPath := filepath.Join(testSetPath, "tests")

			for _, testItem := range item.Item {
				if err := pi.processEmptyResponse(&testItem, globalVariables, basePath); err != nil {
					return err
				}

				if err := pi.writeTestData(testItem, testsPath, globalVariables, &testCounter); err != nil {
					return fmt.Errorf("failed to write test data: %w", err)
				}
			}

			testCounter = 0
		}
	}

	if len(collection.Items.TestDataItems) > 0 {
		testSetName := pi.generateTestSetName()
		testSetPath := filepath.Join(cwd, "keploy", testSetName)
		testsPath := filepath.Join(testSetPath, "tests")
		itemsToProcess = collection.Items.TestDataItems

		for _, testItem := range itemsToProcess {
			if err := pi.processEmptyResponse(&testItem, globalVariables, basePath); err != nil {
				return err
			}

			if err := pi.writeTestData(testItem, testsPath, globalVariables, &testCounter); err != nil {
				return fmt.Errorf("failed to write test data: %w", err)
			}
		}

		return nil
	}

	return nil
}

func (pi *PostmanImporter) generateTestSetName() string {
	maxTestSetNumber := 0
	files, err := os.ReadDir(filepath.Join("keploy"))
	if err == nil {
		for _, file := range files {
			if file.IsDir() && strings.HasPrefix(file.Name(), testSetNamePrefix) {
				var testSetNumber int
				if _, err := fmt.Sscanf(file.Name(), testSetNamePrefix+"%d", &testSetNumber); err == nil && testSetNumber > maxTestSetNumber {
					maxTestSetNumber = testSetNumber
				}
			}
		}
	}
	return fmt.Sprintf("%s%d", testSetNamePrefix, maxTestSetNumber+1)
}

func (pi *PostmanImporter) scanForEmptyResponses(collection *PostmanCollectionStruct) bool {

	for _, item := range collection.Items.PostmanItems {
		for _, testItem := range item.Item {
			if len(testItem.Response) == 0 {
				pi.logger.Warn("Empty response found", zap.String("testItem", testItem.Name))
				return true
			}
		}
	}
	for _, testItem := range collection.Items.TestDataItems {
		if len(testItem.Response) == 0 {
			pi.logger.Warn("Empty response found", zap.String("testItem", testItem.Name))
			return true
		}
	}
	return false
}

func (pi *PostmanImporter) promptUserForCapture() bool {
	reader := bufio.NewReader(os.Stdin)
	fmt.Print("Some responses are empty. We need to hit the server to record these responses. Is your server running? (yes/no): ")
	response, err := reader.ReadString('\n')
	if err != nil {
		pi.logger.Error("Failed to read user input", zap.Error(err))
		return false
	}
	response = strings.TrimSpace(strings.ToLower(response))
	return response == "yes"
}

func (pi *PostmanImporter) processEmptyResponse(testItem *TestData, globalVariables map[string]string, basePath string) error {
	if len(testItem.Response) != 0 {
		return nil
	}

	if !pi.toCapture {
		pi.logger.Info("Skipping request capture as basePath is not provided")
		return nil
	}

	req := constructRequest(&testItem.Request, globalVariables)
	if req.URL != "" {
		resp, err := pi.sendRequest(req, basePath)
		if err != nil {
			return fmt.Errorf("failed to send request: %w", err)
		}

		var result []map[string]string
		for key, value := range resp.Header {
			result = append(result, map[string]string{
				"key":   key,
				"value": value,
			})
		}

		response := PostmanResponse{
			Name:            "New Request",
			Body:            resp.Body,
			Status:          resp.StatusMessage,
			Code:            resp.StatusCode,
			OriginalRequest: &testItem.Request,
			Header:          result,
		}
		testItem.Response = append(testItem.Response, response)
		return nil
	}
	pi.logger.Error("URL is empty", zap.String("testItem", testItem.Name))
	return errors.New("URL is empty")
}

func (pi *PostmanImporter) writeTestData(testItem TestData, testsPath string, globalVariables map[string]string, testCounter *int) error {
	for _, response := range testItem.Response {
		testName := fmt.Sprintf("test-%d", *testCounter+1)

		requestSchema := constructRequest(response.OriginalRequest, globalVariables)
		if response.OriginalRequest == nil {
			requestSchema = constructRequest(&testItem.Request, globalVariables)
		}

		responseSchema := constructResponse(response)

		testCase := &yaml.NetworkTrafficDoc{
			Version: models.GetVersion(),
			Kind:    models.HTTP,
			Name:    testItem.Name,
		}

		if err := testCase.Spec.Encode(&models.HTTPSchema{
			Request:  requestSchema,
			Response: responseSchema,
		}); err != nil {
			return fmt.Errorf("failed to encode test case: %w", err)
		}

		testCase.Curl = pkg.MakeCurlCommand(requestSchema)

		testResultBytes, err := yamlLib.Marshal(testCase)
		if err != nil {
			return fmt.Errorf("failed to marshal test result: %w", err)
		}

		if err := yaml.WriteFile(pi.ctx, pi.logger, testsPath, testName, testResultBytes, false); err != nil {
			return fmt.Errorf("failed to write test result: %w", err)
		}

		(*testCounter)++
	}
	return nil
}

func constructRequest(req *PostmanRequest, variables map[string]string) models.HTTPReq {
	if req == nil {
		return models.HTTPReq{}
	}

	headers := extractHeaders(req.Header)
	url := extractURL(req.URL)

	requestSchema := models.HTTPReq{
		URL:    replaceTemplateVars(url, variables),
		Method: models.Method(req.Method),
		Header: headers,
	}

	// Process request body based on mode
	switch req.Body.Mode {
	case "raw":
		requestSchema.Body = req.Body.Raw
	case "urlencoded":
		requestSchema.Body = processUrlencodedBody(req.Body.Urlencoded)
	case "formdata":
		requestSchema.Form = processFormdataBody(req.Body.Formdata)
	}

	return requestSchema
}

func extractHeaders(headers []map[string]interface{}) map[string]string {
	headersMap := make(map[string]string)
	for _, header := range headers {
		headersMap[header["key"].(string)] = header["value"].(string)
	}
	return headersMap
}

func extractURL(url interface{}) string {
	switch v := url.(type) {
	case string:
		return v
	case map[string]interface{}:
		url := v["raw"].(string)
		return url
	default:
		return ""
	}
}

func processUrlencodedBody(body []map[string]interface{}) string {
	keyValues := []string{}
	for _, item := range body {
		keyValues = append(keyValues, item["key"].(string)+"="+item["value"].(string))
	}
	return strings.Join(keyValues, "&")
}

func processFormdataBody(body []map[string]interface{}) []models.FormData {
	form := []models.FormData{}
	for _, formData := range body {
		form = append(form, models.FormData{
			Key:    formData["key"].(string),
			Values: []string{formData["value"].(string)},
		})
	}
	return form
}

func constructResponse(res PostmanResponse) models.HTTPResp {
	headers := make(map[string]string)
	for _, header := range res.Header {
		headers[header["key"]] = header["value"]
	}

	return models.HTTPResp{
		Body:          res.Body,
		StatusMessage: res.Status,
		StatusCode:    res.Code,
		Header:        headers,
	}
}

func replaceTemplateVars(input string, variables map[string]string) string {
	re := regexp.MustCompile(`\{\{\s*(\w+)\s*\}\}`)

	return re.ReplaceAllStringFunc(input, func(match string) string {
		submatches := re.FindStringSubmatch(match)
		if len(submatches) > 1 {
			if replacement, exists := variables[submatches[1]]; exists {
				return replacement
			}
		}
		return match
	})
}

type PostmanCollectionStruct struct {
	Info struct {
		PostmanID string `json:"_postman_id"`
		Name      string `json:"name"`
		Schema    string `json:"schema"`
	} `json:"info"`
	Items     ItemsContainer           `json:"item"`
	Variables []map[string]interface{} `json:"variable"`
}

type PostmanCollection struct {
	Info struct {
		PostmanID string `json:"_postman_id"`
		Name      string `json:"name"`
		Schema    string `json:"schema"`
	} `json:"info"`
	Items     json.RawMessage          `json:"item"`
	Variables []map[string]interface{} `json:"variable"`
}

type ItemsContainer struct {
	PostmanItems  []PostmanItem
	TestDataItems []TestData
}

type PostmanItem struct {
	Name      string              `json:"name"`
	Variables []map[string]string `json:"variable"`
	Item      []TestData          `json:"item"`
}

type TestData struct {
	Name      string                   `json:"name"`
	Request   PostmanRequest           `json:"request"`
	Response  []PostmanResponse        `json:"response"`
	Variables []map[string]interface{} `json:"variable"`
}

type PostmanRequest struct {
	Method string                   `json:"method"`
	Header []map[string]interface{} `json:"header"`
	Body   PostmanRequestBody       `json:"body"`
	URL    interface{}              `json:"url"`
}

type PostmanRequestBody struct {
	Mode       string                   `json:"mode"`
	Raw        string                   `json:"raw"`
	Urlencoded []map[string]interface{} `json:"urlencoded"`
	Formdata   []map[string]interface{} `json:"formdata"`
	Options    map[string]interface{}   `json:"options"`
}

type PostmanResponse struct {
	Name            string              `json:"name"`
	Body            string              `json:"body"`
	Status          string              `json:"status"`
	Code            int                 `json:"code"`
	OriginalRequest *PostmanRequest     `json:"originalRequest,omitempty"`
	Header          []map[string]string `json:"header"`
}

func (ic *ItemsContainer) UnmarshalJSON(data []byte) error {
	var postmanItems []PostmanItem
	if err := json.Unmarshal(data, &postmanItems); err == nil {
		ic.PostmanItems = postmanItems
	}

	var items []TestData

	if err := json.Unmarshal(data, &items); err != nil {
		return err
	}

	ic.TestDataItems = items

	return nil
}

func (ic ItemsContainer) MarshalJSON() ([]byte, error) {
	if len(ic.PostmanItems) > 0 {
		return json.Marshal(ic.PostmanItems)
	}
	return json.Marshal(ic.TestDataItems)
}
