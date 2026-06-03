package replay

// decrypt.go — runtime decryption of ENC: values at replay time.
//
// When a recording was made with encryption_protection.enabled=true, secrets
// are stored as ENC:<keyId>:<base64(nonce||ciphertext||tag)>. The replay
// engine must decrypt them to real values before sending requests to the
// application so that genuine assertions can be made.
//
// Decryption flow per test case:
//  1. Walk HTTPReq headers, URL params, and body for ENC: prefixed strings.
//  2. Extract keyId from each ENC: value.
//  3. Fetch the AES-256 key from api-server /internal/kms/key/{keyId} (cached).
//  4. AES-256-GCM decrypt → real value, replace in-place.
//
// Keys are cached in a per-run map to avoid one HTTP call per ENC: field.
// The cache lives only for the duration of one test set run.

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// encryptedPrefix is the marker prepended to every encrypted value.
const encryptedPrefix = "ENC:"

// isEncrypted returns true if value starts with "ENC:" (was encrypted by Encryptor).
func isEncrypted(value string) bool {
	return strings.HasPrefix(value, encryptedPrefix)
}

// parseKeyID extracts the keyId from an ENC:<keyId>:<b64> encoded string.
// Returns "" if the format is invalid.
func parseKeyID(encoded string) string {
	if !isEncrypted(encoded) {
		return ""
	}
	rest := encoded[len(encryptedPrefix):]
	idx := strings.Index(rest, ":")
	if idx < 0 {
		return ""
	}
	return rest[:idx]
}

