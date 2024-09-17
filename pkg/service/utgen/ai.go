package utgen

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"go.keploy.io/server/v2/pkg/service"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
)

type AIClient struct {
	Model        string
	APIBase      string
	APIVersion   string
	APIKey       string
	APIServerURL string
	Auth         service.Auth
	Logger       *zap.Logger
	SessionID    string
}

type Prompt struct {
	System string `json:"system"`
	User   string `json:"user"`
}

type CompletionParams struct {
	Model       string    `json:"model"`
	Messages    []Message `json:"messages"`
	MaxTokens   int       `json:"max_tokens"`
	Stream      bool      `json:"stream"`
	Temperature float32   `json:"temperature"`
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

type Choice struct {
	Delta Delta `json:"delta"`
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
	MaxTokens int    `json:"maxTokens"`
	Prompt    Prompt `json:"prompt"`
	SessionID string `json:"sessionId"`
}

func NewAIClient(model, apiBase, apiVersion, apiKey, apiServerURL string, auth service.Auth, sessionID string, logger *zap.Logger) *AIClient {
	return &AIClient{
		Model:        model,
		APIBase:      apiBase,
		APIVersion:   apiVersion,
		Logger:       logger,
		APIKey:       apiKey,
		APIServerURL: apiServerURL,
		Auth:         auth,
		SessionID:    sessionID,
	}
}

func (ai *AIClient) Call(ctx context.Context, prompt *Prompt, maxTokens int) (string, int, int, error) {

	var apiBaseURL string

	// Signal handling for Ctrl+C (SIGINT)
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigChan)

	// Create a cancellable context for the request
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Start a goroutine to listen for Ctrl+C and cancel the context
	go func() {
		select {
		case <-sigChan:
			fmt.Println("Received interrupt signal. Cancelling AI request...")
			cancel() // Cancel the context if Ctrl+C is pressed
		case <-ctx.Done():
			// Context already cancelled, do nothing
		}
	}()

	if prompt.System == "" && prompt.User == "" {
		return "", 0, 0, errors.New("the prompt must contain 'system' and 'user' keys")
	}

	var messages []Message
	if prompt.System == "" {
		messages = []Message{
			{Role: "user", Content: prompt.User},
		}
	} else {
		messages = []Message{
			{Role: "system", Content: prompt.System},
			{Role: "user", Content: prompt.User},
		}
	}

	completionParams := CompletionParams{
		Model:       ai.Model,
		Messages:    messages,
		MaxTokens:   maxTokens,
		Stream:      true,
		Temperature: 0.2,
	}

	var apiKey string
	if ai.APIBase == ai.APIServerURL {

		token, err := ai.Auth.GetToken(ctx)
		if err != nil {
			return "", 0, 0, fmt.Errorf("error getting token: %v", err)
		}

		ai.Logger.Debug("Making AI request to API server", zap.String("api_server_url", ai.APIServerURL), zap.String("token", token))
		httpClient := &http.Client{}
		// make AI request as request body to the API server
		aiRequest := AIRequest{
			MaxTokens: maxTokens,
			Prompt:    *prompt,
			SessionID: ai.SessionID,
		}
		aiRequestBytes, err := json.Marshal(aiRequest)
		if err != nil {
			return "", 0, 0, fmt.Errorf("error marshalling AI request: %v", err)
		}

		req, err := http.NewRequest("POST", fmt.Sprintf("%s/ai/call", ai.APIServerURL), bytes.NewBuffer(aiRequestBytes))
		if err != nil {
			return "", 0, 0, fmt.Errorf("error creating request: %v", err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)

		resp, err := httpClient.Do(req)
		if err != nil {
			return "", 0, 0, fmt.Errorf("error making request: %v", err)
		}

		// read the response body AIResponse
		bodyBytes, _ := io.ReadAll(resp.Body)
		var aiResponse AIResponse
		err = json.Unmarshal(bodyBytes, &aiResponse)
		if err != nil {
			return "", 0, 0, fmt.Errorf("error unmarshalling response body: %v", err)
		}

		if resp.StatusCode != http.StatusOK {
			return "", 0, 0, fmt.Errorf("unexpected status code: %v, response body: %s", resp.StatusCode, aiResponse.Error)
		}
		defer func() {
			err := resp.Body.Close()
			if err != nil {
				utils.LogError(ai.Logger, err, "failed to close response body for authentication")
			}
		}()

		return aiResponse.FinalContent, aiResponse.PromptTokens, aiResponse.CompletionTokens, nil
	} else if ai.APIBase != "" {
		apiBaseURL = ai.APIBase
	} else {
		apiBaseURL = "https://api.openai.com/v1"
	}

	requestBody, err := json.Marshal(completionParams)
	if err != nil {
		return "", 0, 0, fmt.Errorf("error marshalling request body: %v", err)
	}

	queryParams := ""
	if ai.APIVersion != "" {
		queryParams = "?api-version=" + ai.APIVersion
	}

	req, err := http.NewRequestWithContext(ctx, "POST", apiBaseURL+"/chat/completions"+queryParams, bytes.NewBuffer(requestBody))
	if err != nil {
		return "", 0, 0, fmt.Errorf("error creating request: %v", err)
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
		return "", 0, 0, fmt.Errorf("error making request: %v", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			utils.LogError(ai.Logger, err, "Error closing response body")
		}
	}()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		bodyString := string(bodyBytes)
		return "", 0, 0, fmt.Errorf("unexpected status code: %v, response body: %s", resp.StatusCode, bodyString)
	}

	var contentBuilder strings.Builder
	reader := bufio.NewReader(resp.Body)

	if ai.Logger.Level() == zap.DebugLevel {
		fmt.Println("Streaming results from LLM model...")
	}

	fmt.Println("Streaming results from LLM model...")

	for {
		select {
		case <-ctx.Done():
			fmt.Println("contect cancelling")
			return "", 0, 0, ctx.Err()
		default:

			line, err := reader.ReadString('\n')
			if err != nil && err != io.EOF {
				utils.LogError(ai.Logger, err, "Error reading stream")
				return "", 0, 0, err
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
	}
	if ai.Logger.Level() == zap.DebugLevel {
		fmt.Println()
	}

	finalContent := contentBuilder.String()
	promptTokens := len(strings.Fields(prompt.System)) + len(strings.Fields(prompt.User))
	completionTokens := len(strings.Fields(finalContent))

	return finalContent, promptTokens, completionTokens, nil
}
