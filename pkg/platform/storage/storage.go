// Package storage defines methods for storage DB.
package storage

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"path/filepath"
	"strings"
	"sync"
	"time"

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

	done := make(chan struct{})
	var once sync.Once

	closeDone := func() {
		once.Do(func() {
			close(done)
		})
	}

	// Spinner goroutine
	go func() {
		spinnerChars := []rune{'|', '/', '-', '\\'}
		i := 0
		for {
			select {
			case <-done:
				fmt.Print("\r") // Clear spinner line after done
				return
			default:
				fmt.Printf("\rUploading... %c", spinnerChars[i%len(spinnerChars)])
				i++
				time.Sleep(100 * time.Millisecond)
			}
		}
	}()
	defer closeDone() // Ensure we close the channel when the function exits

	// Create a multipart buffer and writer
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)

	// Create a custom part header for the file field
	partHeader := textproto.MIMEHeader{}
	partHeader.Set("Content-Disposition", fmt.Sprintf(`form-data; name="%s"; filename="%s"`, "mock", filepath.Base("mocks.yaml")))
	partHeader.Set("Content-Type", "application/octet-stream")
	partHeader.Set("Content-Encoding", "gzip") // Explicitly declare compression

	part, err := writer.CreatePart(partHeader)
	if err != nil {
		return err
	}

	// Compress file data with gzip and write into multipart part
	gzipWriter := gzip.NewWriter(part)
	if _, err := io.Copy(gzipWriter, file); err != nil {
		return err
	}
	if err := gzipWriter.Close(); err != nil {
		return err
	}

	// Add other form fields
	if err := writer.WriteField("appName", appName); err != nil {
		s.logger.Error("Error writing appName field", zap.Error(err))
		return err
	}
	if err := writer.WriteField("mockName", mockName); err != nil {
		s.logger.Error("Error writing mockName field", zap.Error(err))
		return err
	}
	if err := writer.Close(); err != nil {
		s.logger.Error("Error closing writer", zap.Error(err))
		return err
	}

	// Prepare the HTTP request
	req, err := http.NewRequestWithContext(ctx, "POST", s.serverURL+"/mock/upload", body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+token)

	// Execute the HTTP request
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			utils.LogError(s.logger, err, "failed to close the http response body")
		}
	}()

	// Parse the response
	var mockUploadResponse MockUploadResponse
	if err := json.NewDecoder(resp.Body).Decode(&mockUploadResponse); err != nil {
		utils.LogError(s.logger, err, "failed to decode the response body")
		return err
	}

	closeDone()

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

	req.Header.Set("Accept-Encoding", "gzip") // Request gzip encoding

	// Execute the request
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != http.StatusOK {
		defer func() {
			err := resp.Body.Close()
			if err != nil {
				utils.LogError(s.logger, err, "failed to close the http response body")
			}
		}()
		// Read the response body to get the error message
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, fmt.Errorf("failed to read response body and the resp code is: %d", resp.StatusCode)
		}
		return nil, fmt.Errorf("download failed with status code: %d, message: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	// Check if the response is gzipped
	if strings.EqualFold(resp.Header.Get("Content-Encoding"), "gzip") {
		s.logger.Debug("mock response is gzipped")
		gr, err := gzip.NewReader(resp.Body)
		if err != nil {
			defer func() {
				err := resp.Body.Close()
				if err != nil {
					utils.LogError(s.logger, err, "failed to close the http response body")
				}
			}()
			return nil, fmt.Errorf("failed to create gzip reader: %w", err)
		}
		return gr, nil // gr is an io.Reader, decompressing transparently
	}

	return resp.Body, nil
}
