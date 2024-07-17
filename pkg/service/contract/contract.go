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

func (s *contractService) Generate(ctx context.Context, genTests bool) error {
	fmt.Println("HELLO IN CONTRACT SERVICE")
	if s.CheckConfigFile() != nil {
		s.logger.Error("Error in checking config file while generating")
		return fmt.Errorf("Error in checking config file while generating")
	}
	err := yaml.GenerateHelper(ctx, s.logger, s.config.Contract.Services, s.config.Contract.Self, genTests)
	if err != nil {
		s.logger.Error("Error in generating contract", zap.Error(err))
		return fmt.Errorf("Error in generating contract")
	}
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
	servicesMapping := s.config.Contract.ServicesMapping
	fmt.Println("Services Mapping is : ", servicesMapping)
	// Check if the size of servicesMapping is less than 1
	if len(servicesMapping) < 1 {
		s.logger.Error("services mapping must contain at least 2 services")
		return fmt.Errorf("services mapping must contain at least 2 services")
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
