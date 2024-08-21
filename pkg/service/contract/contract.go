// Package contract provides the implementation of the contract service
package contract

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/fatih/color"
	"go.keploy.io/server/v2/config"
	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/pkg/platform/yaml"
	"go.keploy.io/server/v2/pkg/service/contract/consumer"
	"go.keploy.io/server/v2/pkg/service/contract/provider"
	"go.keploy.io/server/v2/utils"

	"go.uber.org/zap"
	yamlLib "gopkg.in/yaml.v3"
)

// contractService implements the Service interface
type contract struct {
	logger    *zap.Logger
	testDB    TestDB
	mockDB    MockDB
	openAPIDB OpenAPIDB
	config    *config.Config
	consumer  consumer.Service
	provider  provider.Service
}

func New(logger *zap.Logger, testDB TestDB, mockDB MockDB, openAPIDB OpenAPIDB, config *config.Config) Service {
	return &contract{
		logger:    logger,
		testDB:    testDB,
		mockDB:    mockDB,
		openAPIDB: openAPIDB,
		config:    config,
		consumer:  consumer.New(logger, config),
		provider:  provider.New(logger, config),
	}
}

func (s *contract) HTTPDocToOpenAPI(logger *zap.Logger, data models.HTTPDoc) (models.OpenAPI, error) {
	custom := data

	var err error
	// Convert response body to an object
	var responseBodyObject map[string]interface{}
	if custom.Spec.Response.Body != "" {
		err := json.Unmarshal([]byte(custom.Spec.Response.Body), &responseBodyObject)
		if err != nil {
			return models.OpenAPI{}, err
		}

	}

	// Get the type of each value in the response body object
	responseTypes := ExtractVariableTypes(responseBodyObject)

	// Convert request body to an object
	var requestBodyObject map[string]interface{}
	if custom.Spec.Request.Body != "" {
		err := json.Unmarshal([]byte(custom.Spec.Request.Body), &requestBodyObject)
		if err != nil {
			return models.OpenAPI{}, err
		}

	}

	// Get the type of each value in the request body object
	requestTypes := ExtractVariableTypes(requestBodyObject)

	// Generate response by status code
	byCode := GenerateResponse(Response{
		Code:    custom.Spec.Response.StatusCode,
		Message: custom.Spec.Response.StatusMessage,
		Types:   responseTypes,
		Body:    responseBodyObject,
	})

	// Add parameters to the request
	parameters := GenerateHeader(custom.Spec.Request.Header)

	// Extract In Path parameters
	identifiers := ExtractIdentifiers(custom.Spec.Request.URL)
	// Generate Dummy Names for the identifiers
	dummyNames := GenerateDummyNamesForIdentifiers(identifiers)
	// Add In Path parameters to the parameters
	if len(identifiers) > 0 {
		parameters = AppendInParameters(parameters, dummyNames, "path")
	}
	// Extract Query parameters
	queryParams, err := ExtractQueryParams(custom.Spec.Request.URL)
	if err != nil {
		utils.LogError(logger, err, "failed to extract query parameters")
		return models.OpenAPI{}, err
	}
	// Add Query parameters to the parameters
	if len(queryParams) > 0 {
		parameters = AppendInParameters(parameters, queryParams, "query")
	}
	// Generate Operation ID
	operationID := generateUniqueID()
	if operationID == "" {
		err := fmt.Errorf("failed to generate unique ID")
		utils.LogError(logger, err, "failed to generate unique ID")
		return models.OpenAPI{}, err
	}
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
		utils.LogError(logger, err, "Unsupported Method")
		return models.OpenAPI{}, err
	}

	// Extract the URL path
	parsedURL, hostName := ExtractURLPath(custom.Spec.Request.URL)
	if parsedURL == "" {
		logger.Error("Error extracting URL path")
		return models.OpenAPI{}, err
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

	return openapi, nil
}