// decryptValue decrypts a single ENC:<keyId>:<b64> encoded string.
// key must be 32 bytes (AES-256).
func decryptValue(encoded string, key []byte) (string, error) {
	if !isEncrypted(encoded) {
		return encoded, nil
	}
	rest := encoded[len(encryptedPrefix):]
	idx := strings.Index(rest, ":")
	if idx < 0 {
		return "", fmt.Errorf("decrypt: malformed ENC value (missing keyId separator)")
	}
	b64data := rest[idx+1:]

	ciphertext, err := base64.StdEncoding.DecodeString(b64data)
	if err != nil {
		return "", fmt.Errorf("decrypt: base64 decode: %w", err)
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return "", fmt.Errorf("decrypt: AES cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("decrypt: GCM: %w", err)
	}

	nonceSize := gcm.NonceSize()
	if len(ciphertext) < nonceSize+gcm.Overhead() {
		return "", fmt.Errorf("decrypt: ciphertext too short")
	}
	nonce, data := ciphertext[:nonceSize], ciphertext[nonceSize:]
	plain, err := gcm.Open(nil, nonce, data, nil)
	if err != nil {
		return "", fmt.Errorf("decrypt: GCM open: %w", err)
	}
	return string(plain), nil
}

// kmsKeyCache caches fetched workspace keys so each keyId is fetched only once
// per test-set run. Not shared across runs — create a new instance per run.
type kmsKeyCache struct {
	mu      sync.Mutex
	keys    map[string][]byte // keyId → 32-byte AES-256 key
	apiURL  string            // api-server base URL (no trailing slash)
	token   string            // Bearer JWT token for /internal/kms/key/{id}
	httpCli *http.Client
}

func newKMSKeyCache(apiServerURL, token string) *kmsKeyCache {
	return &kmsKeyCache{
		keys:    make(map[string][]byte),
		apiURL:  strings.TrimRight(apiServerURL, "/"),
		token:   token,
		httpCli: &http.Client{Timeout: 10 * time.Second},
	}
}

// getKey returns the AES-256 key for keyId, fetching from api-server if needed.
func (c *kmsKeyCache) getKey(ctx context.Context, keyID string) ([]byte, error) {
	c.mu.Lock()
	if k, ok := c.keys[keyID]; ok {
		c.mu.Unlock()
		return k, nil
	}
	c.mu.Unlock()

	// Fetch from api-server /internal/kms/key/{keyId}.
	fetchURL := fmt.Sprintf("%s/internal/kms/key/%s", c.apiURL, keyID)
	reqCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, fetchURL, nil)
	if err != nil {
		return nil, fmt.Errorf("kms: build request: %w", err)
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}

	resp, err := c.httpCli.Do(req)
	if err != nil {
		return nil, fmt.Errorf("kms: request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("kms: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var payload struct {
		KeyID string `json:"key_id"`
		Key   string `json:"key"` // base64-encoded 32-byte AES-256 key
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("kms: parse response: %w", err)
	}
	if payload.Key == "" {
		return nil, fmt.Errorf("kms: empty key in response")
	}

	keyBytes, err := base64.StdEncoding.DecodeString(payload.Key)
	if err != nil {
		return nil, fmt.Errorf("kms: base64 decode key: %w", err)
	}
	if len(keyBytes) != 32 {
		return nil, fmt.Errorf("kms: key must be 32 bytes, got %d", len(keyBytes))
	}

	c.mu.Lock()
	c.keys[keyID] = keyBytes
	c.mu.Unlock()
	return keyBytes, nil
}

// decryptField decrypts a single field value (no-op if not ENC: prefixed).
func (c *kmsKeyCache) decryptField(ctx context.Context, value string) (string, error) {
	if !isEncrypted(value) {
		return value, nil
	}
	keyID := parseKeyID(value)
	if keyID == "" {
		return value, fmt.Errorf("decrypt: could not parse keyId from ENC: value")
	}
	key, err := c.getKey(ctx, keyID)
	if err != nil {
		return value, err
	}
	return decryptValue(value, key)
}

// decryptTestCaseRequest decrypts all ENC: values in tc.HTTPReq in-place.
// It modifies the test case directly so SimulateRequest sees the real values.
// Non-ENC fields are passed through unchanged.
func (c *kmsKeyCache) decryptTestCaseRequest(ctx context.Context, tc *models.TestCase, logger *zap.Logger) {
	if tc == nil || tc.Kind != models.HTTP {
		return
	}

	// Decrypt request headers.
	for k, v := range tc.HTTPReq.Header {
		if isEncrypted(v) {
			plain, err := c.decryptField(ctx, v)
			if err != nil {
				logger.Warn("failed to decrypt request header",
					zap.String("header", k), zap.Error(err))
				continue
			}
			tc.HTTPReq.Header[k] = plain
		}
	}

	// Decrypt URL params.
	for k, v := range tc.HTTPReq.URLParams {
		if isEncrypted(v) {
			plain, err := c.decryptField(ctx, v)
			if err != nil {
				logger.Warn("failed to decrypt URL param", zap.String("param", k), zap.Error(err))
				continue
			}
			tc.HTTPReq.URLParams[k] = plain
		}
	}

	// Decrypt body (may be an entire encrypted blob or a JSON with encrypted fields).
	if isEncrypted(tc.HTTPReq.Body) {
		plain, err := c.decryptField(ctx, tc.HTTPReq.Body)
		if err != nil {
			logger.Warn("failed to decrypt request body", zap.Error(err))
		} else {
			tc.HTTPReq.Body = plain
		}
	}
}

// newKMSCacheForRun creates a per-run key cache for decryption.
// Returns nil (skip decryption) when api-server URL is not configured.
// In OSS mode no auth token is available — requests are made without one.
// Enterprise/k8s-proxy override this by setting a token provider on the
// Replayer after construction.
func (r *Replayer) newKMSCacheForRun(_ context.Context) *kmsKeyCache {
	if r.config == nil || strings.TrimSpace(r.config.APIServerURL) == "" {
		// Running fully offline — no decryption available.
		return nil
	}
	// OSS mode: no auth token. Enterprise/k8s-proxy inject one at their own layer.
	return newKMSKeyCache(r.config.APIServerURL, "")
}
