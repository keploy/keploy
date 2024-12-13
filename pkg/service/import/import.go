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
}

type PostmanResponse struct {
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

const Schema = "https://schema.getpostman.com/json/collection/v2.0.0/collection.json"

func Import(ctx context.Context, logger *zap.Logger, path string) error {

	if path == "" {
		return fmt.Errorf("path to Postman collection cannot be empty")
	}

	if !strings.HasSuffix(path, ".json") {
		return fmt.Errorf("invalid file type: expected .json Postman collection")
	}

	postmanCollectionBytes, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("failed to read file: %w", err)
	}

	var postmanCollection struct {
		Info struct {
			PostmanID string `json:"_postman_id"`
			Name      string `json:"name"`
			Schema    string `json:"schema"`
		} `json:"info"`
		Items     ItemsContainer           `json:"item"`
		Variables []map[string]interface{} `json:"variable"`
	}

	if err := json.Unmarshal(postmanCollectionBytes, &postmanCollection); err != nil {
		return fmt.Errorf("failed to unmarshal json: %w", err)
	}

	if postmanCollection.Info.Schema != Schema {
		logger.Warn("Postman Collection schema mismatch: expected v2.0.0", zap.String("schema", postmanCollection.Info.Schema))
	}

	if len(postmanCollection.Items.PostmanItems) > 0 {
		for index, item := range postmanCollection.Items.PostmanItems {
			if item.Name == "" {
				item.Name = fmt.Sprintf("test-set-%d", index+1)
			}

			testSetName := item.Name

			cwd, err := os.Getwd()

			if err != nil {
				return fmt.Errorf("failed to get current working directory: %w", err)
			}

			testSetPath := filepath.Join(cwd, "keploy", testSetName)

			testsPath := filepath.Join(testSetPath, "tests")

			globalVariables := make(map[string]string)

			for _, variable := range postmanCollection.Variables {

				if variable["disabled"] != nil && variable["disabled"].(bool) {
					continue
				}

				variableKey, ok := variable["key"].(string)
				if !ok {
					logger.Error("global variable key is not a string", zap.Any("key", variable["key"]))
					continue
				}

				variableValue, ok := variable["value"].(string)
				if !ok {
					logger.Error("global variable value is not a string", zap.Any("value", variable["value"]))
					continue
				}

				globalVariables[variableKey] = variableValue
			}

			// Reiterating again if global variables values are also dependent on other global variables
			for key, value := range globalVariables {
				globalVariables[key] = ReplaceTemplateVars(value, globalVariables)
			}

			testCounter := 0

			for _, testItem := range item.Item {
				if len(testItem.Response) == 0 {
					continue
				}

				err := WriteTestData(ctx, logger, testItem, testsPath, globalVariables, &testCounter)

				if err != nil {
					return fmt.Errorf("failed to write test data: %w", err)
				}

			}
		}
	}

	if len(postmanCollection.Items.TestDataItems) > 0 {

		maxTestSetNumber := 0
		files, err := os.ReadDir(filepath.Join("keploy"))
		if err == nil {
			for _, file := range files {
				if file.IsDir() && strings.HasPrefix(file.Name(), "test-set-") {
					var testSetNumber int
					_, err := fmt.Sscanf(file.Name(), "test-set-%d", &testSetNumber)
					if err == nil && testSetNumber > maxTestSetNumber {
						maxTestSetNumber = testSetNumber
					}
				}
			}
		}

		cwd, err := os.Getwd()

		if err != nil {
			return fmt.Errorf("failed to get current working directory: %w", err)
		}

		testSetName := fmt.Sprintf("test-set-%d", maxTestSetNumber+1)

		testSetPath := filepath.Join(cwd, "keploy", testSetName)

		testsPath := filepath.Join(testSetPath, "tests")

		globalVariables := make(map[string]string)

		for _, variable := range postmanCollection.Variables {

			if variable["disabled"] != nil && variable["disabled"].(bool) {
				continue
			}

			variableKey, ok := variable["key"].(string)
			if !ok {
				logger.Error("global variable key is not a string", zap.Any("key", variable["key"]))
				continue
			}

			variableValue, ok := variable["value"].(string)
			if !ok {
				logger.Error("global variable value is not a string", zap.Any("value", variable["value"]))
				continue
			}

			globalVariables[variableKey] = variableValue
		}

		// Reiterating again if global variables values are also dependent on other global variables
		for key, value := range globalVariables {
			globalVariables[key] = ReplaceTemplateVars(value, globalVariables)
		}

		testCounter := 0

		for _, testItem := range postmanCollection.Items.TestDataItems {
			if len(testItem.Response) == 0 {
				continue
			}

			err := WriteTestData(ctx, logger, testItem, testsPath, globalVariables, &testCounter)

			if err != nil {
				return fmt.Errorf("failed to write test data: %w", err)
			}

		}
	}

	fmt.Println("✅ Postman Collection Successfully Imported To Keploy Tests 🎉")

	return nil
}

