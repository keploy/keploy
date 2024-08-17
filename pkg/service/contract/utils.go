package contract

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/fatih/color"
	"github.com/getkin/kin-openapi/openapi3"
	"github.com/olekukonko/tablewriter"
	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/pkg/platform/yaml"
	"go.uber.org/zap"
	yamlLib "gopkg.in/yaml.v3"
)

// GetVariablesType returns the type of each variable in the object.
func GetVariablesType(obj map[string]interface{}) map[string]map[string]interface{} {
	types := make(map[string]map[string]interface{}, 0)
	// Loop over body object and get the type of each value
	for key, value := range obj {
		var valueType string
		switch value.(type) {
		case float64:
			valueType = "number"
		case int, int32, int64:
			valueType = "integer"
		case string:
			valueType = "string"
		case bool:
			valueType = "boolean"
		case map[string]interface{}:
			valueType = "object"
		case []interface{}:
			valueType = "array"
		default:
			valueType = "string"
		}
		responseType := map[string]interface{}{
			"type": valueType,
		}
		// If the value is an object, recursively get its properties
		if valueType == "object" {
			responseType["properties"] = GetVariablesType(value.(map[string]interface{}))
		}
		// If the value is an array, get the type of the first element
		if valueType == "array" {
			arrayType := "string" // Default to string if array is empty or type can't be determined
			if len(value.([]interface{})) > 0 {
				firstElement := value.([]interface{})[0]
				switch firstElement.(type) {
				case float64:
					arrayType = "number"
				case int, int32, int64:
					arrayType = "integer"
				case string:
					arrayType = "string"
				case bool:
					arrayType = "boolean"
				case map[string]interface{}:
					arrayType = "object"
					responseType["items"] = map[string]interface{}{
						"type":       arrayType,
						"properties": GetVariablesType(firstElement.(map[string]interface{})),
					}
					continue
				default:
					arrayType = "string"
				}
			}
			responseType["items"] = map[string]interface{}{
				"type": arrayType,
			}
		}
		types[key] = responseType
	}
	return types
}

func UnmarshalAndConvertToJSON(bodyStr []byte, bodyObj map[string]interface{}) ([]byte, map[string]interface{}, error) {
	err := json.Unmarshal(bodyStr, &bodyObj)
	if err != nil {
		return nil, nil, err
	}
	// Convert the response body object back to a JSON string
	bodyJSON, err := json.Marshal(bodyObj)
	if err != nil {
		return nil, nil, err
	}
	return bodyJSON, bodyObj, nil
}

func GenerateRepsonse(responseCode int, responseMessage string, responseTypes map[string]map[string]interface{}, responseBody map[string]interface{}) map[string]models.ResponseItem {
	byCode := map[string]models.ResponseItem{
		fmt.Sprintf("%d", responseCode): {
			Description: responseMessage,
			Content: map[string]models.MediaType{
				"application/json": {
					Schema: models.Schema{
						Type:       "object",
						Properties: responseTypes,
					},
					Example: (responseBody),
				},
			},
		},
	}
	return byCode
}

func ExtractURLPath(fullURL string) (string, string) {
	parsedURL, err := url.Parse(fullURL)

	if err != nil {
		return "", ""
	}
	return parsedURL.Path, parsedURL.Host
}

func GenerateHeader(header map[string]string) []models.Parameter {
	var parameters []models.Parameter
	for key, value := range header {
		parameters = append(parameters, models.Parameter{
			Name:     key,
			In:       "header",
			Required: true,
			Schema:   models.ParamSchema{Type: "string"},
			Example:  value,
		})
	}
	return parameters
}

func GenerateInPathParams(params map[string]string) []models.Parameter {
	var parameters []models.Parameter
	for key, value := range params {
		parameters = append(parameters, models.Parameter{
			Name:     key,
			In:       "path",
			Required: true,
			Schema:   models.ParamSchema{Type: "string"},
			Example:  value,
		})
	}
	return parameters
}

// isNumeric checks if a string is a valid numeric value (integer or float).
func isNumeric(s string) bool {
	if _, err := strconv.Atoi(s); err == nil {
		return true
	}
	if _, err := strconv.ParseFloat(s, 64); err == nil {
		return true
	}
	return false
}

