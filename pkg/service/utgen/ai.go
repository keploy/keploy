package utgen

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type AICaller struct {
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

type ResponseChunk struct {
	Choices []Choice `json:"choices"`
}

type Choice struct {
	Delta struct {
		Content string `json:"content"`
	} `json:"delta"`
}

type ModelResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
}

func NewAICaller(model, apiBase string) *AICaller {
	return &AICaller{
		Model:   model,
		APIBase: apiBase,
	}
}

func (ai *AICaller) CallModel(prompt *Prompt, maxTokens int) (string, int, int, error) {
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

	if strings.Contains(ai.Model, "ollama") || strings.Contains(ai.Model, "huggingface") || strings.HasPrefix(ai.Model, "openai/") {
		completionParams.APIBase = ai.APIBase
	}

	requestBody, err := json.Marshal(completionParams)
	if err != nil {
		return "", 0, 0, err
	}

	response, err := http.Post(completionParams.APIBase+"/v1/engines/"+ai.Model+"/completions", "application/json", bytes.NewBuffer(requestBody))
	if err != nil {
		return "", 0, 0, err
	}
	defer response.Body.Close()

	var chunks []ResponseChunk
	decoder := json.NewDecoder(response.Body)
	fmt.Println("Streaming results from LLM model...")

	for {
		var chunk ResponseChunk
		if err := decoder.Decode(&chunk); err == io.EOF {
			break
		} else if err != nil {
			fmt.Printf("Error during streaming: %v\n", err)
			break
		}
		fmt.Print(chunk.Choices[0].Delta.Content)
		chunks = append(chunks, chunk)
		time.Sleep(10 * time.Millisecond) // Optional: Delay to simulate more 'natural' response pacing
	}
	fmt.Println()

	modelResponse, err := ai.streamChunkBuilder(chunks, messages)
	if err != nil {
		return "", 0, 0, err
	}

	return modelResponse.Choices[0].Message.Content, modelResponse.Usage.PromptTokens, modelResponse.Usage.CompletionTokens, nil
}

func (ai *AICaller) streamChunkBuilder(chunks []ResponseChunk, messages []Message) (*ModelResponse, error) {
	var content strings.Builder
	for _, chunk := range chunks {
		content.WriteString(chunk.Choices[0].Delta.Content)
	}

	modelResponse := ModelResponse{
		Choices: []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		}{
			{
				Message: struct {
					Content string `json:"content"`
				}{
					Content: content.String(),
				},
			},
		},
		Usage: struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		}{
			PromptTokens:     len(messages[0].Content) + len(messages[1].Content),
			CompletionTokens: content.Len(),
		},
	}

	return &modelResponse, nil
}
