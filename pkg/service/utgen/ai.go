package utgen

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"go.keploy.io/server/v2/config"
	"go.keploy.io/server/v2/pkg/service"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
)

type AIClient struct {
	Model             string
	APIBase           string
	APIVersion        string
	APIKey            string
	APIServerURL      string
	Auth              service.Auth
	Logger            *zap.Logger
	SessionID         string
	FunctionUnderTest string
}

type Prompt struct {
	System string `json:"system"`
	User   string `json:"user"`
}

type CompletionParams struct {
	Model               string    `json:"model"`
	Messages            []Message `json:"messages"`
	MaxTokens           int       `json:"max_tokens,omitempty"`
	MaxCompletionTokens int       `json:"max_completion_tokens,omitempty"`
	Stream              *bool     `json:"stream,omitempty"`
	Temperature         float32   `json:"temperature,omitempty"`
}

type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type ModelResponse struct {
	ID                string   `json:"id"`
	Choices           []Choice `json:"choices"`
	Created           int      `json:"created"`
	Model             string   `json:"model,omitempty"`
	Object            string   `json:"object"`
	SystemFingerprint string   `json:"system_fingerprint,omitempty"`
	Usage             *Usage   `json:"usage,omitempty"`
}

type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

type ResponseChunk struct {
	Choices []Choice `json:"choices"`
}

type Delta struct {
	Content string `json:"content"`
}

type AIResponse struct {
	IsSuccess        bool   `json:"isSuccess"`
	Error            string `json:"error"`
	FinalContent     string `json:"finalContent"`
	PromptTokens     int    `json:"promptTokens"`
	CompletionTokens int    `json:"completionTokens"`
	APIKey           string `json:"apiKey"`
}

type AIRequest struct {
	MaxTokens      int         `json:"maxTokens"`
	Prompt         Prompt      `json:"prompt"`
	SessionID      string      `json:"sessionId"`
	Iteration      int         `json:"iteration"`
	RequestPurpose PurposeType `json:"requestPurpose"`
}

// PurposeType defines the type of purpose for the AI request.
type PurposeType string

const (
	// TestForFunction represents a purpose type where the request is to test a function.
	TestForFunction PurposeType = "TestForFunction"

	// TestForFile represents a purpose type where the request is to test a file.
	TestForFile PurposeType = "TestForFile"
)

type CompletionResponse struct {
	ID      string    `json:"id"`
	Object  string    `json:"object"`
	Created int64     `json:"created"`
	Model   string    `json:"model"`
	Choices []Choice  `json:"choices"`
	Usage   UsageData `json:"usage"`
}

type Choice struct {
	Index        int     `json:"index"`
	FinishReason string  `json:"finish_reason"`
	Message      Message `json:"message"`
	Delta        Delta   `json:"delta"`
}

type UsageData struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

func NewAIClient(genConfig config.UtGen, apiKey, apiServerURL string, auth service.Auth, sessionID string, logger *zap.Logger) *AIClient {
	return &AIClient{
		Model:             genConfig.Model,
		APIBase:           genConfig.APIBaseURL,
		APIVersion:        genConfig.APIVersion,
		Logger:            logger,
		APIKey:            apiKey,
		APIServerURL:      apiServerURL,
		Auth:              auth,
		SessionID:         sessionID,
		FunctionUnderTest: genConfig.FunctionUnderTest,
	}
}

