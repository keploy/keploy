package record

import (
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

func piiWarnings(t *testing.T, tc *models.TestCase) []string {
	t.Helper()
	core, logs := observer.New(zapcore.WarnLevel)
	(&Recorder{logger: zap.New(core)}).warnPIIDetections(tc, "test-set-3")
	messages := make([]string, 0, logs.Len())
	for _, entry := range logs.All() {
		messages = append(messages, entry.Message)
	}
	return messages
}

func TestPIIWarningSaysWhatToDoNext(t *testing.T) {
	got := piiWarnings(t, &models.TestCase{
		HTTPReq: models.HTTPReq{
			Method: models.Method("POST"),
			URL:    "http://localhost:8080/users?ref=abc",
			Body:   `{"email":"user@example.com"}`,
		},
	})
	if len(got) != 1 {
		t.Fatalf("got %d warnings, want 1: %q", len(got), got)
	}
	for _, want := range []string{
		"POST http://localhost:8080/users",
		"request.body.email (email)",
		"`keploy sanitize -t test-set-3`",
	} {
		if !strings.Contains(got[0], want) {
			t.Errorf("warning %q does not say %q", got[0], want)
		}
	}
	if strings.Contains(got[0], "ref=abc") {
		t.Errorf("warning %q repeats the query string", got[0])
	}
}

func TestPIIWarningNamesASkippedResponseBody(t *testing.T) {
	got := piiWarnings(t, &models.TestCase{
		HTTPReq:  models.HTTPReq{Method: models.Method("GET"), URL: "http://localhost:8080/export"},
		HTTPResp: models.HTTPResp{BodySkipped: true, BodySize: 2097152},
	})
	if len(got) != 1 {
		t.Fatalf("got %d warnings, want 1: %q", len(got), got)
	}
	for _, want := range []string{"response body", "GET http://localhost:8080/export", "2097152 bytes", "not scanned"} {
		if !strings.Contains(got[0], want) {
			t.Errorf("warning %q does not say %q", got[0], want)
		}
	}
}

func TestPIIWarningQuietWithNothingToReport(t *testing.T) {
	got := piiWarnings(t, &models.TestCase{
		HTTPReq:  models.HTTPReq{Method: models.Method("GET"), URL: "http://localhost:8080/health"},
		HTTPResp: models.HTTPResp{Body: `{"status":"ok"}`},
	})
	if len(got) != 0 {
		t.Fatalf("got warnings for a clean test case: %q", got)
	}
}

func TestPIIWarningDecodesAnEscapedPath(t *testing.T) {
	got := piiWarnings(t, &models.TestCase{
		HTTPReq: models.HTTPReq{Method: models.Method("GET"), URL: "http://localhost:8080/users/user%40example.com"},
	})
	if len(got) != 1 {
		t.Fatalf("got %d warnings, want 1: %q", len(got), got)
	}
	if !strings.Contains(got[0], "request.url_path (email)") {
		t.Errorf("warning %q does not report the email in the decoded path", got[0])
	}
}

func TestPIIWarningDoesNotRepeatPIIFromThePath(t *testing.T) {
	for _, rawURL := range []string{
		"http://localhost:8080/users/user@example.com",
		"http://localhost:8080/users/user%40example.com",
	} {
		got := piiWarnings(t, &models.TestCase{
			HTTPReq: models.HTTPReq{Method: models.Method("GET"), URL: rawURL},
		})
		if len(got) != 1 {
			t.Fatalf("%s: got %d warnings, want 1: %q", rawURL, len(got), got)
		}
		if strings.Contains(got[0], "user@example.com") || strings.Contains(got[0], "user%40example.com") {
			t.Errorf("warning %q repeats the email from the path", got[0])
		}
		if !strings.Contains(got[0], "GET http://localhost:8080 (path withheld") {
			t.Errorf("warning %q does not name the call by method and host", got[0])
		}
	}
}

func TestPIIWarningDoesNotRepeatPIIFromTheTestName(t *testing.T) {
	got := piiWarnings(t, &models.TestCase{
		Name: "signup-user@example.com",
		HTTPReq: models.HTTPReq{
			Method: models.Method("POST"),
			URL:    "http://localhost:8080/users",
			Body:   `{"email":"user@example.com"}`,
		},
	})
	if len(got) != 1 {
		t.Fatalf("got %d warnings, want 1: %q", len(got), got)
	}
	if strings.Contains(got[0], "user@example.com") {
		t.Errorf("warning %q repeats the email from the test name", got[0])
	}
	if !strings.Contains(got[0], "POST http://localhost:8080/users") {
		t.Errorf("warning %q does not fall back to the method and path", got[0])
	}
}

func TestPIIWarningKeepsACleanTestName(t *testing.T) {
	got := piiWarnings(t, &models.TestCase{
		Name: "create-user",
		HTTPReq: models.HTTPReq{
			Method: models.Method("POST"),
			URL:    "http://localhost:8080/users",
			Body:   `{"email":"user@example.com"}`,
		},
	})
	if len(got) != 1 || !strings.Contains(got[0], "create-user") {
		t.Fatalf("warning %q does not name the test case", got)
	}
}

func TestPIIWarningScansMultipartFormFields(t *testing.T) {
	got := piiWarnings(t, &models.TestCase{
		HTTPReq: models.HTTPReq{
			Method: models.Method("POST"),
			URL:    "http://localhost:8080/signup",
			Form: []models.FormData{
				{Key: "contact", Values: []string{"user@example.com"}},
				{Key: "password", Values: []string{"hunter2"}},
				{Key: "plan", Values: []string{"basic"}},
			},
		},
	})
	if len(got) != 1 {
		t.Fatalf("got %d warnings, want 1: %q", len(got), got)
	}
	for _, want := range []string{"request.form.contact (email)", "request.form.password ("} {
		if !strings.Contains(got[0], want) {
			t.Errorf("warning %q does not say %q", got[0], want)
		}
	}
	for _, leak := range []string{"user@example.com", "hunter2", "request.form.plan"} {
		if strings.Contains(got[0], leak) {
			t.Errorf("warning %q should not contain %q", got[0], leak)
		}
	}
}

func TestPIIWarningScansURLEncodedFormFields(t *testing.T) {
	for _, tc := range []struct {
		contentType string
		body        string
	}{
		// Capture has already unescaped this body.
		{"application/x-www-form-urlencoded", "username=user&password=hunter2"},
		// With a parameter, Capture leaves the body escaped.
		{"application/x-www-form-urlencoded; charset=utf-8", "username=user&pass%77ord=hunter2"},
	} {
		got := piiWarnings(t, &models.TestCase{
			HTTPReq: models.HTTPReq{
				Method: models.Method("POST"),
				URL:    "http://localhost:8080/login",
				Header: map[string]string{"Content-Type": tc.contentType},
				Body:   tc.body,
			},
		})
		if len(got) != 1 {
			t.Fatalf("%s: got %d warnings, want 1: %q", tc.contentType, len(got), got)
		}
		if !strings.Contains(got[0], "request.form.password (field_name)") {
			t.Errorf("%s: warning %q does not report the password field", tc.contentType, got[0])
		}
		for _, leak := range []string{"hunter2", "request.form.username"} {
			if strings.Contains(got[0], leak) {
				t.Errorf("%s: warning %q should not contain %q", tc.contentType, got[0], leak)
			}
		}
	}
}

func TestPIIWarningLeavesANonFormBodyAlone(t *testing.T) {
	got := piiWarnings(t, &models.TestCase{
		HTTPReq: models.HTTPReq{
			Method: models.Method("POST"),
			URL:    "http://localhost:8080/notes",
			Header: map[string]string{"Content-Type": "text/plain"},
			Body:   "password=hunter2",
		},
	})
	if len(got) != 0 {
		t.Fatalf("got warnings for a plain-text body: %q", got)
	}
}

func TestPIIWarningRedactsPIIInFieldPaths(t *testing.T) {
	got := piiWarnings(t, &models.TestCase{
		HTTPReq: models.HTTPReq{
			Method: models.Method("POST"),
			URL:    "http://localhost:8080/users",
			Body:   `{"user@example.com":{"password":"hunter2"}}`,
		},
	})
	if len(got) != 1 {
		t.Fatalf("got %d warnings, want 1: %q", len(got), got)
	}
	if strings.Contains(got[0], "user@example.com") {
		t.Errorf("warning %q copies the email from a JSON key", got[0])
	}
	if !strings.Contains(got[0], "request.body.[redacted].password (field_name)") {
		t.Errorf("warning %q does not name the redacted path", got[0])
	}
}
