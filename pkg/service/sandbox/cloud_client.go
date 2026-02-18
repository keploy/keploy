package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// CloudClient provides methods to interact with the API server for sandbox operations.
type CloudClient struct {
	baseURL    string
	httpClient *http.Client
	jwtToken   string
	logger     *zap.Logger
}

// NewCloudClient creates a new CloudClient for sandbox API server interactions.
func NewCloudClient(baseURL, jwtToken string, logger *zap.Logger) *CloudClient {
	return &CloudClient{
		baseURL:    baseURL,
		httpClient: &http.Client{},
		jwtToken:   jwtToken,
		logger:     logger,
	}
}

// GetManifest fetches the sandbox manifest from the API server.
// Returns nil, nil if the manifest does not exist (404).
func (c *CloudClient) GetManifest(ctx context.Context, ref string) (*models.SandboxManifest, error) {
	url := fmt.Sprintf("%s/sandbox/manifest?ref=%s", c.baseURL, url.QueryEscape(ref))

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create manifest request: %w", err)
	}
	if err := c.addAuthHeader(req); err != nil {
		return nil, fmt.Errorf("failed to prepare manifest request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch manifest: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		c.logger.Info("sandbox manifest not found in cloud", zap.String("ref", ref))
		return nil, nil
	}

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("failed to fetch manifest (status %d): %s", resp.StatusCode, string(body))
	}

	var manifest models.SandboxManifest
	if err := json.NewDecoder(resp.Body).Decode(&manifest); err != nil {
		return nil, fmt.Errorf("failed to decode manifest response: %w", err)
	}

	return &manifest, nil
}

// UploadArtifact uploads the manifest and artifact zip to the API server.
func (c *CloudClient) UploadArtifact(ctx context.Context, manifest *models.SandboxManifest, artifactData io.Reader) error {
	manifestJSON, err := json.Marshal(manifest)
	if err != nil {
		return fmt.Errorf("failed to marshal manifest: %w", err)
	}

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)

	// Add manifest field.
	if err := writer.WriteField("manifest", string(manifestJSON)); err != nil {
		return fmt.Errorf("failed to write manifest field: %w", err)
	}

	// Add artifact file.
	part, err := writer.CreateFormFile("artifact", "artifact.zip")
	if err != nil {
		return fmt.Errorf("failed to create artifact form file: %w", err)
	}
	if _, err := io.Copy(part, artifactData); err != nil {
		return fmt.Errorf("failed to copy artifact data: %w", err)
	}

	if err := writer.Close(); err != nil {
		return fmt.Errorf("failed to close multipart writer: %w", err)
	}

	url := fmt.Sprintf("%s/sandbox/upload", c.baseURL)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, &body)
	if err != nil {
		return fmt.Errorf("failed to create upload request: %w", err)
	}
	if err := c.addAuthHeader(req); err != nil {
		return fmt.Errorf("failed to prepare upload request: %w", err)
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to upload artifact: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("artifact upload failed (status %d): %s", resp.StatusCode, string(respBody))
	}

	c.logger.Info("sandbox artifact uploaded successfully", zap.String("ref", manifest.Ref))
	return nil
}

// DownloadArtifact downloads the artifact zip from the API server.
func (c *CloudClient) DownloadArtifact(ctx context.Context, ref string) (io.ReadCloser, error) {
	url := fmt.Sprintf("%s/sandbox/download?ref=%s", c.baseURL, url.QueryEscape(ref))

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create download request: %w", err)
	}
	if err := c.addAuthHeader(req); err != nil {
		return nil, fmt.Errorf("failed to prepare download request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to download artifact: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, fmt.Errorf("artifact download failed (status %d): %s", resp.StatusCode, string(body))
	}

	return resp.Body, nil
}

func (c *CloudClient) addAuthHeader(req *http.Request) error {
	token := strings.TrimSpace(c.jwtToken)
	if token == "" {
		return fmt.Errorf("missing jwt token")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	return nil
}
