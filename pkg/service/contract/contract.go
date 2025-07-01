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

func (s *contract) HTTPDocToOpenAPI(logger *zap.Logger, custom models.HTTPDoc) (models.OpenAPI, error) {

	var err error

	// Helper function to check if content is JSON
	isJSONContent := func(contentType string) bool {
		return strings.Contains(contentType, "application/json") ||
			strings.Contains(contentType, "application/ld+json")
	}

	// Parse response body - only for JSON content
	var responseBodyObject map[string]interface{}
	if custom.Spec.Response.Body != "" {
		contentType, exists := custom.Spec.Response.Header["Content-Type"]
		if exists && isJSONContent(contentType) {
			err := json.Unmarshal([]byte(custom.Spec.Response.Body), &responseBodyObject)
			if err != nil {
				return models.OpenAPI{}, err
			}
		}
		// For non-JSON content (HTML, text, etc.) - skip parsing
		// responseBodyObject remains nil
	}

	// Get the type of each value in the response body object
	responseTypes := ExtractVariableTypes(responseBodyObject)

	// Parse request body - only for JSON content
	var requestBodyObject map[string]interface{}
	if custom.Spec.Request.Body != "" {
		contentType, exists := custom.Spec.Request.Header["Content-Type"]
		if exists && isJSONContent(contentType) {
			err := json.Unmarshal([]byte(custom.Spec.Request.Body), &requestBodyObject)
			if err != nil {
				return models.OpenAPI{}, err
			}
		}
		// For non-JSON content - skip parsing
		// requestBodyObject remains nil
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
	case "OPTIONS":
		pathItem.Options = &models.Operation{
			Summary:     "CORS preflight request",
			Description: "Auto-generated CORS preflight operation",
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
		utils.LogError(logger, err, "failed to extract URL path")
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

// GenerateMocksSchemas generates mock schemas based on the provided services and mappings.
func (s *contract) GenerateMocksSchemas(ctx context.Context, services []string, mappings map[string][]string) error {

	// Retrieve all test set IDs from the test database.
	testSetsIDs, err := s.testDB.GetAllTestSetIDs(ctx)
	if err != nil {
		// Log and return error if test set IDs retrieval fails.
		utils.LogError(s.logger, err, "failed to get test set IDs")
		return err
	}

	// If specific services are provided, ensure they exist in the mappings.
	if len(services) != 0 {
		for _, service := range services {
			if _, exists := mappings[service]; !exists {
				// Warn if the service is not found in the services mapping.
				s.logger.Warn("Service not found in services mapping, no contract generation", zap.String("service", service))
			}
		}
	}

	// Loop through each test set ID to process the HTTP mocks.
	for _, testSetID := range testSetsIDs {
		// Retrieve HTTP mocks for the test set from the mock database.
		httpMocks, err := s.mockDB.GetHTTPMocks(ctx, testSetID, s.config.Path, "mocks")
		if err != nil {
			// Log and return error if HTTP mock retrieval fails.
			utils.LogError(s.logger, err, "failed to get HTTP mocks", zap.String("testSetID", testSetID))
			return err
		}

		// Track duplicate mocks to avoid generating the same schema multiple times.
		var duplicateServices []string

		// Loop through each HTTP mock to generate OpenAPI documentation.
		for _, mock := range httpMocks {
			var isAppend bool // Flag to indicate whether to append to existing mocks.

			// Loop through services and their mappings to find the relevant mock.
			for service, serviceMappings := range mappings {
				// If a specific service list is provided, skip services not in the list.
				if !yaml.Contains(services, service) && len(services) != 0 {
					continue
				}

				var mappingFound bool // Flag to track if the mapping for the service is found.

				// Check if the mock's URL matches any service mapping.
				for _, mapping := range serviceMappings {
					if mapping == mock.Spec.Request.URL {

						// Check for duplicate services to append the mock to the existing mocks.yaml before.
						if yaml.Contains(duplicateServices, service) {
							isAppend = true
						} else {
							duplicateServices = append(duplicateServices, service)
						}

						// Convert the HTTP mock to OpenAPI documentation.
						openapi, err := s.HTTPDocToOpenAPI(s.logger, *mock)
						if err != nil {
							utils.LogError(s.logger, err, "failed to convert the yaml file to openapi")
							return fmt.Errorf("failed to convert the yaml file to openapi")
						}

						// Validate the generated OpenAPI schema.
						err = validateSchema(openapi)
						if err != nil {
							utils.LogError(s.logger, err, "failed to validate the OpenAPI schema")
							return err
						}

						// Write the OpenAPI document to the specified directory.
						err = s.openAPIDB.WriteSchema(ctx, s.logger, filepath.Join(s.config.Path, "schema", "mocks", service, testSetID), "mocks", openapi, isAppend)
						if err != nil {
							utils.LogError(s.logger, err, "failed to write the OpenAPI schema")
							return err
						}

						mappingFound = true // Mark the mapping as found.
						break
					}
				}

				// Break the outer loop if the relevant mapping is found.
				if mappingFound {
					break
				}
			}
		}
	}

	return nil // Return nil if the function completes successfully.
}

func (s *contract) GenerateTestsSchemas(ctx context.Context, selectedTests []string) error {
	testSetsIDs, err := s.testDB.GetAllTestSetIDs(ctx)
	if err != nil {
		utils.LogError(s.logger, err, "failed to get test set IDs")
		return err
	}

	for _, testSetID := range testSetsIDs {
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
				utils.LogError(s.logger, err, "failed to convert the yaml file to openapi")
				return fmt.Errorf("failed to convert the yaml file to openapi")
			}
			// Validate the OpenAPI document
			err = validateSchema(openapi)
			if err != nil {
				utils.LogError(s.logger, err, "failed to validate the OpenAPI schema")
				return err
			}
			// Save it using the OpenAPIDB
			err = s.openAPIDB.WriteSchema(ctx, s.logger, filepath.Join(s.config.Path, "schema", "tests", testSetID), tc.Name, openapi, false)
			if err != nil {
				utils.LogError(s.logger, err, "failed to write the OpenAPI schema")
				return err
			}

		}

	}
	return nil
}

func (s *contract) Generate(ctx context.Context, checkConfig bool) error {
	if checkConfig && checkConfigFile(s.config.Contract.Mappings.ServicesMapping) != nil {
		utils.LogError(s.logger, fmt.Errorf("unable to find services mappings in the config file"), "Unable to find services mappings in the config file")
		return fmt.Errorf("unable to find services mappings in the config file")
	}

	mappings := s.config.Contract.Mappings.ServicesMapping
	serviceColor := color.New(color.FgYellow).SprintFunc()
	fmt.Println(serviceColor("=========================================="))
	fmt.Println(serviceColor(fmt.Sprintf("Starting Generating OpenAPI Schemas for Current Service: %s ....", s.config.Contract.Mappings.Self)))
	fmt.Println(serviceColor("=========================================="))

	err := s.GenerateTestsSchemas(ctx, s.config.Contract.Tests)
	if err != nil {
		utils.LogError(s.logger, err, "failed to generate tests schemas")
		return err
	}
	err = s.GenerateMocksSchemas(ctx, s.config.Contract.Services, mappings)
	if err != nil {
		utils.LogError(s.logger, err, "failed to generate mocks schemas")
		return err
	}
	if err := saveServiceMappings(s.config.Contract.Mappings, filepath.Join(s.config.Path, "schema")); err != nil {
		utils.LogError(s.logger, err, "failed to save service mappings")
		return err
	}

	return nil
}

func (s *contract) DownloadTests(_ string) error {

	targetPath := "./Download/Tests"
	if err := yaml.CreateDir(targetPath, s.logger); err != nil {
		utils.LogError(s.logger, err, "failed to create directory", zap.String("directory", targetPath))
		return err
	}

	cprFolder, err := filepath.Abs("../VirtualCPR")
	if err != nil {
		utils.LogError(s.logger, err, "failed to get absolute path", zap.String("path", "../VirtualCPR"))
		return err
	}

	// Loop through the services in the mappings in the config file
	for service := range s.config.Contract.Mappings.ServicesMapping {
		// Fetch the tests of those services from virtual cpr
		testsPath := filepath.Join(cprFolder, service, "keploy", "schema", "tests")
		// Copy this dir to the target path
		serviceFolder := filepath.Join(targetPath, service)
		if err := yaml.CopyDir(testsPath, serviceFolder, false, s.logger); err != nil {
			utils.LogError(s.logger, err, "failed to copy directory", zap.String("directory", testsPath))
			return err
		}
		s.logger.Info("Service's tests (contracts) downloaded", zap.String("service", service))
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
				utils.LogError(s.logger, err, "failed to copy directory", zap.String("directory", filepath.Join(keployTestsPath, testSetID.Name(), "tests")))
				return err
			}
			s.logger.Info("Service's HTTP tests downloaded", zap.String("service", service), zap.String("tests", testSetID.Name()))

		}

	}
	return nil
}

