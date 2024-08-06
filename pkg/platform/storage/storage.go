// Package storage defines methods for storage DB.
package storage

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"path/filepath"
	"strings"

	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
)

type Storage struct {
	serverURL string
	logger    *zap.Logger
}

type MockUploadResponse struct {
	IsSuccess bool   `json:"isSuccess"`
	Error     string `json:"error"`
}

func New(serverURL string, logger *zap.Logger) *Storage {
	return &Storage{
		serverURL: serverURL,
		logger:    logger,
	}
}

func (s *Storage) Upload(ctx context.Context, file io.Reader, mockName string, appName string, token string) error {
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
	err = writer.WriteField("appName", appName)
	if err != nil {
		s.logger.Error("Error writing appName field", zap.Error(err))
		return err
	}
	err = writer.WriteField("mockName", mockName)
	if err != nil {
		s.logger.Error("Error writing mockName field", zap.Error(err))
		return err
	}
	err = writer.Close()
	if err != nil {
		s.logger.Error("Error closing writer", zap.Error(err))
		return err
	}

	// Create a new HTTP request
	req, err := http.NewRequestWithContext(ctx, "POST", s.serverURL+"/mock/upload", body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+token)

	// Execute the request
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() {
		err := resp.Body.Close()
		if err != nil {
			utils.LogError(s.logger, err, "failed to close the http response body")
		}
	}()

	var mockUploadResponse MockUploadResponse
	if err := json.NewDecoder(resp.Body).Decode(&mockUploadResponse); err != nil {
		utils.LogError(s.logger, err, "failed to decode the response body")
		return err
	}

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("upload failed with status code: %d and error %s", resp.StatusCode, mockUploadResponse.Error)
	}

	if !mockUploadResponse.IsSuccess {
		utils.LogError(s.logger, fmt.Errorf("upload failed: %s", mockUploadResponse.Error), "failed to upload the mock")
		return fmt.Errorf("upload failed: %s", mockUploadResponse.Error)
	}

	s.logger.Info("Mock uploaded successfully")

	return nil
}

func (s *Storage) Download(ctx context.Context, mockName string, appName string, userName string, jwtToken string) (io.Reader, error) {
	// Create the HTTP request
	req, err := http.NewRequestWithContext(ctx, "GET", fmt.Sprintf("%s/mock/download?appName=%s&mockName=%s&userName=%s", s.serverURL, appName, mockName, userName), nil)
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
		defer resp.Body.Close()
		// Read the response body to get the error message
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, fmt.Errorf("failed to read response body and the resp code is: %d", resp.StatusCode)
		}
		return nil, fmt.Errorf("download failed with status code: %d, message: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	return resp.Body, nil
}
