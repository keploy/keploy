package pkg

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.keploy.io/server/v3/internal/logleak"
	"go.keploy.io/server/v3/pkg/models"
)

// Sending a test's request to the app logs its shape and size, never the
// request: its headers carry the test's credentials, its URL a token, its
// body a password. A request the app's connection drops fails with the
// client's error, which quotes the URL: withheld in the log line, and kept
// in the error returned, which the test's own report shows (beside the
// request it records anyway). It logged the whole *http.Request at debug
// level.
func TestSimulateHTTPLogsNoneOfTheRequest(t *testing.T) {
	const s = logleak.Canary
	for _, tc := range []struct {
		name  string
		fails bool
		app   http.HandlerFunc
	}{
		{"an answered request", false, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"ok":true}`)) }},
		{"a request whose connection the app drops", true, func(w http.ResponseWriter, _ *http.Request) {
			c, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				_ = c.(*net.TCPConn).SetLinger(0)
				_ = c.Close()
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app := httptest.NewServer(tc.app)
			t.Cleanup(app.Close)
			logger, logs := logleak.Observe()
			testCase := &models.TestCase{Name: "test-1", Kind: models.HTTP, HTTPReq: models.HTTPReq{
				Method: "POST", URL: app.URL + "/v1/reset/" + s + "?access_token=" + s,
				Header: map[string]string{"Authorization": "Bearer " + s, "Content-Type": "application/json"},
				Body:   `{"password":"` + s + `"}`,
			}}
			_, err := SimulateHTTP(context.Background(), testCase, "s", logger, SimulationConfig{APITimeout: 5})
			if logs.Len() == 0 {
				t.Fatal("precondition: nothing was logged; the test drove none of the lines")
			}
			logleak.Check(t, logs, s, "SimulateHTTP")
			if tc.fails {
				if err == nil || logs.FilterMessage("failed to send testcase request to app").Len() == 0 {
					t.Fatalf("precondition: the dropped request did not fail and log (%v)", err)
				}
				if !logleak.Holds(err.Error(), s) {
					t.Error("the error returned for the test's report lost the request's URL")
				}
			}
		})
	}
}
