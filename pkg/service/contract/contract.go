// Package contract provides the implementation of the contract service
package contract

import (
	"context"
	"fmt"
	"os"

	"go.keploy.io/server/v2/config"
	"go.keploy.io/server/v2/pkg/platform/yaml"

	"go.uber.org/zap"
)

type Instrumentation interface {
}

// contractService implements the Service interface
type contractService struct {
	logger *zap.Logger
	config *config.Config
}

func (s *contractService) Generate(ctx context.Context, genTests bool) error {
	fmt.Println("HELLO IN CONTRACT SERVICE")
	if s.CheckConfigFile() != nil {
		s.logger.Error("Error in checking config file while generating")
		return fmt.Errorf("Error in checking config file while generating")
	}
	err := yaml.GenerateHelper(ctx, s.logger, s.config.Contract.Services, genTests)
	if err != nil {
		s.logger.Error("Error in generating contract", zap.Error(err))
		return fmt.Errorf("Error in generating contract")
	}
	return nil
}
func (s *contractService) Download(_ context.Context, genTests bool) error {
	fmt.Printf("Download contract for services: %v\n", s.config.Contract.Services)

	if s.CheckConfigFile() != nil {
		s.logger.Error("Error in checking config file while downloading")
		return fmt.Errorf("Error in checking config file while downloading")
	}
	path := s.config.Contract.Path
	// Validate the path
	path, err := yaml.ValidatePath(path)
	// Ensure destination directory exists
	if _, err := os.Stat(path); os.IsNotExist(err) {
		err := os.Mkdir(path, os.ModePerm)
		if err != nil {
			s.logger.Error("Error creating destination directory:")
			return err
		}
	}
	if err != nil {
		s.logger.Error("Error in validating path", zap.Error(err))
		return fmt.Errorf("Error in validating path")
	}
	keployFolder := "./keploy/"

	if genTests {
		var downloadedTests []string
		// Read the directory contents
		entries, err := os.ReadDir(keployFolder + "schema/tests/")
		if err != nil {
			s.logger.Error("Failed to read directory", zap.String("directory", keployFolder), zap.Error(err))
			return err
		}
		testsProvidedByUser := s.config.Contract.Tests
		var genAll bool
		if len(testsProvidedByUser) == 0 {
			genAll = true
		}
		if genAll {
			for _, entry := range entries {
				err := yaml.CopyDir(keployFolder+"schema/tests/"+entry.Name(), path+"/"+entry.Name(), s.logger)
				if err != nil {
					fmt.Println("Error moving directory:", err)
					return err
				}

			}
		} else {
			// Iterate over directory entries
			for _, entry := range entries {
				for _, test := range testsProvidedByUser {
					if entry.Name() == test {
						downloadedTests = append(downloadedTests, test)
						err := yaml.CopyDir(keployFolder+"schema/tests/"+entry.Name(), path+"/"+entry.Name(), s.logger)
						if err != nil {
							fmt.Println("Error moving directory:", err)
							return err
						}
						break
					}

				}
			}
			// Check if all the tests provided by the user are downloaded
			for _, test := range testsProvidedByUser {
				if !yaml.Contains(downloadedTests, test) {
					s.logger.Warn("Test Contract not found", zap.String("test", test))
				}
			}
		}
	} else {
		var downloadedServices []string
		// Read the directory contents
		entries, err := os.ReadDir(keployFolder + "schema/mocks/")
		if err != nil {
			s.logger.Error("Failed to read directory", zap.String("directory", keployFolder), zap.Error(err))
			return err
		}
		servicesProvidedByUser := s.config.Contract.Services
		for _, entry := range entries {
			for _, service := range servicesProvidedByUser {
				if entry.Name() == service {
					downloadedServices = append(downloadedServices, service)
					// Move that directory to path
					err := yaml.CopyDir(keployFolder+"schema/mocks/"+entry.Name(), path+"/"+entry.Name(), s.logger)
					if err != nil {
						fmt.Println("Error moving directory:", err)
						return err
					}
					s.logger.Info("Service's mocks contracts downloaded", zap.String("service", service))
					break

				}
			}

		}
		// Check if all the services provided by the user are downloaded
		for _, service := range servicesProvidedByUser {
			if !yaml.Contains(downloadedServices, service) {
				s.logger.Warn("Service Contract not found", zap.String("service", service))
			}
		}

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

// New creates a new contractService
func New(logger *zap.Logger, config *config.Config) Service {
	return &contractService{
		logger: logger,
		config: config,
	}
}
