// Package contract provides the implementation of the contract service
package contract

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/getkin/kin-openapi/openapi3"
	"go.keploy.io/server/v2/config"
	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/pkg/platform/yaml"

	"go.uber.org/zap"
	yamlLib "gopkg.in/yaml.v3"
)

// contractService implements the Service interface
type contractService struct {
	logger *zap.Logger
	testDB TestDB
	mockDB MockDB
	config *config.Config
}

func New(logger *zap.Logger, testDB TestDB, mockDB MockDB, config *config.Config) Service {
	return &contractService{
		logger: logger,
		testDB: testDB,
		mockDB: mockDB,
		config: config,
	}
}

func (s *contractService) ConvertHTTPToOpenAPI(ctx context.Context, logger *zap.Logger, filePath string, name string, outputPath string, readData bool, data models.HTTPSchema2, isAppend bool) (success bool) {

	var custom models.HTTPSchema2
	if readData {
		data, err := yaml.ReadFile(ctx, logger, filePath, name)
		if err != nil {
			logger.Fatal("Error reading file", zap.Error(err))
			return false
		}

		// Parse the custom format YAML into the HTTPSchema struct
		err = yamlLib.Unmarshal(data, &custom)
		if err != nil {
			logger.Error("Error parsing YAML: %v", zap.Error(err))
			return false
		}
	} else {
		custom = data
	}
	var err error
	// Convert response body to an object
	var responseBodyObject map[string]interface{}
	if custom.Spec.Response.Body != "" {
		_, responseBodyObject, err = UnmarshalAndConvertToJSON([]byte(custom.Spec.Response.Body), responseBodyObject)
		if err != nil {
			logger.Error("Error converting response body object to JSON string", zap.Error(err))
			return false
		}
	}

	// Get the type of each value in the response body object
	responseTypes := GetVariablesType(responseBodyObject)

	// Convert request body to an object
	var requestBodyObject map[string]interface{}
	if custom.Spec.Request.Body != "" {
		_, requestBodyObject, err = UnmarshalAndConvertToJSON([]byte(custom.Spec.Request.Body), requestBodyObject)
		if err != nil {
			logger.Error("Error converting response body object to JSON string", zap.Error(err))
			return false
		}
	}

	// Get the type of each value in the request body object
	requestTypes := GetVariablesType(requestBodyObject)

	// Generate response by status code
	byCode := GenerateRepsonse(custom.Spec.Response.StatusCode, custom.Spec.Response.StatusMessage, responseTypes, responseBodyObject)

	// Add parameters to the request
	parameters := GenerateHeader(custom.Spec.Request.Header)

	// Extract In Path parameters
	identifiers, count := ExtractIdentifiersAndCount(custom.Spec.Request.URL)
	// Generate Dummy Names for the identifiers
	dummyNames := GenerateDummyNamesForIdentifiers(identifiers)
	// Add In Path parameters to the parameters
	parameters = AppendInParameters(parameters, dummyNames, count, "path")
	// Extract Query parameters
	queryParams, err := ExtractQueryParams(custom.Spec.Request.URL)
	if err != nil {
		logger.Error("Error extracting query parameters", zap.Error(err))
		return false
	}
	// Add Query parameters to the parameters
	parameters = AppendInParameters(parameters, queryParams, len(queryParams), "query")
	// Generate Operation ID
	operationID := generateUniqueID()
	// Determine if the request method is GET or POST
	var pathItem models.PathItem
	switch custom.Spec.Request.Method {
	case "GET":
		pathItem = models.PathItem{
			Get: &models.Operation{
				Summary:     "Auto-generated operation",
				Description: "Auto-generated from custom format",
				OperationID: operationID,
				Parameters:  parameters,
				Responses:   byCode,
			},
		}
	case "POST":
		pathItem = models.PathItem{
			Post: &models.Operation{
				Summary:     "Auto-generated operation",
				Description: "Auto-generated from custom format",
				Parameters:  parameters,
				OperationID: operationID,
				RequestBody: &models.RequestBody{
					Content: map[string]models.MediaType{
						"application/json": {
							Schema: models.Schema{
								Type:       "object",
								Properties: requestTypes,
							},
							Example: requestBodyObject,
						},
					},
				},
				Responses: byCode,
			},
		}
	case "PUT":
		pathItem.Put = &models.Operation{
			Summary:     "Update an employee by ID",
			Description: "Update an employee by ID",
			Parameters:  parameters,
			OperationID: operationID,
			RequestBody: &models.RequestBody{
				Content: map[string]models.MediaType{
					"application/json": {
						Schema: models.Schema{
							Type:       "object",
							Properties: requestTypes,
						},
						Example: (requestBodyObject),
					},
				},
			},
			Responses: byCode,
		}
	case "PATCH":
		pathItem.Patch = &models.Operation{
			Summary:     "Auto-generated operation",
			Description: "Auto-generated from custom format",
			Parameters:  parameters,
			OperationID: operationID,
			RequestBody: &models.RequestBody{
				Content: map[string]models.MediaType{
					"application/json": {
						Schema: models.Schema{
							Type:       "object",
							Properties: requestTypes,
						},
						Example: (requestBodyObject),
					},
				},
			},
			Responses: byCode,
		}
	case "DELETE":
		pathItem.Delete = &models.Operation{
			Summary:     "Delete an employee by ID",
			Description: "Delete an employee by ID",
			OperationID: operationID,
			Parameters:  parameters,
			Responses:   byCode,
		}
	default:
		logger.Fatal("Unsupported method")
		return false
	}

	// Extract the URL path
	parsedURL, hostName := ExtractURLPath(custom.Spec.Request.URL)
	if parsedURL == "" {
		logger.Error("Error extracting URL path")
		return false
	}
	// Replace numeric identifiers in the path with dummy names (if exists)
	parsedURL = ReplacePathIdentifiers(parsedURL, dummyNames)
	//If it's mock so there is no hostname so put it temp
	if hostName == "" {
		hostName = "temp"
	}
	// Convert to OpenAPI format
	openapi := models.OpenAPI{
		OpenAPI: "3.0.0",
		Info: models.Info{
			Title:       custom.Name,
			Version:     custom.Version,
			Description: custom.Kind,
		},
		Servers: []map[string]string{
			{
				"url": hostName,
			},
		},

		Paths: map[string]models.PathItem{
			parsedURL: pathItem,
		},
		Components: map[string]interface{}{},
	}

	// Output OpenAPI format as YAML
	openapiYAML, err := yamlLib.Marshal(openapi)
	if err != nil {
		return false
	}
	// Validate using kin-openapi
	loader := openapi3.NewLoader()
	doc, err := loader.LoadFromData(openapiYAML)
	if err != nil {
		logger.Fatal("Error loading OpenAPI document: %v", zap.Error(err))
		return false

	}

	// Validate the OpenAPI document
	if err := doc.Validate(context.Background()); err != nil {
		logger.Fatal("Error validating OpenAPI document: %v", zap.Error(err))
	}

	fmt.Println("OpenAPI document is valid.")
	_, err = os.Stat(outputPath)
	if os.IsNotExist(err) {
		// Create the directory if it doesn't exist
		err = os.MkdirAll(outputPath, os.ModePerm)
		if err != nil {
			logger.Error("Failed to create directory", zap.String("directory", outputPath), zap.Error(err))
			return false
		}
		logger.Info("Directory created", zap.String("directory", outputPath))
	}

	err = yaml.WriteFile(ctx, logger, outputPath, name, openapiYAML, isAppend)
	if err != nil {
		logger.Error("Failed to write OpenAPI YAML to a file", zap.Error(err))
		return false
	}

	outputFilePath := outputPath + "/" + name + ".yaml"
	fmt.Println("OpenAPI YAML has been saved to " + outputFilePath)
	return true
}

