// Package contract provides the implementation of the contract service
package contract

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/fatih/color"
	"go.keploy.io/server/v2/config"
	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/pkg/platform/yaml"

	"go.uber.org/zap"
	yamlLib "gopkg.in/yaml.v3"
)

const IDENTIFYMODE = 0
const COMPAREMODE = 1

// contractService implements the Service interface
type contractService struct {
	logger    *zap.Logger
	testDB    TestDB
	mockDB    MockDB
	openAPIDB OpenAPIDB
	config    *config.Config
}

func New(logger *zap.Logger, testDB TestDB, mockDB MockDB, openAPIDB OpenAPIDB, config *config.Config) Service {
	return &contractService{
		logger:    logger,
		testDB:    testDB,
		mockDB:    mockDB,
		openAPIDB: openAPIDB,
		config:    config,
	}
}

func (s *contractService) HTTPDocToOpenAPI(ctx context.Context, logger *zap.Logger, filePath string, name string, outputPath string, readData bool, data models.HTTPDoc, isAppend bool) (success bool) {
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
			httpMocks, err := s.mockDB.GetHTTPMocks(ctx, testSetID, keployFolder, "mocks")
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
							done := s.HTTPDocToOpenAPI(ctx, s.logger, keployFolder+entry.Name(), "mocks", keployFolder+"schema/mocks/"+service+"/"+entry.Name(), false, *mock, isAppend)
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
	testSetsIDs, err := s.testDB.GetAllTestSetIDs(ctx)
	if err != nil {
		s.logger.Error("Failed to get test set IDs", zap.Error(err))
		return err
	}

	for _, entry := range testSetsIDs {
		testSetID := entry
		if !yaml.Contains(selectedTests, testSetID) && !genAllTests {
			continue
		}

		testCases, err := s.testDB.GetTestCases(ctx, testSetID)
		if err != nil {
			s.logger.Error("Failed to get test cases", zap.String("testSetID", testSetID), zap.Error(err))
			return err
		}
		for _, tc := range testCases {
			var httpSpec models.HTTPDoc
			httpSpec.Kind = string(tc.Kind)
			httpSpec.Name = tc.Name
			httpSpec.Spec.Request = tc.HTTPReq
			httpSpec.Spec.Response = tc.HTTPResp
			httpSpec.Version = string(tc.Version)

			done := s.HTTPDocToOpenAPI(ctx, s.logger, filepath.Join(keployFolder, entry, "tests"), tc.Name, filepath.Join(keployFolder, "schema", "tests", entry), false, httpSpec, false)
			if !done {
				s.logger.Error("Failed to convert the yaml file to openapi")
				return fmt.Errorf("failed to convert the yaml file to openapi")
			}
		}

	}
	return nil
}

