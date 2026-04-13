package replay

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

func TestMockPushConfigChange_NonOKReturnsError(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name   string
		status int
		body   string
	}{
		{name: "500_with_body", status: http.StatusInternalServerError, body: "server error\n"},
		{name: "401_with_body", status: http.StatusUnauthorized, body: " unauthorized "},
		{name: "400_empty_body", status: http.StatusBadRequest, body: ""},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost {
					t.Fatalf("expected method POST, got %s", r.Method)
				}
				if r.URL.Path != configPushPath {
					t.Fatalf("expected path %s, got %s", configPushPath, r.URL.Path)
				}
				if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
					t.Fatalf("expected Authorization header, got %q", got)
				}
				w.WriteHeader(tc.status)
				if tc.body != "" {
					_, _ = w.Write([]byte(tc.body))
				}
			}))
			defer srv.Close()

			m := &mock{
				cfg:    &config.Config{APIServerURL: srv.URL},
				logger: zap.NewNop(),
				token:  "test-token",
			}

			err := m.pushConfigChange(context.Background(), "ts1", &models.TestSet{}, "owner", "branch")
			if err == nil {
				t.Fatalf("expected error for non-200 response")
			}
			if !strings.Contains(err.Error(), strconv.Itoa(tc.status)) {
				t.Fatalf("expected error to include status code %d, got %q", tc.status, err.Error())
			}
			trimmed := strings.TrimSpace(tc.body)
			if trimmed != "" && !strings.Contains(err.Error(), trimmed) {
				t.Fatalf("expected error to include response body %q, got %q", trimmed, err.Error())
			}
		})
	}
}

func TestMockPushConfigChange_OKReturnsNil(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		resp := MockChangeResp{CommitURL: "http://example.com/commit", Message: "ok"}
		if err := json.NewEncoder(w).Encode(&resp); err != nil {
			t.Fatalf("failed to encode response: %v", err)
		}
	}))
	defer srv.Close()

	m := &mock{
		cfg:    &config.Config{APIServerURL: srv.URL},
		logger: zap.NewNop(),
		token:  "test-token",
	}

	err := m.pushConfigChange(context.Background(), "ts1", &models.TestSet{}, "owner", "branch")
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
}

func TestMockPushConfigChange_OKInvalidJSONReturnsError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("not-json"))
	}))
	defer srv.Close()

	m := &mock{
		cfg:    &config.Config{APIServerURL: srv.URL},
		logger: zap.NewNop(),
		token:  "test-token",
	}

	err := m.pushConfigChange(context.Background(), "ts1", &models.TestSet{}, "owner", "branch")
	if err == nil {
		t.Fatalf("expected error for invalid JSON response")
	}
}