func (s *contract) GenerateMocksSchemas(ctx context.Context, services []string, mappings map[string][]string) error {
	keployFolder := "./keploy/"
	entries, err := os.ReadDir(keployFolder)
	if err != nil {
		utils.LogError(s.logger, err, "failed to read directory", zap.String("directory", keployFolder))
		return err
	}
	// Checking if the services provided by the user are in the services mapping
	if len(services) != 0 {
		for _, service := range services {
			if _, exists := mappings[service]; !exists {
				s.logger.Warn("Service not found in services mapping, no contract generation", zap.String("service", service))
			}
		}
	}
	for _, entry := range entries {
		if entry.IsDir() && strings.Contains(entry.Name(), "test") {
			testSetID := entry.Name()
			httpMocks, err := s.mockDB.GetHTTPMocks(ctx, testSetID, keployFolder, "mocks")
			if err != nil {
				utils.LogError(s.logger, err, "failed to get HTTP mocks", zap.String("testSetID", testSetID))
				return err
			}

			var duplicateMocks []string
			for _, mock := range httpMocks {
				var isAppend bool
				for service, serviceMappings := range mappings {
					if !yaml.Contains(services, service) && len(services) != 0 {
						continue
					}
					var mappingFound bool
					for _, mapping := range serviceMappings {
						if mapping == mock.Spec.Request.URL {
							var mockCode = service

							// if mock.Spec.Request.URLParams != nil {
							// 	mockCode = fmt.Sprintf("%v", mock.Spec.Request.Method) + "-" + fmt.Sprintf("%v", mock.Spec.Request.URL) + "-0"
							// } else {
							// 	mockCode = fmt.Sprintf("%v", mock.Spec.Request.Method) + "-" + fmt.Sprintf("%v", mock.Spec.Request.URL) + "-1"
							// }
							if yaml.Contains(duplicateMocks, mockCode) {
								isAppend = true
							} else {
								duplicateMocks = append(duplicateMocks, mockCode)
							}

							mappingFound = true
							openapi, err := s.HTTPDocToOpenAPI(s.logger, *mock)
							if err != nil {
								utils.LogError(s.logger, err, "failed to convert the yaml file to openapi")
								return fmt.Errorf("failed to convert the yaml file to openapi")
							}
							// Validate the OpenAPI document
							err = validateSchema(openapi)
							if err != nil {
								return err
							}
							// Save it using the OpenAPIDB
							err = s.openAPIDB.WriteOpenAPIToFile(ctx, s.logger, filepath.Join(keployFolder, "schema", "mocks", service, entry.Name()), "mocks", openapi, isAppend)
							if err != nil {
								return err
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
func (s *contract) GenerateTestsSchemas(ctx context.Context, selectedTests []string) error {
	keployFolder := "./keploy/"
	testSetsIDs, err := s.testDB.GetAllTestSetIDs(ctx)
	if err != nil {
		utils.LogError(s.logger, err, "failed to get test set IDs")
		return err
	}

	for _, entry := range testSetsIDs {
		testSetID := entry
		if !yaml.Contains(selectedTests, testSetID) && len(selectedTests) != 0 {
			continue
		}

		testCases, err := s.testDB.GetTestCases(ctx, testSetID)
		if err != nil {
			utils.LogError(s.logger, err, "failed to get test cases", zap.String("testSetID", testSetID))
			return err
		}
		for _, tc := range testCases {
			var httpSpec models.HTTPDoc
			httpSpec.Kind = string(tc.Kind)
			httpSpec.Name = tc.Name
			httpSpec.Spec.Request = tc.HTTPReq
			httpSpec.Spec.Response = tc.HTTPResp
			httpSpec.Version = string(tc.Version)

			openapi, err := s.HTTPDocToOpenAPI(s.logger, httpSpec)
			if err != nil {
				s.logger.Error("Failed to convert the yaml file to openapi")
				return fmt.Errorf("failed to convert the yaml file to openapi")
			}
			// Validate the OpenAPI document
			err = validateSchema(openapi)
			if err != nil {
				return err
			}
			// Save it using the OpenAPIDB
			err = s.openAPIDB.WriteOpenAPIToFile(ctx, s.logger, filepath.Join(keployFolder, "schema", "tests", entry), tc.Name, openapi, false)
			if err != nil {
				return err
			}

		}

	}
	return nil
}

func (s *contract) Generate(ctx context.Context) error {
	if checkConfigFile(s.config.Contract.ServicesMapping) != nil {
		utils.LogError(s.logger, fmt.Errorf("Error in checking config file while validating"), "Error in checking config file while validating")
		return fmt.Errorf("Error in checking config file while validating")
	}

	var config config.Config
	err := yaml.ReadYAMLFile(ctx, s.logger, "./", "keploy", &config, false)
	// configData, err := yaml.ReadFile(ctx, s.logger, "./", "keploy")
	if err != nil {
		return err
	}

	mappings := config.Contract.ServicesMapping
	serviceColor := color.New(color.FgYellow).SprintFunc()
	fmt.Println(serviceColor("=========================================="))
	fmt.Println(serviceColor(fmt.Sprintf("Starting Generating OpenAPI Schemas for Current Service: %s ....", s.config.Contract.Self)))
	fmt.Println(serviceColor("=========================================="))

	err = s.GenerateTestsSchemas(ctx, s.config.Contract.Tests)
	if err != nil {
		return err
	}
	err = s.GenerateMocksSchemas(ctx, s.config.Contract.Services, mappings)
	if err != nil {
		return err
	}
	if err := saveServiceMappings(config, "./keploy/schema"); err != nil {
		return err
	}

	return nil
}

func (s *contract) DownloadTests(ctx context.Context, path string) error {
	fmt.Println("Path given (not simulated): ", path)

	targetPath := "./Download/Tests"
	if err := yaml.CreateDir(targetPath, s.logger); err != nil {
		return err
	}

	cprFolder, err := filepath.Abs("../VirtualCPR")
	if err != nil {
		return err
	}

	var schemaConfigFile config.Config

	configFilePath := "./"
	if err := yaml.ReadYAMLFile(ctx, s.logger, configFilePath, "keploy", &schemaConfigFile, false); err != nil {
		return err
	}
	// Loop through the services in the mappings in the config file
	for service := range schemaConfigFile.Contract.ServicesMapping {
		// Fetch the tests of those services from virtual cpr
		testsPath := filepath.Join(cprFolder, service, "keploy", "schema", "tests")
		// Copy this dir to the target path
		serviceFolder := filepath.Join(targetPath, service)
		if err := yaml.CopyDir(testsPath, serviceFolder, false, s.logger); err != nil {
			utils.LogError(s.logger, err, "failed to copy directory", zap.String("directory", testsPath))
			return err
		}
		s.logger.Info("Service's tests contracts downloaded", zap.String("service", service))
		// Copy the Keploy version (HTTP) tests
		keployTestsPath := filepath.Join(cprFolder, service, "keploy")
		testEntries, err := os.ReadDir(keployTestsPath)
		if err != nil {
			utils.LogError(s.logger, err, "failed to read directory", zap.String("directory", keployTestsPath))
			return err
		}
		for _, testSetID := range testEntries {
			if !testSetID.IsDir() || !strings.Contains(testSetID.Name(), "test") {
				continue
			}
			// Copy the directory to the target path
			if err := yaml.CopyDir(filepath.Join(keployTestsPath, testSetID.Name(), "tests"), filepath.Join(serviceFolder, "schema", testSetID.Name()), false, s.logger); err != nil {
				fmt.Println("Error copying directory:", err)
				return err
			}
			s.logger.Info("Service's HTTP tests contracts downloaded", zap.String("service", service), zap.String("tests", testSetID.Name()))

		}

	}
	return nil
}
func (s *contract) DownloadMocks(ctx context.Context, path string) error {
	fmt.Println("Path given (not simulated): ", path)
	targetPath := "./Download/Mocks"
	if err := yaml.CreateDir(targetPath, s.logger); err != nil {
		return err
	}

	cprFolder, err := filepath.Abs("../VirtualCPR")
	if err != nil {
		return err
	}
	entries, err := os.ReadDir(cprFolder)
	if err != nil {
		utils.LogError(s.logger, err, "failed to read directory", zap.String("directory", cprFolder))
		return err
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		var self = s.config.Contract.Self
		var schemaConfigFile config.Config

		configFilePath := filepath.Join(cprFolder, entry.Name(), "keploy", "schema")
		if err := yaml.ReadYAMLFile(ctx, s.logger, configFilePath, "keploy", &schemaConfigFile, true); err != nil {
			return err
		}

		serviceFound := false
		if _, exists := schemaConfigFile.Contract.ServicesMapping[self]; exists {
			serviceFound = true
		}

		if !serviceFound {
			continue
		}

		serviceFolder := filepath.Join(targetPath, schemaConfigFile.Contract.Self)
		if err := yaml.CreateDir(serviceFolder, s.logger); err != nil {
			return err
		}

		mocksSourcePath := filepath.Join(cprFolder, entry.Name(), "keploy", "schema", "mocks", self)
		serviceColor := color.New(color.FgYellow).SprintFunc()
		fmt.Println(serviceColor("=========================================="))
		fmt.Println(serviceColor(fmt.Sprintf("Starting Downloading Mocks for Service: %s ....", entry.Name())))
		fmt.Println(serviceColor("=========================================="))

		if err := yaml.CopyDir(mocksSourcePath, serviceFolder, true, s.logger); err != nil {
			fmt.Println("Error moving directory:", err)
			return err
		}
		s.logger.Info("Service's schema mocks contracts downloaded", zap.String("service", entry.Name()), zap.String("mocks", mocksSourcePath))

		// Move the Keploy version mocks
		mocksFolders, err := os.ReadDir(filepath.Join(cprFolder, entry.Name(), "keploy"))
		if err != nil {
			utils.LogError(s.logger, err, "failed to read directory", zap.String("directory", cprFolder), zap.Error(err))
			return err
		}
		for _, mockFolder := range mocksFolders {
			if !mockFolder.IsDir() || !strings.Contains(mockFolder.Name(), "test") {
				continue
			}
			httpMocks, err := s.mockDB.GetHTTPMocks(ctx, mockFolder.Name(), filepath.Join(cprFolder, entry.Name(), "keploy"), "mocks")
			if err != nil {
				utils.LogError(s.logger, err, "failed to get HTTP mocks", zap.String("testSetID", mockFolder.Name()), zap.Error(err))
				return err
			}
			var filteredMocks []*models.HTTPDoc
			for _, mock := range httpMocks {
				for _, service := range schemaConfigFile.Contract.ServicesMapping[self] {
					if service == mock.Spec.Request.URL {
						filteredMocks = append(filteredMocks, mock)
						break
					}
				}

			}
			var initialMock = true
			for _, mock := range filteredMocks {
				mockYAML, err := yamlLib.Marshal(mock)
				if err != nil {
					return err
				}
				err = yaml.WriteFile(ctx, s.logger, filepath.Join(serviceFolder, mockFolder.Name()), "mocks", mockYAML, !initialMock)
				if err != nil {
					return err
				}
				if initialMock {
					initialMock = false
				}
			}
			s.logger.Info("Service's HTTP mocks contracts downloaded", zap.String("service", entry.Name()), zap.String("mocks", mockFolder.Name()))

		}

	}

	return nil
}

func (s *contract) Download(ctx context.Context) error {

	if checkConfigFile(s.config.Contract.ServicesMapping) != nil {
		utils.LogError(s.logger, fmt.Errorf("Error in checking config file while validating"), "Error in checking config file while validating")
		return fmt.Errorf("Error in checking config file while validating")
	}
	path := s.config.Contract.Path
	// Validate the path
	path, err := yaml.ValidatePath(path)
	if err != nil {
		utils.LogError(s.logger, err, "failed to validate path")
		return fmt.Errorf("Error in validating path")
	}
	driven := s.config.Contract.Driven
	if driven == "provider" || driven == "server" {
		err = s.DownloadTests(ctx, path)

	} else if driven == "consumer" || driven == "client" {
		err = s.DownloadMocks(ctx, path)

	}
	if err != nil {
		return err
	}
	return nil
}

func (s *contract) Validate(ctx context.Context) error {
	if checkConfigFile(s.config.Contract.ServicesMapping) != nil {
		utils.LogError(s.logger, fmt.Errorf("Error in checking config file while validating"), "Error in checking config file while validating")
		return fmt.Errorf("Error in checking config file while validating")
	}

	if s.config.Contract.Generate {
		err := s.Generate(ctx)
		if err != nil {
			return err
		}
	}
	if s.config.Contract.Download {
		err := s.Download(ctx)
		if err != nil {
			return err
		}
	}
	if s.config.Contract.Driven == "consumer" {

		// Retrieve tests from the schema folder
		testsMapping, err := s.GetAllTestsSchema(ctx)
		if err != nil {
			utils.LogError(s.logger, err, "failed to get tests from schema")
			return err
		}
		// Retrieve mocks of each service from the download folder
		mocksSchemasDownloaded, err := s.GetAllDownloadedMocksSchemas(ctx)
		if err != nil {
			utils.LogError(s.logger, err, "failed to get downloaded mocks schemas")
			return err
		}
		err = s.consumer.ConsumerDrivenValidation(testsMapping, mocksSchemasDownloaded)
		if err != nil {
			return err
		}
	} else if s.config.Contract.Driven == "provider" {
		err := s.provider.ProviderDrivenValidation(ctx)
		if err != nil {
			return err
		}
		fmt.Println("Provider driven validation is not implemented yet")
	}

	return nil
}
