// Package auth defines methods for authenticating with GitHub and API keys.
package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

type apiKeyReq struct {
	InstallationID string `json:"installationID"`
	APIKey         string `json:"apikey"`
}

type apiKeyRes struct {
	IsValid  bool   `json:"isValid"`
	JwtToken string `json:"jwtToken"`
	Error    string `json:"error"`
}

// AuthenticateWithAPIKey authenticates using a Keploy API key by posting to the
// /auth/apikey endpoint. It returns the JWT token on success.
func AuthenticateWithAPIKey(ctx context.Context, apiServerURL, installationID, apiKey string, logger *zap.Logger) (string, error) {
	url := fmt.Sprintf("%s/auth/apikey", apiServerURL)

	reqBody := &apiKeyReq{
		InstallationID: installationID,
		APIKey:         apiKey,
	}
	reqJSON, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("failed to marshal API key auth request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewBuffer(reqJSON))
	if err != nil {
		return "", fmt.Errorf("failed to create API key auth request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{}
	res, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("API key auth request failed: %w", err)
	}
	defer func() {
		if cerr := res.Body.Close(); cerr != nil {
			utils.LogError(logger, cerr, "failed to close API key auth response body")
		}
	}()

	var respBody apiKeyRes
	if err := json.NewDecoder(res.Body).Decode(&respBody); err != nil {
		return "", fmt.Errorf("failed to decode API key auth response: %w", err)
	}

	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return "", fmt.Errorf("API key auth failed (HTTP %d): %s", res.StatusCode, respBody.Error)
	}

	if !respBody.IsValid || respBody.JwtToken == "" {
		errMsg := respBody.Error
		if errMsg == "" {
			errMsg = "invalid API key or empty JWT token"
		}
		return "", fmt.Errorf("API key auth failed: %s", errMsg)
	}

	return respBody.JwtToken, nil
}
