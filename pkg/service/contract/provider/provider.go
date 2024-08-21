// Package provider is a package for provider driven contract testing
package provider

import (
	"context"

	"go.keploy.io/server/v2/config"
	"go.uber.org/zap"
)

const IDENTIFYMODE = 0
const COMPAREMODE = 1

type provider struct {
	logger *zap.Logger

	config *config.Config
}

// New creates a new instance of the consumer service
func New(logger *zap.Logger, config *config.Config) Service {
	return &provider{
		logger: logger,

		config: config,
	}
}

func (s *provider) ProviderDrivenValidation(_ context.Context) error {
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

	return nil
}
