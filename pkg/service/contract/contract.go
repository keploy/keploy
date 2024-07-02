// Package contract provides the implementation of the contract service
package contract

import (
	"context"
	"fmt"

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

func (s *contractService) Generate(ctx context.Context, flag bool) error {
	fmt.Println("HELLO IN CONTRACT SERVICE")
	if s.CheckConfigFile() != nil {
		s.logger.Error("Error in checking config file while generating")
		return fmt.Errorf("Error in checking config file while generating")
	}
	if flag {
		fmt.Println("generate OPENAPI for services mocks")
		for _, service := range s.config.Contract.Services {
			fmt.Println(service)
			yaml.ConvertYamlToOpenAPI(ctx, s.logger, service, service)

		}
	} else {
		fmt.Println("generate OPENAPI for testcases")
	}
	// Implement the logic to call ConvertYamlToOpenAPI
	// ConvertYamlToOpenAPI(ctx, s.logger, filePath, name)
	// For now, just return nil
	return nil
}
func (s *contractService) Download(ctx context.Context) error {
	if s.CheckConfigFile() != nil {
		s.logger.Error("Error in checking config file while downloading")
		return fmt.Errorf("Error in checking config file while downloading")
	}
	fmt.Printf("Download contract for services: %v\n", s.config.Contract.Services)
	fmt.Printf("ctx: %v\n", ctx)
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
	// Check if the self is defined or not && if the services mapping has at least 2 services
	self := s.config.Contract.Self
	if self == "" {
		s.logger.Error("missing self service in config file")
		return fmt.Errorf("missing self service in config file")
	}
	fmt.Println("Self service is : ", self)

	servicesMapping := s.config.Contract.ServicesMapping
	fmt.Println("Services Mapping is : ", servicesMapping)
	// Check if the size of servicesMapping is less than 2
	if len(servicesMapping) < 2 {
		s.logger.Error("services mapping must contain at least 2 services")
		return fmt.Errorf("services mapping must contain at least 2 services")
	}

	// Check if the size of servicesMapping is more than 2 but doesn't contain the self service
	if len(servicesMapping) >= 2 {
		if _, exists := servicesMapping[self]; !exists {
			s.logger.Error("services mapping must contain the self service")
			return fmt.Errorf("services mapping must contain the self service")
		}
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
