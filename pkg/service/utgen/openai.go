package utgen

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	openai "github.com/openai/openai-go"
	"github.com/openai/openai-go/azure"
	"github.com/openai/openai-go/option"
	"go.uber.org/zap"

	"go.keploy.io/server/v2/config"
	"go.keploy.io/server/v2/pkg/service"
)

const (
	defaultOpenAIModel     = openai.ChatModelGPT4o
	defaultOpenAIMaxTokens = 4096
)

type OpenAIClient struct {
	model             string
	client            *openai.Client
	apiBase           string
	apiKey            string
	apiVersion        string
	apiServerURL      string
	auth              service.Auth
	logger            *zap.Logger
	functionUnderTest string
	sessionID         string
}

type OpenAIResponse struct {
	IsSuccess        bool
	Error            string
	FinalContent     string
	PromptTokens     int
	CompletionTokens int
	APIKey           string
}

func NewOpenAIClient(
	genConfig config.UtGen,
	apiKey, apiServerURL string,
	auth service.Auth,
	sessionID string,
	logger *zap.Logger,
) *OpenAIClient {
	model := genConfig.Model
	if model == "" {
		model = defaultOpenAIModel
	}

	var client openai.Client

	switch {
	case genConfig.APIBaseURL != "" && genConfig.APIVersion != "":
		client = openai.NewClient(
			azure.WithEndpoint(genConfig.APIBaseURL, genConfig.APIVersion),
			azure.WithAPIKey(apiKey),
		)
	case genConfig.APIBaseURL != "":
		client = openai.NewClient(
			option.WithBaseURL(genConfig.APIBaseURL),
			option.WithAPIKey(apiKey),
		)
	default:
		client = openai.NewClient(option.WithAPIKey(apiKey))
	}

	return &OpenAIClient{
		model:             genConfig.Model,
		client:            &client,
		apiBase:           genConfig.APIBaseURL,
		apiKey:            apiKey,
		apiVersion:        genConfig.APIVersion,
		auth:              auth,
		logger:            logger,
		apiServerURL:      apiServerURL,
		sessionID:         sessionID,
		functionUnderTest: genConfig.FunctionUnderTest,
	}
}

func (c *OpenAIClient) Call(ctx context.Context, opts CallOptions) (string, error) {
	if c.apiBase == c.apiServerURL {
		return c.callServer(ctx, opts)
	}
	return c.callOpenAI(ctx, opts)
}

func (c *OpenAIClient) callServer(ctx context.Context, opts CallOptions) (string, error) {
	token, err := c.auth.GetToken(ctx)
	if err != nil {
		return "", fmt.Errorf("error getting token: %v", err)
	}

	aiReq := struct {
		MaxTokens      int         `json:"maxTokens"`
		Prompt         Prompt      `json:"prompt"`
		SessionID      string      `json:"sessionId"`
		Iteration      int         `json:"iteration"`
		RequestPurpose PurposeType `json:"requestPurpose"`
	}{
		MaxTokens:      opts.MaxTokens,
		Prompt:         opts.Prompt,
		SessionID:      opts.SessionID,
		Iteration:      opts.Iteration,
		RequestPurpose: opts.RequestPurpose,
	}

	c.logger.Debug(
		"Making AI request to API server",
		zap.String("api_server_url", c.apiServerURL),
		zap.String("token", token),
	)
	body, err := json.Marshal(aiReq)
	if err != nil {
		return "", fmt.Errorf("error marshalling AI request: %v", err)
	}

	req, err := http.NewRequest(
		"POST",
		fmt.Sprintf("%s/ai/call", c.apiServerURL),
		bytes.NewBuffer(body),
	)
	if err != nil {
		return "", fmt.Errorf("error creating request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("error making request: %v", err)
	}
	defer resp.Body.Close()

	var aiResp OpenAIResponse
	if err := json.NewDecoder(resp.Body).Decode(&aiResp); err != nil {
		return "", fmt.Errorf("failed to decode response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf(
			"unexpected status code: %v, error: %s",
			resp.StatusCode,
			aiResp.Error,
		)
	}
	return aiResp.FinalContent, nil
}

func (c *OpenAIClient) callOpenAI(ctx context.Context, opts CallOptions) (string, error) {
	messages := []openai.ChatCompletionMessageParamUnion{
		openai.SystemMessage(opts.Prompt.System),
		openai.UserMessage(opts.Prompt.User),
	}

	maxTokens := opts.MaxTokens
	if maxTokens == 0 {
		maxTokens = defaultOpenAIMaxTokens
	}

	params := openai.ChatCompletionNewParams{
		Model:               c.model,
		Messages:            messages,
		MaxCompletionTokens: openai.Int(int64(maxTokens)),
	}

	if opts.Stream {
		return c.stream(ctx, params)
	}

	resp, err := c.client.Chat.Completions.New(ctx, params)
	if err != nil {
		c.logger.Error("failed to call OpenAI API", zap.Error(err))
		return "", fmt.Errorf("failed to call OpenAI API: %w", err)
	}
	if len(resp.Choices) == 0 {
		c.logger.Error("no choices returned from OpenAI/Azure API")
		return "", fmt.Errorf("no choices returned from OpenAI/Azure API")
	}

	return resp.Choices[0].Message.Content, nil
}

func (c *OpenAIClient) stream(
	ctx context.Context,
	params openai.ChatCompletionNewParams,
) (string, error) {
	var content strings.Builder
	stream := c.client.Chat.Completions.NewStreaming(ctx, params)
	defer stream.Close()

	for stream.Next() {
		evt := stream.Current()
		if len(evt.Choices) > 0 {
			content.WriteString(evt.Choices[0].Delta.Content)
		}
	}
	if err := stream.Err(); err != nil {
		return "", fmt.Errorf("streaming error: %w", err)
	}
	return content.String(), nil
}

func (c *OpenAIClient) SendCoverageUpdate(
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

func (ai *OpenAIClient) GetFunctionUnderTest() string {
	return ai.functionUnderTest
}

func (ai *OpenAIClient) GetSessionID() string {
	return ai.sessionID
}
