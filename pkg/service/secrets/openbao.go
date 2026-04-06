package secrets

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"go.uber.org/zap"
)

// OpenBaoAPIError is returned when the OpenBao API responds with an error status.
type OpenBaoAPIError struct {
	StatusCode int
	Body       string
}

func (e *OpenBaoAPIError) Error() string {
	return fmt.Sprintf("OpenBao API error %d: %s", e.StatusCode, e.Body)
}

// OpenBaoClient communicates with a Keploy-hosted OpenBao instance for
// transit encryption key management. Uses raw HTTP (no heavy SDK dependency).
type OpenBaoClient struct {
	httpClient *http.Client
	baseURL    string // e.g. "https://openbao.keploy.io"
	token      string // auth token (derived from API key)
	logger     *zap.Logger
}

// NewOpenBaoClient creates a client for the OpenBao transit engine.
func NewOpenBaoClient(baseURL, token string, logger *zap.Logger) *OpenBaoClient {
	return &OpenBaoClient{
		httpClient: &http.Client{Timeout: 10 * time.Second},
		baseURL:    baseURL,
		token:      token,
		logger:     logger,
	}
}

// transitKeyName returns the transit key path for an app.
func transitKeyName(appID string) string {
	return "keploy-apps-" + appID
}

// EnsureTransitKey creates the transit encryption key if it doesn't exist.
// Returns nil on success or if the key already exists (HTTP 204/409).
// Returns an error for genuine failures (permission denied, network errors, etc.).
func (c *OpenBaoClient) EnsureTransitKey(ctx context.Context, appID string) error {
	path := fmt.Sprintf("/v1/transit/keys/%s", transitKeyName(appID))
	body := map[string]interface{}{
		"type": "aes256-gcm96",
	}
	_, err := c.doRequest(ctx, http.MethodPost, path, body)
	if err != nil {
		// Key already exists → 409 Conflict. This is expected and not an error.
		var apiErr *OpenBaoAPIError
		if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusConflict {
			if c.logger != nil {
				c.logger.Debug("transit key already exists", zap.String("appID", appID))
			}
			return nil
		}
		return fmt.Errorf("failed to create transit key for app %q: %w", appID, err)
	}
	return nil
}

// GenerateDataKey generates a new data encryption key (DEK) using the transit engine.
// Returns the plaintext DEK (32 bytes for AES-256) and the OpenBao-wrapped version.
func (c *OpenBaoClient) GenerateDataKey(ctx context.Context, appID string) (plaintext []byte, wrappedKey string, keyVersion int, err error) {
	path := fmt.Sprintf("/v1/transit/datakey/plaintext/%s", transitKeyName(appID))
	body := map[string]interface{}{
		"bits": 256,
	}
	resp, err := c.doRequest(ctx, http.MethodPost, path, body)
	if err != nil {
		return nil, "", 0, fmt.Errorf("GenerateDataKey failed: %w", err)
	}
	if resp == nil {
		return nil, "", 0, fmt.Errorf("GenerateDataKey: empty response from OpenBao")
	}

	data, ok := resp["data"].(map[string]interface{})
	if !ok {
		return nil, "", 0, fmt.Errorf("GenerateDataKey: missing 'data' in response")
	}

	plaintextB64, ok := data["plaintext"].(string)
	if !ok || plaintextB64 == "" {
		return nil, "", 0, fmt.Errorf("GenerateDataKey: missing or invalid 'plaintext' in response")
	}
	wrappedKey, ok = data["ciphertext"].(string)
	if !ok || wrappedKey == "" {
		return nil, "", 0, fmt.Errorf("GenerateDataKey: missing or invalid 'ciphertext' in response")
	}
	keyVersionF, ok := data["key_version"].(float64)
	if !ok {
		return nil, "", 0, fmt.Errorf("GenerateDataKey: missing or invalid 'key_version' in response")
	}
	keyVersion = int(keyVersionF)

	plaintext, err = base64.StdEncoding.DecodeString(plaintextB64)
	if err != nil {
		return nil, "", 0, fmt.Errorf("failed to decode plaintext DEK: %w", err)
	}

	return plaintext, wrappedKey, keyVersion, nil
}

// DecryptDataKey unwraps a previously wrapped data encryption key.
func (c *OpenBaoClient) DecryptDataKey(ctx context.Context, appID string, wrappedKey string) ([]byte, error) {
	path := fmt.Sprintf("/v1/transit/decrypt/%s", transitKeyName(appID))
	body := map[string]interface{}{
		"ciphertext": wrappedKey,
	}
	resp, err := c.doRequest(ctx, http.MethodPost, path, body)
	if err != nil {
		return nil, fmt.Errorf("DecryptDataKey failed: %w", err)
	}
	if resp == nil {
		return nil, fmt.Errorf("DecryptDataKey: empty response from OpenBao")
	}

	data, ok := resp["data"].(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("DecryptDataKey: missing 'data' in response")
	}

	plaintextB64, ok := data["plaintext"].(string)
	if !ok || plaintextB64 == "" {
		return nil, fmt.Errorf("DecryptDataKey: missing or invalid 'plaintext' in response")
	}
	plaintext, err := base64.StdEncoding.DecodeString(plaintextB64)
	if err != nil {
		return nil, fmt.Errorf("failed to decode plaintext DEK: %w", err)
	}

	return plaintext, nil
}

// RotateKey triggers key rotation for the app's transit key.
// Old key versions are retained for decrypting previously encrypted data.
func (c *OpenBaoClient) RotateKey(ctx context.Context, appID string) (int, error) {
	path := fmt.Sprintf("/v1/transit/keys/%s/rotate", transitKeyName(appID))
	_, err := c.doRequest(ctx, http.MethodPost, path, nil)
	if err != nil {
		return 0, fmt.Errorf("RotateKey failed: %w", err)
	}

	// Read key info to get the new version.
	infoPath := fmt.Sprintf("/v1/transit/keys/%s", transitKeyName(appID))
	resp, err := c.doRequest(ctx, http.MethodGet, infoPath, nil)
	if err != nil {
		return 0, fmt.Errorf("failed to read key info after rotation: %w", err)
	}
	if resp == nil {
		return 0, fmt.Errorf("RotateKey: empty response when reading key info")
	}

	data, ok := resp["data"].(map[string]interface{})
	if !ok {
		return 0, fmt.Errorf("RotateKey: missing 'data' in key info response")
	}
	latestVersion, ok := data["latest_version"].(float64)
	if !ok {
		return 0, fmt.Errorf("RotateKey: missing or invalid 'latest_version' in key info response")
	}
	return int(latestVersion), nil
}

func (c *OpenBaoClient) doRequest(ctx context.Context, method, path string, body interface{}) (map[string]interface{}, error) {
	var reqBody io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reqBody = bytes.NewReader(data)
	}

	url := c.baseURL + path
	req, err := http.NewRequestWithContext(ctx, method, url, reqBody)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Vault-Token", c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode >= 400 {
		return nil, &OpenBaoAPIError{StatusCode: resp.StatusCode, Body: string(respBody)}
	}

	if len(respBody) == 0 {
		return nil, nil
	}

	var result map[string]interface{}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}
	return result, nil
}
