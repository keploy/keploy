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

	"go.uber.org/zap"

	"go.keploy.io/server/v2/config"
	"go.keploy.io/server/v2/pkg/service"
	"go.keploy.io/server/v2/utils"
)

type GeminiClient struct {
	Model             string
	APIKey            string
	APIBase           string
	APIServerURL      string
	Auth              service.Auth
	Logger            *zap.Logger
	FunctionUnderTest string
	SessionID         string
}

type GeminiRequest struct {
	Contents        []GeminiContent `json:"contents"`
	MaxOutputTokens int             `json:"maxOutputTokens,omitempty"`
	Temperature     float32         `json:"temperature,omitempty"`
}

type GeminiContent struct {
	Parts []GeminiPart `json:"parts"`
	Role  string       `json:"role,omitempty"` // "user" or "model" (system mapped to "model")
}

type GeminiPart struct {
	Text string `json:"text"`
}

type GeminiResponse struct {
	Candidates []GeminiCandidate `json:"candidates"`
}

type GeminiCandidate struct {
	Content GeminiContent `json:"content"`
}

func NewGeminiClient(
	genConfig config.UtGen,
	apiKey, apiServerURL string,
	auth service.Auth,
	sessionID string,
	logger *zap.Logger,
) *GeminiClient {
	return &GeminiClient{
		Model:             genConfig.Model,
		APIKey:            apiKey,
		APIBase:           genConfig.APIBaseURL,
		APIServerURL:      apiServerURL,
		Logger:            logger,
		Auth:              auth,
		SessionID:         sessionID,
		FunctionUnderTest: genConfig.FunctionUnderTest,
	}
}

func (g *GeminiClient) Call(
	ctx context.Context,
	completionParams CompletionParams,
	request AIRequest,
	stream bool,
) (string, error) {
	var apiBaseURL string
	if g.APIBase != "" {
		apiBaseURL = g.APIBase
	} else {
		apiBaseURL = "https://generativelanguage.googleapis.com/v1beta"
	}

	var model string
	if completionParams.Model != "" {
		model = completionParams.Model
	} else if g.Model != "" {
		// TODO: Find a way to set default model for each provider
		if g.Model == "gpt-4o" {
			model = "gemini-1.5-flash"
		} else {
			model = g.Model
		}
	}

	geminiReq := GeminiRequest{
		MaxOutputTokens: completionParams.MaxTokens,
		Temperature:     float32(completionParams.Temperature),
	}

	if len(completionParams.Messages) > 0 {
		for _, msg := range completionParams.Messages {
			role := msg.Role
			if role == "system" {
				// mapping system to model for gemini
				role = "model"
			}
			geminiReq.Contents = append(geminiReq.Contents, GeminiContent{
				Parts: []GeminiPart{{Text: msg.Content}},
				Role:  role,
			})
		}
	} else if request.Prompt.User != "" {
		if request.Prompt.System == "" {
			geminiReq.Contents = []GeminiContent{
				{Parts: []GeminiPart{{Text: request.Prompt.User}}, Role: "user"},
			}
		} else {
			geminiReq.Contents = []GeminiContent{
				{Parts: []GeminiPart{{Text: request.Prompt.System}}, Role: "model"},
				{Parts: []GeminiPart{{Text: request.Prompt.User}}, Role: "user"},
			}
		}
	} else {
		return "", fmt.Errorf("no prompt or messages provided")
	}

	// Serialize request
	requestBody, err := json.Marshal(geminiReq)
	if err != nil {
		return "", fmt.Errorf("error marshalling request body: %v", err)
	}

	var apiKey string
	if g.APIKey == "" {
		apiKey = os.Getenv("API_KEY")
	} else {
		apiKey = g.APIKey
	}
	if apiKey == "" {
		return "", fmt.Errorf("API key is required")
	}

	endpoint := fmt.Sprintf("%s/models/%s:generateContent", apiBaseURL, model)
	if stream {
		endpoint = fmt.Sprintf(
			"%s/models/%s:streamGenerateContent?alt=sse&key=%s",
			apiBaseURL,
			model,
			apiKey,
		)
	} else {
		endpoint = fmt.Sprintf("%s?key=%s", endpoint, apiKey)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewBuffer(requestBody))
	if err != nil {
		return "", fmt.Errorf("error creating request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("error making request: %v", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			utils.LogError(g.Logger, err, "Error closing response body")
		}
	}()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf(
			"unexpected status code: %v, response body: %s", resp.StatusCode, string(bodyBytes),
		)
	}

	var contentBuilder strings.Builder
	reader := bufio.NewReader(resp.Body)

	if g.Logger.Level() == zap.DebugLevel {
		fmt.Println("Streaming results from LLM model...")
	}

	if stream {
		for {
			line, err := reader.ReadString('\n')
			if err != nil && err != io.EOF {
				utils.LogError(g.Logger, err, "Error reading stream")
				return "", err
			}
			line = strings.TrimSpace(strings.TrimSuffix(line, "data: "))
			if line == "" || err == io.EOF {
				break
			}

			var chunk GeminiResponse
			err = json.Unmarshal([]byte(line), &chunk)
			if err != nil {
				utils.LogError(g.Logger, err, "Error unmarshalling stream chunk")
				continue
			}

			if len(chunk.Candidates) > 0 && len(chunk.Candidates[0].Content.Parts) > 0 {
				contentBuilder.WriteString(chunk.Candidates[0].Content.Parts[0].Text)
				if g.Logger.Level() == zap.DebugLevel {
					fmt.Println(chunk.Candidates[0].Content.Parts[0].Text)
				}
			}
			time.Sleep(10 * time.Millisecond)
		}
	} else {
		responseData, err := io.ReadAll(resp.Body)
		if err != nil {
			return "", fmt.Errorf("error reading response: %v", err)
		}
		var response GeminiResponse
		err = json.Unmarshal(responseData, &response)
		if err != nil {
			return "", fmt.Errorf("error unmarshalling response: %v", err)
		}
		if len(response.Candidates) > 0 && len(response.Candidates[0].Content.Parts) > 0 {
			return response.Candidates[0].Content.Parts[0].Text, nil
		}
	}

	finalContent := contentBuilder.String()
	return finalContent, nil
}

func (g *GeminiClient) SendCoverageUpdate(
	ctx context.Context,
	sessionID string,
	oldCoverage, newCoverage float64,
	iterationCount int,
) error {
	// Construct the request body with session ID, old coverage, and new coverage
	requestPurpose := TestForFile
	if len(g.FunctionUnderTest) > 0 {
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
	if g.APIBase != "" {
		apiBaseURL = g.APIBase
	}
	// Create a POST request
	req, err := http.NewRequestWithContext(
		ctx,
		"POST",
		apiBaseURL+"/ai/coverage/update",
		bytes.NewBuffer(requestBody),
	)
	if err != nil {
		return fmt.Errorf("error creating request: %v", err)
	}

	token, err := g.Auth.GetToken(ctx)
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
			utils.LogError(g.Logger, err, "Error closing response body")
		}
	}()

	// Check for successful response
	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		bodyString := string(bodyBytes)
		return fmt.Errorf(
			"unexpected status code: %v, response body: %s",
			resp.StatusCode,
			bodyString,
		)
	}

	g.Logger.Debug("Coverage update sent successfully", zap.String("session_id", sessionID))
	return nil
}

func (g *GeminiClient) GetFunctionUnderTest() string {
	return g.FunctionUnderTest
}

func (g *GeminiClient) GetSessionID() string {
	return g.SessionID
}