// DownloadMocks downloads the mocks for a specific service and stores them in the target path.
// The mocks are extracted from the VirtualCPR folder and saved in the "Download/Mocks" directory.
func (s *contract) DownloadMocks(ctx context.Context, _ string) error {
	// Set the target path where the downloaded mocks will be stored
	targetPath := "./Download/Mocks"

	// Create the target directory if it doesn't already exist
	if err := yaml.CreateDir(targetPath, s.logger); err != nil {
		utils.LogError(s.logger, err, "failed to create directory", zap.String("directory", targetPath))
		return err
	}

	// Get the absolute path to the VirtualCPR folder
	cprFolder, err := filepath.Abs("../VirtualCPR")
	if err != nil {
		utils.LogError(s.logger, err, "failed to get absolute path", zap.String("path", "../VirtualCPR"))
		return err
	}

	// Read all entries (files and directories) in the VirtualCPR folder
	entries, err := os.ReadDir(cprFolder)
	if err != nil {
		utils.LogError(s.logger, err, "failed to read directory", zap.String("directory", cprFolder))
		return err
	}

	// Loop through each entry in the VirtualCPR folder
	for _, entry := range entries {
		// If the entry is not a directory, skip it
		if !entry.IsDir() {
			continue
		}

		// Extract the name of the current service (the one being processed)
		var self = s.config.Contract.Mappings.Self
		var schemaConfigMappings config.Mappings

		// Construct the path to the schema configuration file for the current service
		configFilePath := filepath.Join(cprFolder, entry.Name(), "keploy", "schema")

		// Read the schema configuration YAML schemaConfigMappings
		if err := yaml.ReadYAMLFile(ctx, s.logger, configFilePath, "serviceMappings", &schemaConfigMappings, true); err != nil {
			utils.LogError(s.logger, err, "failed to read the schema configuration file", zap.String("file", "serviceMappings"))
			return err
		}

		// Check if the current service exists in the service mapping from the schema configuration
		serviceFound := false
		if _, exists := schemaConfigMappings.ServicesMapping[self]; exists {
			serviceFound = true
		}

		// If the service is not found in the mapping, skip to the next service
		if !serviceFound {
			continue
		}

		// Create a directory for the current service inside the target path
		serviceFolder := filepath.Join(targetPath, schemaConfigMappings.Self)
		if err := yaml.CreateDir(serviceFolder, s.logger); err != nil {
			utils.LogError(s.logger, err, "failed to create directory", zap.String("directory", serviceFolder))
			return err
		}

		// Construct the path to the mock files for the current service
		mocksSourcePath := filepath.Join(cprFolder, entry.Name(), "keploy", "schema", "mocks", self)

		// Log and display the start of the mock download process for the service
		serviceColor := color.New(color.FgYellow).SprintFunc()
		fmt.Println(serviceColor("=========================================="))
		fmt.Println(serviceColor(fmt.Sprintf("Starting Downloading Mocks for Service: %s ....", entry.Name())))
		fmt.Println(serviceColor("=========================================="))

		// Copy the mock files from the source directory to the target directory
		if err := yaml.CopyDir(mocksSourcePath, serviceFolder, true, s.logger); err != nil {
			utils.LogError(s.logger, err, "failed to copy directory", zap.String("directory", mocksSourcePath))
			return err
		}

		// Log that the mocks for the service have been downloaded
		s.logger.Info("Service's schema mocks contracts downloaded", zap.String("service", entry.Name()), zap.String("mocks", mocksSourcePath))

		// Move the Keploy version-specific mocks
		// Read the contents of the "keploy" folder to find the mock folders
		mocksFolders, err := os.ReadDir(filepath.Join(cprFolder, entry.Name(), "keploy"))
		if err != nil {
			utils.LogError(s.logger, err, "failed to read directory", zap.String("directory", cprFolder), zap.Error(err))
			return err
		}

		// Loop through each folder inside the "keploy" folder
		for _, mockFolder := range mocksFolders {
			// If the folder is not a directory or does not contain "test" in its name, skip it
			if !mockFolder.IsDir() || !strings.Contains(mockFolder.Name(), "test") {
				continue
			}

			// Retrieve the HTTP mocks from the mock database for the current test set
			httpMocks, err := s.mockDB.GetHTTPMocks(ctx, mockFolder.Name(), filepath.Join(cprFolder, entry.Name(), "keploy"), "mocks")
			if err != nil {
				utils.LogError(s.logger, err, "failed to get HTTP mocks", zap.String("testSetID", mockFolder.Name()), zap.Error(err))
				return err
			}

			// Filter the HTTP mocks based on the service URL mappings
			var filteredMocks []*models.HTTPDoc
			for _, mock := range httpMocks {
				for _, service := range schemaConfigMappings.ServicesMapping[self] {
					// Add the mock to the filtered list if the service URL matches
					if service == mock.Spec.Request.URL {
						filteredMocks = append(filteredMocks, mock)
						break
					}
				}
			}

			// Write the filtered mocks to the appropriate folder
			var initialMock = true
			for _, mock := range filteredMocks {
				// Marshal the mock data to YAML format
				mockYAML, err := yamlLib.Marshal(mock)
				if err != nil {
					utils.LogError(s.logger, err, "failed to marshal mock data", zap.Any("mock", mock))
					return err
				}

				// Write the mock YAML file to the target service folder
				err = yaml.WriteFile(ctx, s.logger, filepath.Join(serviceFolder, mockFolder.Name()), "mocks", mockYAML, !initialMock)
				if err != nil {
					utils.LogError(s.logger, err, "failed to write mock file", zap.String("service", entry.Name()), zap.String("testSetID", mockFolder.Name()))
					return err
				}

				// Ensure only the first file is marked as the initial mock
				if initialMock {
					initialMock = false
				}
			}

			// Log that the HTTP mocks for the service have been downloaded
			s.logger.Info("Service's HTTP mocks contracts downloaded", zap.String("service", entry.Name()), zap.String("mocks", mockFolder.Name()))
		}
	}

	// Return nil to indicate success
	return nil
}

