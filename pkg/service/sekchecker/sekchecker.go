package sekchecker

import (
	"context"

	"go.keploy.io/server/v2/config"
	"go.uber.org/zap"
)

type SekChecker struct {
	config *config.Config
	logger *zap.Logger
}

func NewSecurityChecker(cfg *config.Config, logger *zap.Logger) (*SekChecker, error) {
	logger.Info("Initializing Security Checker Service")

	return &SekChecker{
		config: cfg,
		logger: logger,
	}, nil
}

func (s *SekChecker) Start(ctx context.Context) error {
	return nil
}

func (s *SekChecker) AddCustomRule(ctx context.Context) error {
	return nil
}

func (s *SekChecker) RemoveCustomRule(ctx context.Context) error {
	return nil
}

func (s *SekChecker) UpdateCustomRule(ctx context.Context) error {
	return nil
}
