package utgen

import (
	"context"

	"go.uber.org/zap"

	"go.keploy.io/server/v2/config"
	"go.keploy.io/server/v2/pkg/service"
)

type CallOptions struct {
	Prompt         Prompt
	SessionID      string
	Iteration      int
	RequestPurpose PurposeType
	MaxTokens      int
	Stream         bool
	Temperature    float32
	TopP           float32
}

type Prompt struct {
	System string `json:"system"`
	User   string `json:"user"`
}

type Message struct {
	Role    string
	Content string
}

type PurposeType string

const (
	// TestForFunction represents a purpose type where the request is to test a function.
	TestForFunction PurposeType = "TestForFunction"

	// TestForFile represents a purpose type where the request is to test a file.
	TestForFile PurposeType = "TestForFile"
)

type AIModelClient interface {
	Call(ctx context.Context, opts CallOptions) (string, error)
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
) AIModelClient {
	if genConfig.Provider == "gemini" {
		return NewGeminiClient(genConfig, apiKey, apiServerURL, auth, sessionID, logger)
	}
	return NewOpenAIClient(genConfig, apiKey, apiServerURL, auth, sessionID, logger)
}