func (s *contract) Download(ctx context.Context, checkConfig bool) error {

	if checkConfig && checkConfigFile(s.config.Contract.Mappings.ServicesMapping) != nil {
		utils.LogError(s.logger, fmt.Errorf("unable to find services mappings in the config file"), "Unable to find services mappings in the config file")
		return fmt.Errorf("unable to find services mappings in the config file")
	}
	path := s.config.Contract.Path
	// Validate the path
	path, err := yaml.ValidatePath(path)
	if err != nil {
		utils.LogError(s.logger, err, "failed to validate path")
		return fmt.Errorf("error in validating path")
	}
	driven := s.config.Contract.Driven
	if driven == models.ProviderMode.String() {
		err = s.DownloadTests(path)
		if err != nil {
			utils.LogError(s.logger, err, "failed to download tests")
			return err
		}

	} else if driven == models.ConsumerMode.String() {
		err = s.DownloadMocks(ctx, path)
		if err != nil {
			utils.LogError(s.logger, err, "failed to download mocks")
			return err
		}

	}

	return nil
}

func (s *contract) Validate(ctx context.Context) error {
	if s.config.Contract.Mappings.Self == "" {
		utils.LogError(s.logger, fmt.Errorf("self service is not defined in the config file"), "Self service is not defined in the config file")
		return fmt.Errorf("self service is not defined in the config file")
	}

	if s.config.Contract.Generate {
		err := s.Generate(ctx, false)
		if err != nil {
			utils.LogError(s.logger, err, "failed to generate contract")
			return err
		}
	}
	if s.config.Contract.Download {
		err := s.Download(ctx, false)
		if err != nil {
			utils.LogError(s.logger, err, "failed to download contract")
			return err
		}
	}
	if s.config.Contract.Driven == models.ConsumerMode.String() {

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
		err = s.consumer.ValidateSchema(testsMapping, mocksSchemasDownloaded)
		if err != nil {
			utils.LogError(s.logger, err, "failed to validate schema")
			return err
		}
	} else if s.config.Contract.Driven == models.ProviderMode.String() {
		err := s.provider.ValidateSchema(ctx)
		if err != nil {
			utils.LogError(s.logger, err, "failed to validate schema")
			return err
		}
		fmt.Println("Provider driven validation is not implemented yet")
	}

	return nil
}