// ExtractIdentifiersAndCount extracts numeric identifiers (integers or floats) from the path.
func ExtractIdentifiersAndCount(path string) ([]string, int) {
	segments := strings.Split(path, "/")
	segments2 := strings.Split(segments[len(segments)-1], "?")
	segments = append(segments[:len(segments)-1], segments2[0])
	var identifiers []string
	for _, segment := range segments {
		if segment != "" {
			// Check if the segment is numeric (integer or float)
			if isNumeric(segment) {
				identifiers = append(identifiers, segment)
			}
		}
	}

	return identifiers, len(identifiers)
}

// ExtractQueryParams extracts the query parameters and their names from the URL.
func ExtractQueryParams(rawURL string) (map[string]string, error) {
	parsedURL, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	queryParams := parsedURL.Query()
	params := make(map[string]string)
	for key, values := range queryParams {
		if len(values) > 0 {
			// Take the first value if multiple are present
			params[key] = values[0]
		}
	}
	return params, nil
}

// GenerateDummyNamesForIdentifiers generates dummy names for the path identifiers.
func GenerateDummyNamesForIdentifiers(identifiers []string) map[string]string {
	dummyNames := make(map[string]string)
	for i, id := range identifiers {
		dummyName := fmt.Sprintf("param%d", i+1)
		dummyNames[dummyName] = id
	}
	return dummyNames
}
func AppendInParameters(parameters []models.Parameter, inParameters map[string]string, count int, paramType string) []models.Parameter {
	if count == 0 {
		return parameters
	}
	for key, value := range inParameters {
		parameters = append(parameters, models.Parameter{
			Name:     key,
			In:       paramType,
			Required: true,
			Schema:   models.ParamSchema{Type: "string"},
			Example:  value,
		})
	}

	return parameters
}

// ReplacePathIdentifiers replaces numeric identifiers in the path with their corresponding dummy names.
func ReplacePathIdentifiers(path string, dummyNames map[string]string) string {
	segments := strings.Split(path, "/")
	var replacedPath []string
	for _, segment := range segments {
		if segment != "" {
			// Check if the segment is numeric (integer or float)
			if isNumeric(segment) {
				dummyName := ""
				for key, value := range dummyNames {
					if value == segment {
						// i want to put '{' and '}' around the key
						dummyName = "{" + key + "}"
						break
					}
				}
				if dummyName != "" {
					replacedPath = append(replacedPath, dummyName)
				} else {
					replacedPath = append(replacedPath, segment)
				}
			} else {
				replacedPath = append(replacedPath, segment)
			}
		}
	}
	finalPath := strings.Join(replacedPath, "/")
	// Add slash at the beginning of the path
	finalPath = "/" + finalPath
	return finalPath
}

func generateUniqueID() string {
	b := make([]byte, 16)
	_, err := rand.Read(b)
	if err != nil {
		// handle error
		return ""
	}
	return hex.EncodeToString(b) + "-" + time.Now().Format("20060102150405")
}

func readOrParseData(ctx context.Context, logger *zap.Logger, filePath, name string, readData bool, data models.HTTPSchema2) (models.HTTPSchema2, error) {
	var custom models.HTTPSchema2
	if readData {
		data, err := yaml.ReadFile(ctx, logger, filePath, name)
		if err != nil {
			return custom, err
		}
		err = yamlLib.Unmarshal(data, &custom)
		if err != nil {
			return custom, err
		}
	} else {
		custom = data
	}
	return custom, nil
}
func validateOpenAPIDocument(logger *zap.Logger, openapiYAML []byte) error {
	// Validate using kin-openapi
	loader := openapi3.NewLoader()
	doc, err := loader.LoadFromData(openapiYAML)
	if err != nil {
		logger.Fatal("Error loading OpenAPI document: %v", zap.Error(err))
		return nil

	}
	// Validate the OpenAPI document
	if err := doc.Validate(context.Background()); err != nil {
		logger.Fatal("Error validating OpenAPI document: %v", zap.Error(err))
		return err
	}

	fmt.Println("OpenAPI document is valid.")
	return nil
}
func writeOpenAPIToFile(ctx context.Context, logger *zap.Logger, outputPath, name string, openapiYAML []byte, isAppend bool) error {

	_, err := os.Stat(outputPath)
	if os.IsNotExist(err) {
		err = os.MkdirAll(outputPath, os.ModePerm)
		if err != nil {
			logger.Error("Failed to create directory", zap.String("directory", outputPath), zap.Error(err))
			return err
		}
		logger.Info("Directory created", zap.String("directory", outputPath))
	}

	err = yaml.WriteFile(ctx, logger, outputPath, name, openapiYAML, isAppend)
	if err != nil {
		logger.Error("Failed to write OpenAPI YAML to a file", zap.Error(err))
		return err
	}

	outputFilePath := outputPath + "/" + name + ".yaml"
	fmt.Println("OpenAPI YAML has been saved to " + outputFilePath)
	return nil
}