func (s *contractService) GenerateMocksSchemas(ctx context.Context, services []string, mappings map[string][]string, genAllMocks bool) error {
	keployFolder := "./keploy/"
	entries, err := os.ReadDir(keployFolder)
	if err != nil {
		s.logger.Error("Failed to read directory", zap.String("directory", keployFolder), zap.Error(err))
		return err
	}
	if !genAllMocks {
		for _, service := range services {
			if _, exists := mappings[service]; !exists {
				s.logger.Warn("Service not found in services mapping, no contract generation", zap.String("service", service))
				continue
			}
		}
	}
	for _, entry := range entries {
		if entry.IsDir() && strings.Contains(entry.Name(), "test") {
			testSetID := entry.Name()
			httpMocks, err := s.mockDB.GetHTTPMocks(ctx, testSetID, keployFolder)
			if err != nil {
				s.logger.Error("Failed to get HTTP mocks", zap.String("testSetID", testSetID), zap.Error(err))
				return err
			}

			var duplicateMocks []string
			for _, mock := range httpMocks {
				var isAppend bool
				for service, serviceMappings := range mappings {
					if !yaml.Contains(services, service) && !genAllMocks {
						continue
					}
					var mappingFound bool
					for _, mapping := range serviceMappings {
						if mapping == mock.Spec.Request.URL {
							var mockCode string
							if mock.Spec.Request.URLParams != nil {
								mockCode = fmt.Sprintf("%v", mock.Spec.Request.Method) + "-" + fmt.Sprintf("%v", mock.Spec.Request.URL) + "-0"
							} else {
								mockCode = fmt.Sprintf("%v", mock.Spec.Request.Method) + "-" + fmt.Sprintf("%v", mock.Spec.Request.URL) + "-1"
							}
							if yaml.Contains(duplicateMocks, mockCode) {
								isAppend = true
							} else {
								duplicateMocks = append(duplicateMocks, mockCode)
							}

							mappingFound = true
							done := s.ConvertHTTPToOpenAPI(ctx, s.logger, keployFolder+entry.Name(), "mocks", keployFolder+"schema/mocks/"+service+"/"+entry.Name(), false, *mock, isAppend)
							if !done {
								s.logger.Error("Failed to convert the yaml file to openapi")
								return fmt.Errorf("failed to convert the yaml file to openapi")
							}
							break
						}
					}
					if mappingFound {
						break
					}
				}
			}
		}
	}

	return nil
}
func (s *contractService) GenerateTestsSchemas(ctx context.Context, selectedTests []string, genAllTests bool) error {
	keployFolder := "./keploy/"
	s.testDB.ChangeTcPath()
	entries, err := os.ReadDir(keployFolder)
	if err != nil {
		s.logger.Error("Failed to read directory", zap.String("directory", keployFolder), zap.Error(err))
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() && strings.Contains(entry.Name(), "test") {
			testSetID := entry.Name()
			if !yaml.Contains(selectedTests, testSetID) && !genAllTests {
				continue
			}

			testCases, err := s.testDB.GetTestCases(ctx, testSetID)
			if err != nil {
				s.logger.Error("Failed to get test cases", zap.String("testSetID", testSetID), zap.Error(err))
				return err
			}
			for _, tc := range testCases {
				var httpSpec models.HTTPSchema2
				httpSpec.Kind = string(tc.Kind)
				httpSpec.Name = tc.Name
				httpSpec.Spec.Request = tc.HTTPReq
				httpSpec.Spec.Response = tc.HTTPResp
				httpSpec.Version = string(tc.Version)

				done := s.ConvertHTTPToOpenAPI(ctx, s.logger, keployFolder+entry.Name()+"/tests", tc.Name, keployFolder+"schema/tests/"+entry.Name(), false, httpSpec, false)
				if !done {
					s.logger.Error("Failed to convert the yaml file to openapi")
					return fmt.Errorf("failed to convert the yaml file to openapi")
				}
			}
		}
	}
	return nil
}

