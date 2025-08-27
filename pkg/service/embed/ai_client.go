package embed

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/google/uuid"
	"go.keploy.io/server/v2/config"
	"go.keploy.io/server/v2/pkg/service"
	"go.uber.org/zap"
	"gopkg.in/yaml.v2"
)

type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type Choice struct {
	Index        int     `json:"index"`
	FinishReason string  `json:"finish_reason"`
	Message      Message `json:"message"`
}

type CompletionResponse struct {
	Choices []Choice `json:"choices"`
}

type KeployAIRequest struct {
	Prompt    Prompt `json:"prompt"`
	SessionID string `json:"sessionId"`
}

type KeployAIResponse struct {
	IsSuccess    bool   `json:"isSuccess"`
	Error        string `json:"error"`
	FinalContent string `json:"finalContent"`
}

type AIClient struct {
	cfg    *config.Config
	logger *zap.Logger
	apiKey string
	Auth   service.Auth
}

func NewAIClient(cfg *config.Config, logger *zap.Logger, auth service.Auth) (*AIClient, error) {
	apiKey := os.Getenv("API_KEY")
	if apiKey == "" {
		return nil, fmt.Errorf("api key not found; set API_KEY environment variable")
	}

	return &AIClient{
		cfg:    cfg,
		logger: logger,
		apiKey: apiKey,
		Auth:   auth,
	}, nil
}

func (ac *AIClient) GenerateResponse(ctx context.Context, prompt *Prompt) (string, error) {
	apiBaseURL := "https://api.openai.com/v1"
	if ac.cfg.Embed.LLMBaseURL != "" {
		apiBaseURL = ac.cfg.Embed.LLMBaseURL
	}

	if strings.Contains(apiBaseURL, "keploy.io") {
		token, err := ac.getKeployToken(ctx)
		if err != nil {
			return "", fmt.Errorf("error getting Keploy token: %v", err)
		}

		ac.logger.Debug("Making AI request to Keploy API server", zap.String("api_server_url", apiBaseURL))

		aiRequest := KeployAIRequest{
			Prompt:    *prompt,
			SessionID: uuid.NewString(),
		}

		aiRequestBytes, err := json.Marshal(aiRequest)
		if err != nil {
			return "", fmt.Errorf("error marshalling Keploy AI request: %v", err)
		}

		req, err := http.NewRequestWithContext(ctx, "POST", apiBaseURL+"/ai/call", bytes.NewBuffer(aiRequestBytes))
		if err != nil {
			return "", fmt.Errorf("error creating request for Keploy AI: %v", err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)

		client := &http.Client{}
		resp, err := client.Do(req)
		if err != nil {
			return "", fmt.Errorf("error making request to Keploy AI: %v", err)
		}
		defer resp.Body.Close()

		bodyBytes, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK {
			return "", fmt.Errorf("unexpected status code from Keploy AI: %v, response body: %s", resp.StatusCode, string(bodyBytes))
		}

		var aiResponse KeployAIResponse
		err = json.Unmarshal(bodyBytes, &aiResponse)
		if err != nil {
			return "", fmt.Errorf("error unmarshalling Keploy AI response body: %v", err)
		}

		if !aiResponse.IsSuccess {
			return "", fmt.Errorf("keploy AI service returned an error: %s", aiResponse.Error)
		}

		return ac.unmarshalYAML(aiResponse.FinalContent)
	}

	model := ac.cfg.Embed.Model
	if model == "" {
		model = "gpt-4o"
	}

	messages := []Message{
		{Role: "system", Content: prompt.System},
		{Role: "user", Content: prompt.User},
	}

	requestBody, err := json.Marshal(map[string]interface{}{
		"model":       model,
		"messages":    messages,
		"temperature": 0.7,
	})
	if err != nil {
		return "", fmt.Errorf("error marshalling request body: %v", err)
	}

	queryParams := ""
	if ac.cfg.Embed.LLMApiVersion != "" {
		queryParams = "?api-version=" + ac.cfg.Embed.LLMApiVersion
	}

	req, err := http.NewRequestWithContext(ctx, "POST", apiBaseURL+"/chat/completions"+queryParams, bytes.NewBuffer(requestBody))
	if err != nil {
		return "", fmt.Errorf("error creating request: %v", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+ac.apiKey)
	req.Header.Set("api-key", ac.apiKey)

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("error making request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		bodyString := string(bodyBytes)
		return "", fmt.Errorf("unexpected status code: %v, response body: %s", resp.StatusCode, bodyString)
	}

	responseData, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("error reading response: %v", err)
	}

	var completionResponse CompletionResponse
	err = json.Unmarshal(responseData, &completionResponse)
	if err != nil {
		return "", fmt.Errorf("error unmarshalling response: %v", err)
	}

	if len(completionResponse.Choices) == 0 {
		return "", fmt.Errorf("no choices returned from AI")
	}

	response := completionResponse.Choices[0].Message.Content
	return ac.unmarshalYAML(response)
}

func (ac *AIClient) getKeployToken(ctx context.Context) (string, error) {
	if ac.Auth == nil {
		return "", fmt.Errorf("auth is not configured for Keploy token retrieval")
	}
	return ac.Auth.GetToken(ctx)
}

func (ac *AIClient) unmarshalYAML(response string) (string, error) {
	var data map[string]interface{}
	err := yaml.Unmarshal([]byte(response), &data)
	if err != nil {
		ac.logger.Debug("Response is not a YAML, returning as is.", zap.String("response", response))
		return response, nil
	}

	return response, nil
}