func validateServices(services []string, mappings map[string][]string, genAllMocks bool, logger *zap.Logger) error {
	if !genAllMocks {
		for _, service := range services {
			if _, exists := mappings[service]; !exists {
				logger.Warn("Service not found in services mapping, no contract generation", zap.String("service", service))
			}
		}
	}
	return nil
}
func marshalRequestBodies(mockOperation, testOperation *models.Operation) (string, string, error) {
	var mockRequestBody []byte
	var testRequestBody []byte
	var err error
	if mockOperation.RequestBody != nil {
		mockRequestBody, err = json.Marshal(mockOperation.RequestBody.Content["application/json"].Schema.Properties)
		if err != nil {
			return "", "", fmt.Errorf("error marshalling mock RequestBody: %v", err)
		}
	}
	if testOperation.RequestBody != nil {
		testRequestBody, err = json.Marshal(testOperation.RequestBody.Content["application/json"].Schema.Properties)
		if err != nil {
			return "", "", fmt.Errorf("error marshalling test RequestBody: %v", err)
		}
	}
	return string(mockRequestBody), string(testRequestBody), nil
}

func marshalResponseBodies(status string, mockOperation, testOperation *models.Operation) (string, string, error) {
	var mockResponseBody []byte
	var testResponseBody []byte
	var err error
	if mockOperation.Responses[status].Content != nil {
		mockResponseBody, err = json.Marshal(mockOperation.Responses[status].Content["application/json"].Schema.Properties)
		if err != nil {
			return "", "", fmt.Errorf("error marshalling mock ResponseBody: %v", err)
		}
	}
	if testOperation.Responses[status].Content != nil {
		testResponseBody, err = json.Marshal(testOperation.Responses[status].Content["application/json"].Schema.Properties)
		if err != nil {
			return "", "", fmt.Errorf("error marshalling test ResponseBody: %v", err)
		}
	}
	return string(mockResponseBody), string(testResponseBody), nil
}
func findOperation(item models.PathItem) (*models.Operation, string) {
	if item.Get != nil {
		return item.Get, "GET"
	}
	if item.Post != nil {
		return item.Post, "POST"
	}
	if item.Put != nil {
		return item.Put, "PUT"
	}
	if item.Delete != nil {
		return item.Delete, "DELETE"
	}
	if item.Patch != nil {
		return item.Patch, "PATCH"
	}
	return nil, ""
}