func (s *contractService) Generate(ctx context.Context, genAllTests bool, genAllMocks bool) error {
	fmt.Println("HELLO IN CONTRACT SERVICE")
	if s.CheckConfigFile() != nil {
		s.logger.Error("Error in checking config file while generating")
		return fmt.Errorf("Error in checking config file while generating")
	}
	var config config.Config
	configData, err := yaml.ReadFile(ctx, s.logger, "./", "keploy")
	if err != nil {
		s.logger.Fatal("Error reading file", zap.Error(err))
		return err
	}
	err = yamlLib.Unmarshal(configData, &config)
	if err != nil {
		s.logger.Error("Error parsing YAML", zap.Error(err))
		return err
	}
	mappings := config.Contract.ServicesMapping

	err = s.GenerateTestsSchemas(ctx, s.config.Contract.Tests, genAllTests)
	if err != nil {
		return err
	}
	err = s.GenerateMocksSchemas(ctx, s.config.Contract.Services, mappings, genAllMocks)
	if err != nil {
		return err
	}

	return nil
}

func (s *contractService) DownloadTests(path string) error {
	keployFolder := "./keploy/"
	targetPath := path + "/keploy/tests/schema/"
	// Ensure destination directory exists
	if _, err := os.Stat(targetPath); os.IsNotExist(err) {
		err := os.MkdirAll(targetPath, os.ModePerm)
		if err != nil {
			s.logger.Error("Error creating destination directory:")
			return err
		}
	}
	// Move OpenAPI schemas for tests
	entries, err := os.ReadDir(keployFolder + "schema/tests/")
	if err != nil {
		s.logger.Error("Failed to read directory", zap.String("directory", keployFolder), zap.Error(err))
		return err
	}

	for _, entry := range entries {
		err := yaml.CopyDir(keployFolder+"schema/tests/"+entry.Name(), targetPath+entry.Name(), s.logger)
		if err != nil {
			fmt.Println("Error moving directory:", err)
			return err
		}
		s.logger.Info("Test's contracts downloaded", zap.String("tests", entry.Name()))

	}
	//Move the Keploy version tests
	// Ensure destination directory exists
	targetPath = path + "/keploy/tests/keployVersion/"
	if _, err := os.Stat(targetPath); os.IsNotExist(err) {
		err := os.MkdirAll(path, os.ModePerm)
		if err != nil {
			s.logger.Error("Error creating destination directory:")
			return err
		}
	}
	entries, err = os.ReadDir(keployFolder)
	if err != nil {
		s.logger.Error("Failed to read directory", zap.String("directory", keployFolder), zap.Error(err))
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() && strings.Contains(entry.Name(), "test") {
			// Move that directory to path
			err := yaml.CopyDir(keployFolder+entry.Name()+"/tests", targetPath+entry.Name(), s.logger)
			if err != nil {
				fmt.Println("Error moving directory:", err)
				return err
			}
			s.logger.Info("Keploy's Tests downloaded", zap.String("tests", entry.Name()))

		}
	}
	return nil
}
func (s *contractService) DownloadMocks(path string) error {
	keployFolder := "./keploy/"
	targetPath := path + "/keploy/mocks/schema/"
	// Ensure destination directory exists
	if _, err := os.Stat(targetPath); os.IsNotExist(err) {
		err := os.MkdirAll(targetPath, os.ModePerm)
		if err != nil {
			s.logger.Error("Error creating destination directory:")
			return err
		}
	}
	// Move OpenAPI schemas for mocks
	entries, err := os.ReadDir(keployFolder + "schema/mocks/")
	if err != nil {
		s.logger.Error("Failed to read directory", zap.String("directory", keployFolder), zap.Error(err))
		return err
	}
	for _, entry := range entries {
		// Move that directory to path
		err := yaml.CopyDir(keployFolder+"schema/mocks/"+entry.Name(), targetPath+entry.Name(), s.logger)
		if err != nil {
			fmt.Println("Error moving directory:", err)
			return err
		}
		s.logger.Info("Service's mocks contracts downloaded", zap.String("service", entry.Name()))

	}
	//Move the Keploy version mocks
	// Ensure destination directory exists
	targetPath = path + "/keploy/mocks/keployVersion/"
	if _, err := os.Stat(targetPath); os.IsNotExist(err) {
		err := os.MkdirAll(path, os.ModePerm)
		if err != nil {
			s.logger.Error("Error creating destination directory:")
			return err
		}
	}
	entries, err = os.ReadDir(keployFolder)
	if err != nil {
		s.logger.Error("Failed to read directory", zap.String("directory", keployFolder), zap.Error(err))
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() && strings.Contains(entry.Name(), "test") {
			// Move that directory to path
			err := os.MkdirAll(targetPath+entry.Name(), os.ModePerm)
			if err != nil {
				s.logger.Error("Error creating destination directory:")
				return err
			}
			err = yaml.CopyFile(keployFolder+entry.Name()+"/mocks.yaml", targetPath+entry.Name()+"/mocks.yaml", s.logger)
			if err != nil {
				fmt.Println("Error moving directory:", err)
				return err
			}
			s.logger.Info("Keploy's Mock downloaded", zap.String("mock", entry.Name()))

		}
	}
	return nil

}
func (s *contractService) Download(_ context.Context, driven string) error {

	if s.CheckConfigFile() != nil {
		s.logger.Error("Error in checking config file while downloading")
		return fmt.Errorf("Error in checking config file while downloading")
	}
	path := s.config.Contract.Path
	// Validate the path
	path, err := yaml.ValidatePath(path)
	if err != nil {
		s.logger.Error("Error in validating path", zap.Error(err))
		return fmt.Errorf("Error in validating path")
	}

	if driven == "provider" || driven == "server" {
		err = s.DownloadTests(path)

	} else if driven == "consumer" || driven == "client" {
		err = s.DownloadMocks(path)
	}
	if err != nil {
		return err
	}
	return nil
}
func (s *contractService) Validate(ctx context.Context) error {
	if s.CheckConfigFile() != nil {
		s.logger.Error("Error in checking config file while validating")
		return fmt.Errorf("Error in checking config file while validating")
	}
	fmt.Printf("Validate contract for services: %v\n", s.config.Contract.Services)
	fmt.Printf("ctx: %v\n", ctx)

	return nil
}
func (s *contractService) CheckConfigFile() error {
	servicesMapping := s.config.Contract.ServicesMapping
	fmt.Println("Services Mapping is : ", servicesMapping)
	// Check if the size of servicesMapping is less than 1
	if len(servicesMapping) < 1 {
		s.logger.Error("services mapping must contain at least 1 services")
		return fmt.Errorf("services mapping must contain at least 1 services")
	}
	return nil
}
