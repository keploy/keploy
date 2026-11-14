package conn

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/internal/logleak"
	"go.keploy.io/server/v3/pkg/models"
)

// errorAfter reads its data, then fails: a client that hung up mid-body.
type errorAfter struct{ r io.Reader }

func (e *errorAfter) Read(p []byte) (int, error) {
	n, err := e.r.Read(p)
	if err == io.EOF {
		return n, errors.New("unexpected EOF")
	}
	return n, err
}

// The capture of an incoming call logs facts about the call (its method,
// its host, its path's shape, its query's names, its sizes), never its
// body or its URL as sent: a body holds what the application was sent (a
// login's password), and a path or a query a token. It logged a body it
// could not read whole, base64 encoded, and the whole URL of a call whose
// answer it did not keep.
func TestCaptureLogsNoneOfTheCall(t *testing.T) {
	const s = logleak.Canary
	target := "/v1/reset/" + s + "?access_token=" + s
	for _, tc := range []struct {
		name string
		body io.Reader
		resp string
		line string
	}{
		{"a body the client cut short", &errorAfter{strings.NewReader(`{"password":"` + s + `"}`)}, "ok", "failed to read the http request body"},
		{"an answer over 1MB", strings.NewReader(`{"password":"` + s + `"}`), strings.Repeat("a", LargeBodyThreshold) + s, "response body exceeds 1MB"},
		{"a call over the test case limit", strings.NewReader(`{"password":"` + s + `"}`), strings.Repeat("b", MaxTestCaseSize) + s, "exceeds 5MB limit"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logger, logs := logleak.Observe()
			req := httptest.NewRequest(http.MethodPost, target, tc.body)
			req.Header.Set("Authorization", "Bearer "+s)
			resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(tc.resp))}
			ch := make(chan *models.TestCase, 1)
			at := time.Unix(1000, 0)
			Capture(context.Background(), logger, ch, req, resp, at, at.Add(time.Millisecond), models.IncomingOptions{}, false, false, 8080)
			if logs.FilterMessageSnippet(tc.line).Len() == 0 {
				t.Fatalf("precondition: the capture did not log the case's line (%q); the test drove nothing", tc.line)
			}
			logleak.Check(t, logs, s, "the capture")
		})
	}
}