func generateSummaryTable(summary models.Summary) {
	notMatchedColor := color.New(color.FgHiRed).SprintFunc()
	missedColor := color.New(color.FgHiYellow).SprintFunc()
	successColor := color.New(color.FgHiGreen).SprintFunc()
	serviceColor := color.New(color.FgHiBlue).SprintFunc()

	// Create a new tablewriter to format the output as a table
	table := tablewriter.NewWriter(os.Stdout)

	// Set table headers
	table.SetHeader([]string{"Consumer Service", "Consumer Service Test-set", "Mock-name", "Failed", "Passed", "Missed"})
	table.SetAlignment(tablewriter.ALIGN_CENTER)
	// Loop through each service summary to populate the table
	for idx, serviceSummary := range summary.ServicesSummary {
		serviceNamePrinted := false // Track if service name is printed
		failedCount := serviceSummary.FailedCount
		passedCount := serviceSummary.PassedCount
		missedCount := serviceSummary.MissedCount
		table.Append([]string{
			printOnce(serviceColor(serviceSummary.Service), &serviceNamePrinted),
			"",
			"",
			notMatchedColor(failedCount),
			successColor(passedCount),
			missedColor(missedCount),
		})
		for testSet, status := range serviceSummary.TestSets {
			testSetPrinted := false // Track if test-set name is printed
			for _, mock := range status.Failed {
				// Add rows for failed mocks
				table.Append([]string{
					printOnce(serviceSummary.Service, &serviceNamePrinted),
					printOnce(testSet, &testSetPrinted),
					notMatchedColor(mock),
				})
			}

			for _, mock := range status.Missed {
				table.Append([]string{
					printOnce(serviceSummary.Service, &serviceNamePrinted),
					printOnce(testSet, &testSetPrinted),
					missedColor(mock),
				})
			}
		}
		// Add a blank line (or border) after each service
		if idx < len(summary.ServicesSummary)-1 {
			table.Append([]string{"", "", "", "", "", ""}) // Empty row to separate services
		}
	}

	// Render the table to stdout
	table.Render()
}

func printOnce(text string, printed *bool) string {
	if *printed {
		return "" // Return empty string if already printed
	}
	*printed = true
	return text // Return text and set printed flag to true
}

// getTestsSchema retrieves all the tests from the schema folder.
func (s *contractService) getTestsSchema(ctx context.Context, testsFolder string) (map[string]map[string]*models.OpenAPI, error) {
	s.openAPIDB.ChangeTcPath(testsFolder)
	testSetIDs, err := os.ReadDir(testsFolder)
	if err != nil {
		return nil, fmt.Errorf("failed to read tests directory: %w", err)
	}

	testsMapping := make(map[string]map[string]*models.OpenAPI)
	for _, testSetID := range testSetIDs {
		if !testSetID.IsDir() {
			continue
		}

		tests, err := s.openAPIDB.GetTestCasesSchema(ctx, testSetID.Name(), "")
		if err != nil {
			return nil, fmt.Errorf("failed to get test cases for testSetID %s: %w", testSetID.Name(), err)
		}

		testsMapping[testSetID.Name()] = make(map[string]*models.OpenAPI)
		for _, test := range tests {
			testsMapping[testSetID.Name()][test.Info.Title] = test
		}
	}

	return testsMapping, nil
}

// getMockScores retrieves mocks and compares them with test cases, calculating scores.
func (s *contractService) getMockScores(ctx context.Context, downloadMocksFolder string, testsMapping map[string]map[string]*models.OpenAPI) (map[string]map[string]map[string]models.SchemaInfo, error) {
	// Read the contents of the Download Mocks folder to get all service directories.
	entries, err := os.ReadDir(downloadMocksFolder)
	if err != nil {
		// If there's an error reading the directory, return it.
		return nil, fmt.Errorf("failed to read mocks directory: %w", err)
	}

	// Initialize a map to store the scores for each service, mock set, and mock.
	scores := make(map[string]map[string]map[string]models.SchemaInfo)

	// Loop over each entry in the Download Mocks folder.
	for _, entry := range entries {
		// Check if the entry is a directory (indicating a service folder).
		if entry.IsDir() {
			// Define the path to the service folder (e.g., Download/Mocks/service-name).
			serviceFolder := filepath.Join(downloadMocksFolder, entry.Name())

			// Read the contents of the service folder to get mock set IDs (subdirectories).
			mockSetIDs, err := os.ReadDir(serviceFolder)
			if err != nil {
				// If there's an error reading the service folder, return it.
				return nil, fmt.Errorf("failed to read service directory %s: %w", serviceFolder, err)
			}

			// Initialize the service entry in the scores map if it doesn't already exist.
			if scores[entry.Name()] == nil {
				scores[entry.Name()] = make(map[string]map[string]models.SchemaInfo)
			}
			// Loop over each mock set ID in the service folder.
			for _, mockSetID := range mockSetIDs {
				// Ensure the mock set ID is a directory.
				if !mockSetID.IsDir() {
					continue
				}
				// Initialize the mock set entry if it hasn't been initialized yet.
				if scores[entry.Name()][mockSetID.Name()] == nil {
					scores[entry.Name()][mockSetID.Name()] = make(map[string]models.SchemaInfo)
				}
				// Retrieve the mocks for the given mock set ID (e.g., schema files in the folder).
				mocks, err := s.openAPIDB.GetMocksSchemas(ctx, mockSetID.Name(), serviceFolder, "schema")
				if err != nil {
					// If there's an error retrieving mocks, return it.
					return nil, fmt.Errorf("failed to get HTTP mocks for mockSetID %s: %w", mockSetID.Name(), err)
				}

				// Compare the mocks with test cases and calculate scores.
				// The result is stored in the scores map under the respective service and mock set ID.
				s.scoresForMocks(mocks, scores[entry.Name()][mockSetID.Name()], testsMapping, mockSetID.Name())
			}
		}
	}
	// Return the calculated scores.
	return scores, nil
}

