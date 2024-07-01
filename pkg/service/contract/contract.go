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
	fmt.Printf("Download contract for services: %v\n", s.config.Contract.Services)
	return nil
}
func (s *contractService) Validate(ctx context.Context) error {
	fmt.Printf("Validate contract for services: %v\n", s.config.Contract.Services)
	return nil
}

// New creates a new contractService
func New(logger *zap.Logger, config *config.Config) Service {
	return &contractService{
		logger: logger,
		config: config,
	}
}
