package load

import (
	"go.keploy.io/server/v2/config"
	"go.uber.org/zap"
)

type ThresholdEvaluator struct {
	config *config.Config
	logger *zap.Logger
}

func NewThresholdEvaluator(cfg *config.Config, logger *zap.Logger) *ThresholdEvaluator {
	return &ThresholdEvaluator{
		config: cfg,
		logger: logger,
	}
}

func (te *ThresholdEvaluator) Evaluate(steps []StepMetrics) error {
	return nil
}