// scoresForMocks compares mocks to test cases and assigns scores.
func (s *contractService) scoresForMocks(mocks []*models.OpenAPI, mockSet map[string]models.SchemaInfo, testsMapping map[string]map[string]*models.OpenAPI, mockSetID string) {
	// Ensure mockSet is initialized before assigning
	if mockSet == nil {
		mockSet = make(map[string]models.SchemaInfo)
	}
	// Loop through each mock in the provided list of mocks.
	for _, mock := range mocks {
		// Initialize the mock's score to 0.0 and store the mock's data in the mockSet map.
		// 'mockSet' is a map where the key is the mock title and the value is the SchemaInfo structure containing score and data.
		mockSet[mock.Info.Title] = models.SchemaInfo{
			Score: 0.0,
			Data:  *mock, // Store the mock data here.
		}

		// Loop through each test set (testSetID) in the testsMapping.
		// testsMapping maps test set IDs to test case titles.
		for testSetID, tests := range testsMapping {
			// Loop through each test in the current test set.
			for _, test := range tests {
				// Call 'match2' to compare the mock with the current test.
				// This function returns a candidateScore (how well the mock matches the test) and a pass boolean.
				candidateScore, pass, err := match2(*mock, *test, testSetID, mockSetID, s.logger, IDENTIFYMODE)

				// Handle any errors encountered during the comparison process.
				if err != nil {
					// Log the error and continue with the next iteration, skipping the current comparison.
					s.logger.Error("Error in matching the two models", zap.Error(err))
					continue
				}

				// If the mock passed the comparison and the candidate score is greater than the current score:
				if pass && candidateScore > mockSet[mock.Info.Title].Score {
					// Update the mock's score and store the test case information in the mockSet.
					// This keeps track of the best matching test case for the current mock.
					mockSet[mock.Info.Title] = models.SchemaInfo{
						Service:   "",              // Optional: could store service info if needed.
						TestSetID: testSetID,       // Store the test set ID that provided the highest score.
						Name:      test.Info.Title, // Store the test case name (title).
						Score:     candidateScore,  // Update the score with the highest candidate score.
						Data:      *mock,           // Store the mock data.
					}
				}
			}
		}
	}
}

