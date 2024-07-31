// Package contract provides the implementation of the contract service
package contract

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

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
	custom, err := readOrParseData(ctx, logger, filePath, name, readData, data)
	if err != nil {
		logger.Fatal("Error reading or parsing data", zap.Error(err))
		return false
	}

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
	err = validateOpenAPIDocument(logger, openapiYAML)
	if err != nil {
		return false
	}

	err = writeOpenAPIToFile(ctx, logger, outputPath, name, openapiYAML, isAppend)
	return err == nil
}

func (s *contractService) GenerateMocksSchemas(ctx context.Context, services []string, mappings map[string][]string, genAllMocks bool) error {
	keployFolder := "./keploy/"
	entries, err := os.ReadDir(keployFolder)
	if err != nil {
		s.logger.Error("Failed to read directory", zap.String("directory", keployFolder), zap.Error(err))
		return err
	}
	if err := validateServices(services, mappings, genAllMocks, s.logger); err != nil {
		return err
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
	configData, err := yaml.ReadFile(ctx, s.logger, "./keploy/schema", "keploy")
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

func (s *contractService) DownloadTests(ctx context.Context, path string) error {
	fmt.Println("Path given (not simulated): ", path)

	targetPath := "./Download/Tests"
	if err := yaml.CreateDir(targetPath, s.logger); err != nil {
		return err
	}

	cprFolder := "/home/ahmed/Desktop/GSOC/Keploy/Issues/VirtualCPR"

	var schemaConfigFile config.Config

	configFilePath := filepath.Join("./keploy", "schema")
	if err := yaml.ReadYAMLFile(ctx, s.logger, configFilePath, "keploy", &schemaConfigFile); err != nil {
		return err
	}
	// Loop through the services in the mappings in the config file
	for service := range schemaConfigFile.Contract.ServicesMapping {
		// Fetch the tests of those services from virtual cpr
		testsPath := filepath.Join(cprFolder, service, "keploy", "schema", "tests")
		// Copy this dir to the target path
		serviceFolder := filepath.Join(targetPath, service)
		if err := yaml.CopyDir(testsPath, serviceFolder, false, s.logger); err != nil {
			fmt.Println("Error copying directory:", err)
			return err
		}
		s.logger.Info("Service's tests contracts downloaded", zap.String("service", service))
		// Copy the Keploy version (HTTP) tests
		keployTestsPath := filepath.Join(cprFolder, service, "keploy")
		testEntries, err := os.ReadDir(keployTestsPath)
		if err != nil {
			s.logger.Error("Failed to read directory", zap.String("directory", keployTestsPath), zap.Error(err))
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
func (s *contractService) DownloadMocks(ctx context.Context, path string) error {
	fmt.Println("Path given (not simulated): ", path)
	targetPath := "./Download/Mocks"
	if err := yaml.CreateDir(targetPath, s.logger); err != nil {
		return err
	}

	cprFolder := "/home/ahmed/Desktop/GSOC/Keploy/Issues/VirtualCPR"

	entries, err := os.ReadDir(cprFolder)
	if err != nil {
		s.logger.Error("Failed to read directory", zap.String("directory", cprFolder), zap.Error(err))
		return err
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		var self = s.config.Contract.Self
		var schemaConfigFile config.Config

		configFilePath := filepath.Join(cprFolder, entry.Name(), "keploy", "schema")
		if err := yaml.ReadYAMLFile(ctx, s.logger, configFilePath, "keploy", &schemaConfigFile); err != nil {
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
		if err := yaml.CopyDir(mocksSourcePath, serviceFolder, true, s.logger); err != nil {
			fmt.Println("Error moving directory:", err)
			return err
		}
		s.logger.Info("Service's schema mocks contracts downloaded", zap.String("service", entry.Name()), zap.String("mocks", mocksSourcePath))

		// Move the Keploy version mocks
		mocksFolders, err := os.ReadDir(filepath.Join(cprFolder, entry.Name(), "keploy"))
		if err != nil {
			s.logger.Error("Failed to read directory", zap.String("directory", cprFolder), zap.Error(err))
			return err
		}
		for _, mockFolder := range mocksFolders {
			if !mockFolder.IsDir() || !strings.Contains(mockFolder.Name(), "test") {
				continue
			}
			httpMocks, err := s.mockDB.GetHTTPMocks(ctx, mockFolder.Name(), filepath.Join(cprFolder, entry.Name(), "keploy"))
			if err != nil {
				s.logger.Error("Failed to get HTTP mocks", zap.String("testSetID", mockFolder.Name()), zap.Error(err))
				return err
			}
			var filteredMocks []*models.HTTPSchema2
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

func (s *contractService) Download(ctx context.Context, driven string) error {

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
		err = s.DownloadTests(ctx, path)

	} else if driven == "consumer" || driven == "client" {

		err = s.DownloadMocks(ctx, path)
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