func (ai *AIClient) Call(ctx context.Context, completionParams CompletionParams, aiRequest AIRequest, stream bool) (string, error) {

	var apiBaseURL string

	var apiKey string
	if ai.APIBase == ai.APIServerURL {

		token, err := ai.Auth.GetToken(ctx)
		if err != nil {
			return "", fmt.Errorf("error getting token: %v", err)
		}

		ai.Logger.Debug("Making AI request to API server", zap.String("api_server_url", ai.APIServerURL), zap.String("token", token))
		httpClient := &http.Client{}
		aiRequestBytes, err := json.Marshal(aiRequest)
		if err != nil {
			return "", fmt.Errorf("error marshalling AI request: %v", err)
		}

		req, err := http.NewRequest("POST", fmt.Sprintf("%s/ai/call", ai.APIServerURL), bytes.NewBuffer(aiRequestBytes))
		if err != nil {
			return "", fmt.Errorf("error creating request: %v", err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)

		resp, err := httpClient.Do(req)
		if err != nil {
			return "", fmt.Errorf("error making request: %v", err)
		}

		bodyBytes, _ := io.ReadAll(resp.Body)
		var aiResponse AIResponse
		err = json.Unmarshal(bodyBytes, &aiResponse)
		if err != nil {
			return "", fmt.Errorf("error unmarshalling response body: %v", err)
		}

		if resp.StatusCode != http.StatusOK {
			return "", fmt.Errorf("unexpected status code: %v, response body: %s", resp.StatusCode, aiResponse.Error)
		}
		defer func() {
			err := resp.Body.Close()
			if err != nil {
				utils.LogError(ai.Logger, err, "failed to close response body for authentication")
			}
		}()

		return aiResponse.FinalContent, nil
	} else if ai.APIBase != "" {
		apiBaseURL = ai.APIBase
	} else {
		apiBaseURL = "https://api.openai.com/v1"
	}

	requestBody, err := json.Marshal(completionParams)
	if err != nil {
		return "", fmt.Errorf("error marshalling request body: %v", err)
	}

	queryParams := ""
	if ai.APIVersion != "" {
		queryParams = "?api-version=" + ai.APIVersion
	}

	req, err := http.NewRequestWithContext(ctx, "POST", apiBaseURL+"/chat/completions"+queryParams, bytes.NewBuffer(requestBody))
	if err != nil {
		return "", fmt.Errorf("error creating request: %v", err)
	}

	if ai.APIKey == "" {
		apiKey = os.Getenv("API_KEY")
	} else {
		apiKey = ai.APIKey
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("api-key", apiKey)

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("error making request: %v", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			utils.LogError(ai.Logger, err, "Error closing response body")
		}
	}()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		bodyString := string(bodyBytes)
		return "", fmt.Errorf("unexpected status code: %v, response body: %s", resp.StatusCode, bodyString)
	}

	var contentBuilder strings.Builder
	reader := bufio.NewReader(resp.Body)

	if ai.Logger.Level() == zap.DebugLevel {
		fmt.Println("Streaming results from LLM model...")
	}

	fmt.Println("Streaming results from LLM model...")
	if stream {
		for {
			line, err := reader.ReadString('\n')
			if err != nil && err != io.EOF {
				utils.LogError(ai.Logger, err, "Error reading stream")
				return "", err
			}
			line = strings.TrimSpace(strings.TrimPrefix(line, "data: "))
			if line == "[DONE]" {
				break
			}

			if line == "" {
				continue
			}

			var chunk ModelResponse
			err = json.Unmarshal([]byte(line), &chunk)
			if err != nil {
				utils.LogError(ai.Logger, err, "Error unmarshalling chunk")
				continue
			}

			if len(chunk.Choices) > 0 {
				if chunk.Choices[0].Delta != (Delta{}) {
					contentBuilder.WriteString(chunk.Choices[0].Delta.Content)
					if ai.Logger.Level() == zap.DebugLevel {
						fmt.Print(chunk.Choices[0].Delta.Content)
					}
				}
			}

			if err == io.EOF {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
	} else {
		responseData, err := io.ReadAll(resp.Body)
		if err != nil {
			return "", fmt.Errorf("error reading response: %v", err)
		}
		var completionResponse CompletionResponse
		err = json.Unmarshal(responseData, &completionResponse)
		if err != nil {
			return "", fmt.Errorf("error unmarshalling response: %v", err)
		}
		if len(completionResponse.Choices) > 0 {
			finalContent := completionResponse.Choices[0].Message.Content
			return finalContent, nil
		}
	}
	finalContent := contentBuilder.String()

	return finalContent, nil
}

func (ai *AIClient) SendCoverageUpdate(ctx context.Context, sessionID string, oldCoverage, newCoverage float64, iterationCount int) error {
	// Construct the request body with session ID, old coverage, and new coverage
	requestPurpose := TestForFile
	if len(ai.FunctionUnderTest) > 0 {
		requestPurpose = TestForFunction
	}
	requestBody, err := json.Marshal(map[string]interface{}{
		"sessionId":      sessionID,
		"initalCoverage": oldCoverage,
		"finalCoverage":  newCoverage,
		"iteration":      iterationCount,
		"requestPurpose": requestPurpose,
	})
	if err != nil {
		return fmt.Errorf("error marshalling request body: %v", err)
	}

	// Determine the base URL
	var apiBaseURL string
	if ai.APIBase != "" {
		apiBaseURL = ai.APIBase
	}
	// Create a POST request
	req, err := http.NewRequestWithContext(ctx, "POST", apiBaseURL+"/ai/coverage/update", bytes.NewBuffer(requestBody))
	if err != nil {
		return fmt.Errorf("error creating request: %v", err)
	}

	token, err := ai.Auth.GetToken(ctx)

	if err != nil {
		return fmt.Errorf("error getting token: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	// Execute the request
	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("error making request: %v", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			utils.LogError(ai.Logger, err, "Error closing response body")
		}
	}()

	// Check for successful response
	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		bodyString := string(bodyBytes)
		return fmt.Errorf("unexpected status code: %v, response body: %s", resp.StatusCode, bodyString)
	}

	ai.Logger.Debug("Coverage update sent successfully", zap.String("session_id", sessionID))
	return nil
}
