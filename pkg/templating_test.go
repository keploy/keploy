package pkg

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

func TestRenderTestCaseWithTemplates_890(t *testing.T) {
	logger := zap.NewNop()

	t.Run("BasicTemplating", func(t *testing.T) {
		utils.TemplatizedValues = map[string]interface{}{
			"name": "keploy",
		}
		utils.SecretValues = map[string]interface{}{}
		defer func() {
			utils.TemplatizedValues = map[string]interface{}{}
		}()

		tc := &models.TestCase{
			HTTPReq: models.HTTPReq{
				URL:  "http://example.com/{{.name}}",
				Body: `{"key":"{{.name}}"}`,
			},
		}

		rendered, err := RenderTestCaseWithTemplates(logger, tc)
		require.NoError(t, err)
		assert.Equal(t, "http://example.com/keploy", rendered.HTTPReq.URL)
		assert.Equal(t, `{"key":"keploy"}`, rendered.HTTPReq.Body)
	})

	t.Run("SecretTemplating", func(t *testing.T) {
		utils.TemplatizedValues = map[string]interface{}{}
		utils.SecretValues = map[string]interface{}{
			"token": "secret-token",
		}
		defer func() {
			utils.SecretValues = map[string]interface{}{}
		}()

		tc := &models.TestCase{
			HTTPReq: models.HTTPReq{
				Header: map[string]string{
					"Authorization": "Bearer {{.secret.token}}",
				},
			},
		}

		rendered, err := RenderTestCaseWithTemplates(logger, tc)
		require.NoError(t, err)
		assert.Equal(t, "Bearer secret-token", rendered.HTTPReq.Header["Authorization"])
	})

	t.Run("EncodedURLWithBraces", func(t *testing.T) {
		utils.TemplatizedValues = map[string]interface{}{
			"id": "123",
		}
		defer func() {
			utils.TemplatizedValues = map[string]interface{}{}
		}()

		tc := &models.TestCase{
			HTTPReq: models.HTTPReq{
				URL: "http://example.com/users/%7B%7B.id%7D%7D",
			},
		}

		rendered, err := RenderTestCaseWithTemplates(logger, tc)
		require.NoError(t, err)
		assert.Equal(t, "http://example.com/users/123", rendered.HTTPReq.URL)
	})

	t.Run("IgnoreNonKeployPlaceholders", func(t *testing.T) {
		tc := &models.TestCase{
			HTTPReq: models.HTTPReq{
				Body: `Value: {{\pi}}`,
			},
		}

		rendered, err := RenderTestCaseWithTemplates(logger, tc)
		require.NoError(t, err)
		assert.Equal(t, `Value: {{\pi}}`, rendered.HTTPReq.Body)
	})

	t.Run("NoTemplates", func(t *testing.T) {
		utils.TemplatizedValues = map[string]interface{}{}
		utils.SecretValues = map[string]interface{}{}

		tc := &models.TestCase{
			HTTPReq: models.HTTPReq{
				URL: "http://example.com/test",
			},
		}

		rendered, err := RenderTestCaseWithTemplates(logger, tc)
		require.NoError(t, err)
		assert.Equal(t, tc.HTTPReq.URL, rendered.HTTPReq.URL)
		// Ensure it's a copy
		rendered.HTTPReq.URL = "changed"
		assert.NotEqual(t, tc.HTTPReq.URL, rendered.HTTPReq.URL)
	})
}

func TestSimulateHTTP_WithTemplating_891(t *testing.T) {
	logger := zap.NewNop()
	ctx := context.Background()

	// Mock server to respond to requests
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/keploy", r.URL.Path)
		assert.Equal(t, "application/json", r.Header.Get("Content-Type"))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer server.Close()

	utils.TemplatizedValues = map[string]interface{}{
		"name": "keploy",
	}
	defer func() {
		utils.TemplatizedValues = map[string]interface{}{}
	}()

	tc := &models.TestCase{
		Name: "test-templating",
		HTTPReq: models.HTTPReq{
			Method: "GET",
			URL:    server.URL + "/{{.name}}",
			Header: map[string]string{
				"Content-Type": "application/json",
			},
		},
	}

	resp, err := SimulateHTTP(ctx, tc, "test-set", logger, 10)
	require.NoError(t, err)
	require.NotNil(t, resp)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, `{"status":"ok"}`, resp.Body)
}
