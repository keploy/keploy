package pkg

import (
	"encoding/json"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.keploy.io/server/v3/utils/log"
)

func TestRenderTestCaseTemplates(t *testing.T) {
	logger, _, err := log.New()
	if err != nil {
		t.Fatalf("failed to create logger: %v", err)
	}

	tests := []struct {
		name              string
		testCase          *models.TestCase
		templatizedValues map[string]interface{}
		secretValues      map[string]string
		expectedURL       string
		expectedBody      string
		wantErr           bool
	}{
		{
			name: "simple template rendering",
			testCase: &models.TestCase{
				HTTPReq: models.HTTPReq{
					URL:    "http://example.com/{{.userId}}",
					Body:   `{"name": "{{.userName}}"}`,
					Method: "POST",
				},
			},
			templatizedValues: map[string]interface{}{
				"userId":   123,
				"userName": "alice",
			},
			expectedURL:  "http://example.com/123",
			expectedBody: `{"name": "alice"}`,
			wantErr:      false,
		},
		{
			name: "secret values rendering",
			testCase: &models.TestCase{
				HTTPReq: models.HTTPReq{
					URL:  "http://example.com/api",
					Body: `{"token": "{{.secret.apiKey}}"}`,
				},
			},
			secretValues: map[string]string{
				"apiKey": "secret123",
			},
			expectedURL:  "http://example.com/api",
			expectedBody: `{"token": "secret123"}`,
			wantErr:      false,
		},
		{
			name: "encoded URL with template placeholders",
			testCase: &models.TestCase{
				HTTPReq: models.HTTPReq{
					URL:  "http://example.com/{{.userId}}",
					Body: "",
				},
			},
			templatizedValues: map[string]interface{}{
				"userId": 456,
			},
			expectedURL:  "http://example.com/456",
			expectedBody: "",
			wantErr:      false,
		},
		{
			name: "ignore non-Keploy placeholders",
			testCase: &models.TestCase{
				HTTPReq: models.HTTPReq{
					URL:  "http://example.com/api",
					Body: `{"formula": "E=mc^2", "latex": "{{\pi}}"}`,
				},
			},
			templatizedValues: map[string]interface{}{},
			expectedURL:       "http://example.com/api",
			expectedBody:      `{"formula": "E=mc^2", "latex": "{{\pi}}"}`,
			wantErr:           false,
		},
		{
			name: "no templates to render",
			testCase: &models.TestCase{
				HTTPReq: models.HTTPReq{
					URL:  "http://example.com/api",
					Body: `{"plain": "value"}`,
				},
			},
			templatizedValues: map[string]interface{}{},
			expectedURL:       "http://example.com/api",
			expectedBody:      `{"plain": "value"}`,
			wantErr:           false,
		},
		{
			name: "string function in template",
			testCase: &models.TestCase{
				HTTPReq: models.HTTPReq{
					URL:  "http://example.com/{{string .userId}}",
					Body: "",
				},
			},
			templatizedValues: map[string]interface{}{
				"userId": 789,
			},
			expectedURL:  "http://example.com/789",
			expectedBody: "",
			wantErr:      false,
		},
		{
			name: "mixed templates and secrets",
			testCase: &models.TestCase{
				HTTPReq: models.HTTPReq{
					URL:  "http://example.com/{{.userId}}",
					Body: `{"userId": "{{.userId}}", "token": "{{.secret.apiKey}}"}`,
				},
			},
			templatizedValues: map[string]interface{}{
				"userId": 999,
			},
			secretValues: map[string]string{
				"apiKey": "supersecret",
			},
			expectedURL:  "http://example.com/999",
			expectedBody: `{"userId": "999", "token": "supersecret"}`,
			wantErr:      false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Set up global state
			utils.TemplatizedValues = tt.templatizedValues
			// Convert secretValues to map[string]interface{}
			if len(tt.secretValues) > 0 {
				utils.SecretValues = make(map[string]interface{})
				for k, v := range tt.secretValues {
					utils.SecretValues[k] = v
				}
			} else {
				utils.SecretValues = nil
			}
			defer func() {
				utils.TemplatizedValues = nil
				utils.SecretValues = nil
			}()

			// Create a copy of the test case to avoid modifying the original
			tcBytes, _ := json.Marshal(tt.testCase)
			var tc models.TestCase
			json.Unmarshal(tcBytes, &tc)

			err := RenderTestCaseTemplates(&tc, logger)
			if (err != nil) != tt.wantErr {
				t.Errorf("RenderTestCaseTemplates() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if !tt.wantErr {
				if tc.HTTPReq.URL != tt.expectedURL {
					t.Errorf("URL = %v, want %v", tc.HTTPReq.URL, tt.expectedURL)
				}
				if tc.HTTPReq.Body != tt.expectedBody {
					t.Errorf("Body = %v, want %v", tc.HTTPReq.Body, tt.expectedBody)
				}
			}
		})
	}
}

func TestRenderTestCaseTemplates_NoTemplates(t *testing.T) {
	logger, _, err := log.New()
	if err != nil {
		t.Fatalf("failed to create logger: %v", err)
	}

	tc := &models.TestCase{
		HTTPReq: models.HTTPReq{
			URL:  "http://example.com/api",
			Body: `{"plain": "value"}`,
		},
	}

	// Ensure no template values are set
	utils.TemplatizedValues = nil
	utils.SecretValues = nil

	err = RenderTestCaseTemplates(tc, logger)
	if err != nil {
		t.Errorf("RenderTestCaseTemplates() with no templates should not error, got %v", err)
	}

	// Verify the test case is unchanged
	if tc.HTTPReq.URL != "http://example.com/api" {
		t.Errorf("URL should be unchanged, got %v", tc.HTTPReq.URL)
	}
	if tc.HTTPReq.Body != `{"plain": "value"}` {
		t.Errorf("Body should be unchanged, got %v", tc.HTTPReq.Body)
	}
}
