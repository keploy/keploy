// Package postmanimport implements the import of a Postman collection to Keploy tests.
package postmanimport

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"go.keploy.io/server/v2/pkg"
	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/pkg/platform/yaml"
	"go.uber.org/zap"
	yamlLib "gopkg.in/yaml.v3"
)

const (
	postmanSchemaVersion = "https://schema.getpostman.com/json/collection/v2.0.0/collection.json"
	testSetNamePrefix    = "test-set-"
)

type PostmanImporter struct {
	logger *zap.Logger
	ctx    context.Context
}

func NewPostmanImporter(ctx context.Context, logger *zap.Logger) *PostmanImporter {
	return &PostmanImporter{
		logger: logger,
		ctx:    ctx,
	}
}

func (pi *PostmanImporter) Import(collectionPath string) error {
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

	if err := pi.importTestSets(postmanCollection, globalVariables); err != nil {
		return err
	}

	pi.logger.Info("✅ Postman Collection Successfully Imported To Keploy Tests 🎉")
	return nil
}

func (pi *PostmanImporter) validateCollectionPath(path string) error {
	if path == "" {
		return fmt.Errorf("path to Postman collection cannot be empty")
	}

	if !strings.HasSuffix(path, ".json") {
		return fmt.Errorf("invalid file type: expected .json Postman collection")
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

func (pi *PostmanImporter) importTestSets(collection *PostmanCollectionStruct, globalVariables map[string]string) error {
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
				if len(testItem.Response) == 0 {
					continue
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
			if len(testItem.Response) == 0 {
				continue
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
			Kind:    models.Kind("Http"),
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
