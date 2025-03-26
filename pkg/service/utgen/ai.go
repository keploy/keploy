package utgen

import (
	"context"

	"go.uber.org/zap"

	"go.keploy.io/server/v2/config"
	"go.keploy.io/server/v2/pkg/service"
)

type AIClientInterface interface {
	Call(
		ctx context.Context,
		config CompletionParams,
		request AIRequest,
		stream bool,
	) (string, error)
	SendCoverageUpdate(
		ctx context.Context,
		sessionID string,
		oldCoverage, newCoverage float64,
		iterationCount int,
	) error
	GetFunctionUnderTest() string
	GetSessionID() string
}

func NewAIClient(
	genConfig config.UtGen,
	apiKey, apiServerURL string,
	auth service.Auth,
	sessionID string,
	logger *zap.Logger,
) AIClientInterface {
	if genConfig.Provider == "gemini" {
		return NewGeminiClient(genConfig, apiKey, apiServerURL, auth, sessionID, logger)
	}
	return NewOpenAIClient(genConfig, apiKey, apiServerURL, auth, sessionID, logger)
}
