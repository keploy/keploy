package utgen

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/google/generative-ai-go/genai"
	"go.uber.org/zap"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"

	"go.keploy.io/server/v2/config"
	"go.keploy.io/server/v2/pkg/service"
)

const (
	defaultGeminiModel     = "gemini-1.5-flash"
	defaultGeminiMaxTokens = 1000000
)

type GeminiClient struct {
	model             string
	client            *genai.Client
	apiKey            string
	apiBase           string
	apiServerURL      string
	auth              service.Auth
	logger            *zap.Logger
	functionUnderTest string
	sessionID         string
}

func NewGeminiClient(
	genConfig config.UtGen,
	apiKey, apiServerURL string,
	auth service.Auth,
	sessionID string,
	logger *zap.Logger,
) *GeminiClient {
	model := genConfig.Model
	if model == "" || model == "gpt-4o" {
		model = defaultGeminiModel
	}
	if apiKey == "" {
		apiKey = os.Getenv("API_KEY")
	}

	ctx := context.Background()

	options := []option.ClientOption{option.WithAPIKey(apiKey)}
	if genConfig.APIBaseURL != "" {
		options = append(options, option.WithEndpoint(genConfig.APIBaseURL))
	}

	client, err := genai.NewClient(ctx, option.WithAPIKey(apiKey))
	if err != nil {
		logger.Error("error creating google gemini ai client. calls will fail", zap.Error(err))
	}

	return &GeminiClient{
		model:             genConfig.Model,
		client:            client,
		apiKey:            apiKey,
		apiBase:           genConfig.APIBaseURL,
		apiServerURL:      apiServerURL,
		logger:            logger,
		auth:              auth,
		sessionID:         sessionID,
		functionUnderTest: genConfig.FunctionUnderTest,
	}
}

func (c *GeminiClient) Call(
	ctx context.Context,
	opts CallOptions,
) (string, error) {
	if c.client == nil {
		return "", errors.New("gemini client is not initialized")
	}

	maxTokens := opts.MaxTokens
	if opts.MaxTokens == 0 {
		opts.MaxTokens = defaultGeminiMaxTokens
	}

	model := c.client.GenerativeModel(c.model)
	model.SetMaxOutputTokens(int32(maxTokens))

	if opts.Prompt.System != "" {
		model.SystemInstruction = &genai.Content{
			Role:  "model",
			Parts: []genai.Part{genai.Text(opts.Prompt.System)},
		}
	}

	parts := []genai.Part{genai.Text(opts.Prompt.User)}
	if opts.Prompt.User == "" {
		return "", fmt.Errorf("no prompt or messages provided")
	}

	if opts.Stream {
		return c.stream(ctx, model, parts)
	}

	resp, err := model.GenerateContent(ctx, parts...)
	if err != nil {
		c.logger.Error("failed to call Gemini API", zap.Error(err))
		return "", fmt.Errorf("failed to call Gemini API: %w", err)
	}
	if len(resp.Candidates) == 0 || len(resp.Candidates[0].Content.Parts) == 0 {
		return "", fmt.Errorf("no content returned from Gemini API")
	}
	return fmt.Sprint(resp.Candidates[0].Content.Parts[0]), nil
}

func (c *GeminiClient) stream(
	ctx context.Context,
	model *genai.GenerativeModel,
	parts []genai.Part,
) (string, error) {
	var content strings.Builder
	iter := model.GenerateContentStream(ctx, parts...)

	for {
		resp, err := iter.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			return "", fmt.Errorf("streaming error: %w", err)
		}
		if len(resp.Candidates) > 0 && len(resp.Candidates[0].Content.Parts) > 0 {
			content.WriteString(fmt.Sprint(resp.Candidates[0].Content.Parts[0]))
		}
	}
	return content.String(), nil
}

func (c *GeminiClient) SendCoverageUpdate(
	ctx context.Context,
	sessionID string,
	oldCoverage, newCoverage float64,
	iterationCount int,
) error {
	return sendCoverageUpdate(
		ctx,
		c.auth,
		c.apiBase,
		c.logger,
		sessionID,
		oldCoverage,
		newCoverage,
		iterationCount,
		c.functionUnderTest,
	)
}

func (c *GeminiClient) GetFunctionUnderTest() string {
	return c.functionUnderTest
}

func (c *GeminiClient) GetSessionID() string {
	return c.sessionID
}
