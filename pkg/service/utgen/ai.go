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
	"strings"
	"time"
)

type AIClient struct {
	Model   string
	APIBase string
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
	APIBase     string    `json:"api_base,omitempty"`
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

func NewAIClient(model, apiBase string) *AIClient {
	return &AIClient{
		Model:   model,
		APIBase: apiBase,
	}
}

func (ai *AIClient) Call(ctx context.Context, prompt *Prompt, maxTokens int) (string, int, int, error) {

	var apiBaseURL string

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

	if ai.APIBase != "" {
		completionParams.APIBase = ai.APIBase
		apiBaseURL = ai.APIBase
	} else {
		apiBaseURL = "https://api.openai.com/"
	}

	requestBody, err := json.Marshal(completionParams)
	if err != nil {
		return "", 0, 0, fmt.Errorf("error marshalling request body: %v", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", apiBaseURL+"/v1/chat/completions", bytes.NewBuffer(requestBody))
	if err != nil {
		return "", 0, 0, fmt.Errorf("error creating request: %v", err)
	}

	apiKey := os.Getenv("API_KEY")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return "", 0, 0, fmt.Errorf("error making request: %v", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			fmt.Printf("Error closing response body: %v\n", err)
		}
	}()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		bodyString := string(bodyBytes)
		return "", 0, 0, fmt.Errorf("unexpected status code: %v, response body: %s", resp.StatusCode, bodyString)
	}

	var contentBuilder strings.Builder
	reader := bufio.NewReader(resp.Body)
	fmt.Println("Streaming results from LLM model...")

	for {
		line, err := reader.ReadString('\n')
		if err != nil && err != io.EOF {
			fmt.Printf("Error reading stream: %v\n", err)
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
			fmt.Printf("Error unmarshalling chunk: %v\nLine: %s\n", err, line)
			continue
		}

		if len(chunk.Choices) > 0 {
			if chunk.Choices[0].Delta != (Delta{}) {
				contentBuilder.WriteString(chunk.Choices[0].Delta.Content)
				fmt.Print(chunk.Choices[0].Delta.Content)
			}
		}

		if err == io.EOF {
			break
		}
		time.Sleep(10 * time.Millisecond) // Optional: Delay to simulate more 'natural' response pacing
	}
	fmt.Println()

	finalContent := contentBuilder.String()
	promptTokens := len(strings.Fields(prompt.System)) + len(strings.Fields(prompt.User))
	completionTokens := len(strings.Fields(finalContent))

	return finalContent, promptTokens, completionTokens, nil
}