// ValidateMockAgainstTests compares mock results with test cases and generates a summary report
func (s *contractService) ValidateMockAgainstTests(scores map[string]map[string]map[string]models.SchemaInfo, testsMapping map[string]map[string]*models.OpenAPI) (models.Summary, error) {
	var summary models.Summary

	// Defining color schemes for success, failure, and other statuses
	notMatchedColor := color.New(color.FgHiRed).SprintFunc()
	missedColor := color.New(color.FgHiYellow).SprintFunc()
	successColor := color.New(color.FgHiGreen).SprintFunc()
	serviceColor := color.New(color.FgHiBlue).SprintFunc()

	// Loop through the services in the scores map
	// Each "service" represents a consumer service being validated
	for service, mockSetIDs := range scores {
		// Create a new service summary for each service
		var serviceSummary models.ServiceSummary
		serviceSummary.TestSets = make(map[string]models.Status)
		serviceSummary.Service = service // Store the service name

		// Output the beginning of the validation for the current service
		fmt.Println("==========================================")
		fmt.Print("Starting Validation for Consumer Service: ")
		fmt.Print(serviceColor(service)) // Print service name in blue
		fmt.Println(" ....")
		fmt.Println("==========================================")

		// Iterate over the mockSetIDs for each service (mock set contains multiple mocks)
		for mockSetID, mockTest := range mockSetIDs {
			if _, ok := serviceSummary.TestSets[mockSetID]; !ok {
				// Initialize the Status struct if it doesn't already exist for the mockSetID
				serviceSummary.TestSets[mockSetID] = models.Status{}
			}

			// Iterate over each mock in the mockTest map
			for _, mockInfo := range mockTest {

				// Print validation information only if the score is not zero
				if mockInfo.Score != 0.0 {
					fmt.Print("Validating '")
					fmt.Print(serviceColor(service)) // Print the service name in blue
					fmt.Println(fmt.Sprintf("': (%s)/%s for (%s)/%s", mockSetID, mockInfo.Data.Info.Title, mockInfo.TestSetID, mockInfo.Name))
				}

				// Case 1: If the score is 1.0, the mock passed the validation
				if mockInfo.Score == 1.0 {
					// Retrieve the Status struct for the given mockSetID
					status := serviceSummary.TestSets[mockSetID]

					// Append the passed mock title
					status.Passed = append(status.Passed, mockInfo.Data.Info.Title)

					// Reassign the updated status back to the map
					serviceSummary.TestSets[mockSetID] = status
					serviceSummary.PassedCount++ // Increment the passed count

					// Print a success message in green
					fmt.Print("Contract check ")
					fmt.Print(successColor("passed")) // Print "passed" in green
					fmt.Println(fmt.Sprintf(" for the test '%s' / mock '%s'", mockInfo.Name, mockInfo.Data.Info.Title))
					fmt.Println("--------------------------------------------------------------------\n")

					// Case 2: If the score is between 0 and 1.0, the mock failed the validation
				} else if mockInfo.Score > 0.0 {
					// Retrieve the Status struct for the given mockSetID
					status := serviceSummary.TestSets[mockSetID]

					// Append the failed mock title
					status.Failed = append(status.Failed, mockInfo.Data.Info.Title)

					// Reassign the updated status back to the map
					serviceSummary.TestSets[mockSetID] = status
					serviceSummary.FailedCount++ // Increment the failed count

					// Print a failure message in red
					fmt.Print("Contract check")
					fmt.Print(notMatchedColor(" failed")) // Print "failed" in red
					fmt.Println(fmt.Sprintf(" for the test '%s' / mock '%s' ", mockInfo.Name, mockInfo.Data.Info.Title))
					fmt.Println()

					// Additional information: Print consumer and current service comparison
					fmt.Println(fmt.Sprintf("                                    Current %s   ||   Consumer %s  ", serviceColor(s.config.Contract.Self), serviceColor(service)))

					// Perform comparison between the mock and test case again
					_, _, err := match2(mockInfo.Data, *testsMapping[mockInfo.TestSetID][mockInfo.Name], mockInfo.TestSetID, mockSetID, s.logger, COMPAREMODE)
					if err != nil {
						// If an error occurs during comparison, return it
						s.logger.Error("Error in matching the two models", zap.Error(err))
						return models.Summary{}, err
					}

					// Case 3: If the score is 0.0, there was no matching test case found
				} else if mockInfo.Score == 0.0 {
					// Retrieve the Status struct for the given mockSetID
					status := serviceSummary.TestSets[mockSetID]

					// Append the missed mock title
					status.Missed = append(status.Missed, mockInfo.Data.Info.Title)

					// Reassign the updated status back to the map
					serviceSummary.TestSets[mockSetID] = status
					serviceSummary.MissedCount++ // Increment the missed count

					// Print a "missed" message in yellow
					fmt.Println(missedColor(fmt.Sprintf("No ideal test case found for the (%s)/'%s'", mockSetID, mockInfo.Data.Info.Title)))
					fmt.Println("--------------------------------------------------------------------\n")
				}
			}
		}

		// Append the completed service summary to the overall summary
		summary.ServicesSummary = append(summary.ServicesSummary, serviceSummary)
	}

	// Return the overall summary containing details of all services validated
	return summary, nil
}
