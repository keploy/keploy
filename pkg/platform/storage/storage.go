// Package auth defines methods for authenticating with GitHub.
package auth

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"path/filepath"

	"go.uber.org/zap"
)

type Storage struct {
	serverURL string
	logger    *zap.Logger
}

func New(serverURL string, installationID string, logger *zap.Logger, gitHubClientID string) *Storage {
	return &Storage{
		serverURL: serverURL,
		logger:    logger,
	}
}

func (s *Storage) Upload(ctx context.Context, file io.Reader, mockName string, appName string) error {
	// Prepare the multipart form file upload request
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	part, err := writer.CreateFormFile("mock", filepath.Base("mocks.yaml"))
	if err != nil {
		return err
	}
	if _, err := io.Copy(part, file); err != nil {
		return err
	}
	writer.WriteField("appName", appName)
	writer.WriteField("mockName", mockName)
	writer.Close()

	// Create a new HTTP request
	req, err := http.NewRequestWithContext(ctx, "POST", s.serverURL+"/upload", body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())

	// Execute the request
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("upload failed with status code: %d", resp.StatusCode)
	}

	return nil
}

// TODO add userName in API
func (s *Storage) Download(ctx context.Context, mockName string, appName string, userName string, jwtToken string) (io.Reader, error) {
	// Create the HTTP request
	req, err := http.NewRequestWithContext(ctx, "GET", fmt.Sprintf("%s/download?appName=%s&mockName=%s", s.serverURL, appName, mockName), nil)
	if err != nil {
		return nil, err
	}

	req.Header.Set("Authorization", "Bearer "+jwtToken)

	// Execute the request
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download failed with status code: %d", resp.StatusCode)
	}

	return resp.Body, nil
}
