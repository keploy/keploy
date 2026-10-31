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
