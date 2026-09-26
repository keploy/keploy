package pkg

import (
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

// MakeCurlCommand used to range over a map, so two recordings of identical
// traffic emitted the header lines in different orders — churn in the recorded
// test case and noise in every diff of it.
func TestMakeCurlCommand_HeaderOrderIsDeterministic(t *testing.T) {
	req := models.HTTPReq{
		Method: models.Method("POST"),
		URL:    "http://localhost:8080/url",
		Header: map[string]string{
			"Accept":         "*/*",
			"Content-Type":   "application/json",
			"Host":           "localhost:8080",
			"User-Agent":     "curl/8.5.0",
			"X-Request-Id":   "abc123",
			"Content-Length": "27",
		},
		Body: `{"url":"https://google.com"}`,
	}

	first := MakeCurlCommand(req)
	for i := 0; i < 50; i++ {
		if got := MakeCurlCommand(req); got != first {
			t.Fatalf("output differs between identical calls (iteration %d):\n--- first ---\n%s\n--- got ---\n%s", i, first, got)
		}
	}

	// Headers must be sorted, and Content-Length still excluded.
	var headers []string
	for _, line := range strings.Split(first, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "--header '") {
			name := strings.SplitN(strings.TrimPrefix(line, "--header '"), ":", 2)[0]
			headers = append(headers, name)
		}
	}

	want := []string{"Accept", "Content-Type", "Host", "User-Agent", "X-Request-Id"}
	if strings.Join(headers, ",") != strings.Join(want, ",") {
		t.Errorf("headers = %v, want %v (sorted, Content-Length excluded)", headers, want)
	}
}

func TestMakeCurlCommand_NoHeaders(t *testing.T) {
	req := models.HTTPReq{Method: models.Method("GET"), URL: "http://localhost:8080/"}
	got := MakeCurlCommand(req)
	if strings.Contains(got, "--header") {
		t.Errorf("expected no header lines, got:\n%s", got)
	}
	if !strings.Contains(got, "--request GET") || !strings.Contains(got, "--url http://localhost:8080/") {
		t.Errorf("malformed curl command:\n%s", got)
	}
}