func (s *contractService) Generate(ctx context.Context) error {
	if checkConfigFile(s.config.Contract.ServicesMapping) != nil {
		s.logger.Error("Error in checking config file while validating")
		return fmt.Errorf("Error in checking config file while validating")
	}

	genAllMocks := true
	genAllTests := true

	if len(s.config.Contract.Services) != 0 {
		genAllMocks = false
	}
	if len(s.config.Contract.Tests) != 0 {
		genAllTests = false
	}

	var config config.Config
	err := yaml.ReadYAMLFile(ctx, s.logger, "./", "keploy", &config, false)
	// configData, err := yaml.ReadFile(ctx, s.logger, "./", "keploy")
	if err != nil {
		s.logger.Fatal("Error reading file", zap.Error(err))
		return err
	}

	mappings := config.Contract.ServicesMapping
	serviceColor := color.New(color.FgYellow).SprintFunc()
	fmt.Println(serviceColor("=========================================="))
	fmt.Println(serviceColor(fmt.Sprintf("Starting Generating OpenAPI Schemas for Current Service: %s ....", s.config.Contract.Self)))
	fmt.Println(serviceColor("=========================================="))

	err = s.GenerateTestsSchemas(ctx, s.config.Contract.Tests, genAllTests)
	if err != nil {
		return err
	}
	err = s.GenerateMocksSchemas(ctx, s.config.Contract.Services, mappings, genAllMocks)
	if err != nil {
		return err
	}
	if err := saveServiceMappings(config, "./keploy/schema"); err != nil {
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

	cprFolder, err := filepath.Abs("../VirtualCPR")
	if err != nil {
		s.logger.Fatal("Failed to resolve path:", zap.Error(err))
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

	cprFolder, err := filepath.Abs("../VirtualCPR")
	if err != nil {
		s.logger.Fatal("Failed to resolve path:", zap.Error(err))
	}
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
			s.logger.Error("Failed to read directory", zap.String("directory", cprFolder), zap.Error(err))
			return err
		}
		for _, mockFolder := range mocksFolders {
			if !mockFolder.IsDir() || !strings.Contains(mockFolder.Name(), "test") {
				continue
			}
			httpMocks, err := s.mockDB.GetHTTPMocks(ctx, mockFolder.Name(), filepath.Join(cprFolder, entry.Name(), "keploy"), "mocks")
			if err != nil {
				s.logger.Error("Failed to get HTTP mocks", zap.String("testSetID", mockFolder.Name()), zap.Error(err))
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

func (s *contractService) Download(ctx context.Context) error {

	if checkConfigFile(s.config.Contract.ServicesMapping) != nil {
		s.logger.Error("Error in checking config file while validating")
		return fmt.Errorf("Error in checking config file while validating")
	}
	path := s.config.Contract.Path
	// Validate the path
	path, err := yaml.ValidatePath(path)
	if err != nil {
		s.logger.Error("Error in validating path", zap.Error(err))
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

// func (s *contractService) ProviderDrivenValidation(ctx context.Context) error {
// 	downloadTestsFolder := filepath.Join("./Download", "Tests")
// 	entries, err := os.ReadDir(downloadTestsFolder)
// 	if err != nil {
// 		s.logger.Error("Failed to read directory", zap.String("directory", downloadTestsFolder), zap.Error(err))
// 		return err
// 	}
// 	mocksFolder := filepath.Join("./keploy", "schema", "mocks")
// 	s.openAPIDB.ChangeTcPath(mocksFolder)
// 	services, err := os.ReadDir(mocksFolder)
// 	if err != nil {
// 		s.logger.Error("Failed to read directory", zap.String("directory", mocksFolder), zap.Error(err))
// 		return err
// 	}
// 	var mocksMapping map[string]map[string]map[string]*models.OpenAPI = make(map[string]map[string]map[string]*models.OpenAPI)
// 	var mocks []*models.OpenAPI
// 	for _, service := range services {
// 		if !service.IsDir() {
// 			continue
// 		}
// 		testSetIDs, err := os.ReadDir(filepath.Join(mocksFolder, service.Name()))
// 		if err != nil {
// 			s.logger.Error("Failed to read directory", zap.String("directory", service.Name()), zap.Error(err))
// 			return err
// 		}
// 		mocksMapping[service.Name()] = make(map[string]map[string]*models.OpenAPI)
// 		for _, testSetID := range testSetIDs {

// 			if !testSetID.IsDir() {
// 				continue
// 			}
// 			mocks, err = s.openAPIDB.GetMocksSchemas(ctx, filepath.Join(service.Name(), testSetID.Name()), mocksFolder, "mocks")
// 			if err != nil {
// 				s.logger.Error("Failed to get HTTP mocks", zap.String("testSetID", testSetID.Name()), zap.Error(err))
// 				return err
// 			}
// 			mocksMapping[service.Name()][testSetID.Name()] = make(map[string]*models.OpenAPI)
// 			for _, mock := range mocks {
// 				mocksMapping[service.Name()][testSetID.Name()][mock.Info.Title] = mock
// 			}
// 		}
// 	}
// 	var scores map[string]map[string]map[string]models.SchemaInfo = make(map[string]map[string]map[string]models.SchemaInfo)
// 	for _, entry := range entries {
// 		if entry.IsDir() {
// 			serviceFolder := filepath.Join(downloadTestsFolder, entry.Name())
// 			testSetIDs, err := os.ReadDir(filepath.Join(serviceFolder, "schema", "tests"))
// 			if err != nil {
// 				s.logger.Error("Failed to read directory", zap.String("directory", serviceFolder), zap.Error(err))
// 				return err
// 			}
// 			scores[entry.Name()] = make(map[string]map[string]models.SchemaInfo)
// 			for _, testSetID := range testSetIDs {
// 				if !testSetID.IsDir() {
// 					continue
// 				}
// 				tests, err := s.openAPIDB.GetTestCasesSchema(ctx, testSetID.Name(), filepath.Join(serviceFolder, "schema", "tests"))
// 				if err != nil {
// 					s.logger.Error("Failed to get test cases", zap.String("testSetID", testSetID.Name()), zap.Error(err))
// 					return err
// 				}
// 				scores[entry.Name()][testSetID.Name()] = make(map[string]models.SchemaInfo)
// 				for _, test := range tests {
// 					// Take each test and get the ideal mock for it
// 					scores[entry.Name()][testSetID.Name()][test.Info.Title] = models.SchemaInfo{Score: 0.0, Data: *test}
// 					for providerService, mockSetIDs := range mocksMapping {
// 						for mockSetID, mocks := range mockSetIDs {
// 							for _, mock := range mocks {
// 								candidateScore, pass, err := match2(*test, *mock, mockSetID, testSetID.Name(), s.logger, IDENTIFYMODE)
// 								if err != nil {
// 									s.logger.Error("Error in matching the two models", zap.Error(err))
// 									fmt.Println("test-set-id: ", testSetID.Name(), ", mock-set-id: ", mockSetID)
// 									return err
// 								}
// 								if pass && candidateScore > 0 {
// 									if candidateScore > scores[entry.Name()][testSetID.Name()][test.Info.Title].Score {
// 										idealMock := models.SchemaInfo{
// 											Service:   providerService,
// 											TestSetID: mockSetID,
// 											Name:      mock.Info.Title,
// 											Score:     candidateScore,
// 											Data:      *test,
// 										}
// 										scores[entry.Name()][testSetID.Name()][test.Info.Title] = idealMock
// 									}
// 								}

// 							}
// 						}
// 					}

// 				}
// 			}
// 		}
// 	}
// 	// TODO: Validate the scores and generate a summary

// 	return nil
// }

func (s *contractService) ConsumerDrivenValidation(ctx context.Context) error {
	// Loop over Mocks in DOwnload folder and compare them with the tests in the keploy schema folder
	downloadMocksFolder := filepath.Join("./Download", "Mocks")

	testsFolder := filepath.Join("./keploy", "schema", "tests")

	// Retrieve tests from the schema folder
	testsMapping, err := s.getTestsSchema(ctx, testsFolder)
	if err != nil {
		s.logger.Error("Failed to get test cases from schema", zap.Error(err))
		return err
	}

	// Retrieve mocks and calculate scores for each service
	scores, err := s.getMockScores(ctx, downloadMocksFolder, testsMapping)
	if err != nil {
		return err
	}
	// Compare the scores and generate a summary
	summary, err := s.ValidateMockAgainstTests(scores, testsMapping)
	if err != nil {
		return err
	}
	// Print the summary
	generateSummaryTable(summary)

	return nil
}
func (s *contractService) Validate(ctx context.Context) error {
	if checkConfigFile(s.config.Contract.ServicesMapping) != nil {
		s.logger.Error("Error in checking config file while validating")
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
	if s.config.Contract.Driven == "consumer" || s.config.Contract.Driven == "client" {
		err := s.ConsumerDrivenValidation(ctx)
		if err != nil {
			return err
		}
	} else if s.config.Contract.Driven == "provider" || s.config.Contract.Driven == "server" {
		// err := s.ProviderDrivenValidation(ctx)
		// if err != nil {
		// 	return err
		// }
		fmt.Println("Provider driven validation is not implemented yet")
	}

	return nil
}
