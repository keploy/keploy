package storage

import (
    "testing"
    "context"
    "strings"
    "net/http"
    "net/http/httptest"
    "encoding/json"
    "go.uber.org/zap"
)


// Test generated using Keploy
func TestUpload_ErrorResponse(t *testing.T) {
    logger := zap.NewNop()

    handler := func(w http.ResponseWriter, r *http.Request) {
        w.WriteHeader(http.StatusInternalServerError)
        mockResponse := MockUploadResponse{
            IsSuccess: false,
            Error:     "Internal Server Error",
        }
        json.NewEncoder(w).Encode(mockResponse)
    }
    ts := httptest.NewServer(http.HandlerFunc(handler))
    defer ts.Close()

    storage := New(ts.URL, logger, ts.Client())
    fileContent := "test file content"
    fileReader := strings.NewReader(fileContent)
    err := storage.Upload(context.Background(), fileReader, "mockName", "appName", "token")
    if err == nil {
        t.Error("Expected error, got nil")
    }
}

// Test generated using Keploy
func TestDownload_ErrorResponse(t *testing.T) {
    logger := zap.NewNop()

    handler := func(w http.ResponseWriter, r *http.Request) {
        w.WriteHeader(http.StatusNotFound)
        w.Write([]byte("Mock not found"))
    }
    ts := httptest.NewServer(http.HandlerFunc(handler))
    defer ts.Close()

    storage := New(ts.URL, logger, ts.Client())
    _, err := storage.Download(context.Background(), "mockName", "appName", "userName", "jwtToken")
    if err == nil {
        t.Error("Expected error, got nil")
    }
}


// Test generated using Keploy
func TestUpload_NilHttpClient_DefaultsToDefaultClient(t *testing.T) {
    logger := zap.NewNop()

    handler := func(w http.ResponseWriter, r *http.Request) {
        w.WriteHeader(http.StatusOK)
        mockResponse := MockUploadResponse{
            IsSuccess: true,
            Error:     "",
        }
        json.NewEncoder(w).Encode(mockResponse)
    }
    ts := httptest.NewServer(http.HandlerFunc(handler))
    defer ts.Close()

    storage := New(ts.URL, logger, nil)
    fileContent := "test file content"
    fileReader := strings.NewReader(fileContent)
    err := storage.Upload(context.Background(), fileReader, "mockName", "appName", "token")
    if err != nil {
        t.Errorf("Expected no error, got %v", err)
    }
}