func WriteTestData(ctx context.Context, logger *zap.Logger, testItem TestData, testsPath string, globalVariables map[string]string, testCounter *int) error {
	requestSchema := ConstructRequest(testItem.Request, globalVariables)

	for _, response := range testItem.Response {

		testName := fmt.Sprintf("test-%d", *testCounter+1)

		if response.OriginalRequest != nil {
			requestSchema = ConstructRequest(*response.OriginalRequest, globalVariables)
		}

		responseSchema := ConstructResponse(response)

		testCase := &yaml.NetworkTrafficDoc{
			Version: models.GetVersion(),
			Kind:    models.Kind("HTTP"),
			Name:    testItem.Name,
		}

		err := testCase.Spec.Encode(&models.HTTPSchema{
			Request:  requestSchema,
			Response: responseSchema,
		})

		if err != nil {
			return fmt.Errorf("failed to encode test case: %w", err)
		}

		testCase.Curl = pkg.MakeCurlCommand(requestSchema)

		testResultBytes, err := yamlLib.Marshal(testCase)
		if err != nil {
			return fmt.Errorf("failed to marshal test result: %w", err)
		}

		err = yaml.WriteFile(ctx, logger, testsPath, testName, testResultBytes, false)

		if err != nil {
			return fmt.Errorf("failed to write test result: %w", err)
		}

		(*testCounter)++
	}

	return nil
}

func ConstructRequest(req PostmanRequest, variables map[string]string) models.HTTPReq {
	headers := make(map[string]string)

	for _, header := range req.Header {
		headers[header["key"].(string)] = header["value"].(string)
	}

	url, ok := req.URL.(string)
	if !ok {
		url = req.URL.(map[string]interface{})["raw"].(string)
	}

	requestSchema := models.HTTPReq{
		URL:    ReplaceTemplateVars(url, variables),
		Method: models.Method(req.Method),
		Header: headers,
	}

	if req.Body.Mode == "raw" {
		requestSchema.Body = req.Body.Raw
	} else if req.Body.Mode == "urlencoded" {
		keyValues := []string{}

		for _, body := range req.Body.Urlencoded {
			keyValues = append(keyValues, body["key"].(string)+"="+body["value"].(string))
		}

		requestSchema.Body = strings.Join(keyValues, "&")
	} else if req.Body.Mode == "formdata" {
		form := []models.FormData{}

		for _, formData := range req.Body.Formdata {
			form = append(form, models.FormData{
				Key:    formData["key"].(string),
				Values: []string{formData["value"].(string)},
			})
		}

		requestSchema.Form = form
	}

	return requestSchema
}

func ConstructResponse(res PostmanResponse) models.HTTPResp {
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

func ReplaceTemplateVars(input string, variables map[string]string) string {
	// Compile the regex to find words inside {{ }}
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
