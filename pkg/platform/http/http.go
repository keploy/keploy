package http

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"

	"go.uber.org/zap"
)

// This is a general purpose HTTP client package that can be used to make HTTP requests to any server.

type HTTP struct {
	logger *zap.Logger
	lock   *sync.Mutex
	client *http.Client
}

func New(logger *zap.Logger, client *http.Client) *HTTP {
	if client == nil {
		client = &http.Client{}
	}
	return &HTTP{
		logger: logger,
		client: client,
	}
}

func (h *HTTP) GetLatestPlan(ctx context.Context, serverUrl, token string) (string, error) {
	h.logger.Info("Getting the latest plan", zap.String("serverUrl", serverUrl), zap.String("token", token))

	req, err := http.NewRequestWithContext(ctx, "GET", fmt.Sprintf("%s/subscription/plan", serverUrl), nil)
	if err != nil {
		h.logger.Error("failed to create request", zap.Error(err))
		return "", fmt.Errorf("failed to get plan")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := h.client.Do(req)
	if err != nil {
		h.logger.Error("http request failed", zap.Error(err))
		return "", fmt.Errorf("failed to get plan")
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		h.logger.Error("failed to read response body", zap.Error(err))
		return "", fmt.Errorf("failed to get plan")
	}

	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		h.logger.Error("failed to unmarshal plan response", zap.Error(err))
		return "", fmt.Errorf("failed to get plan")
	}

	if errMsg, ok := raw["error"].(string); ok && errMsg != "" {
		h.logger.Error("error from subscription/plan API", zap.String("api_error", errMsg))
		return "", fmt.Errorf("failed to get plan")
	}

	plan, ok := raw["plan"].(map[string]any)
	if !ok {
		h.logger.Error("plan field not found or invalid", zap.Any("raw", raw))
		return "", fmt.Errorf("plan not found")
	}

	planType, ok := plan["type"].(string)
	if !ok || planType == "" {
		h.logger.Error("plan type missing or not a string", zap.Any("plan", plan))
		return "", fmt.Errorf("plan not found")
	}

	return planType, nil
}
