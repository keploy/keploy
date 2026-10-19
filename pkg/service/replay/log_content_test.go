package replay

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/templates"
	"go.keploy.io/server/v3/utils/logcontent/logtest"
)

// canary is in a test case: its URL's query, a header's value, its body, the
// app's answer.
const canary = "kp-canary-replay-9a2e"

// A test sent to the app is logged by its name, method and path, not its
// request, unless KEPLOY_LOG_CONTENT is set: an HTTP test to an app that
// answers it, a gRPC one to a port where nothing listens.
func TestSimulateRequest_ATestSentIsLoggedWithoutItsRequest(t *testing.T) {
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(app.Close)
	appPort, _ := strconv.Atoi(app.URL[strings.LastIndex(app.URL, ":")+1:])
	closed := closedLocalPort(t)
	for want, tc := range map[string]*models.TestCase{
		"Simulating HTTP request": {
			Name: "post-notes-1", Kind: models.HTTP, AppPort: uint16(appPort),
			HTTPReq: models.HTTPReq{Method: "POST", URL: app.URL + "/notes?ref=" + canary,
				Header: map[string]string{"Authorization": "Bearer " + canary}, Body: `{"note":"` + canary + `"}`},
		},
		"Simulating gRPC request": {
			Name: "grpc-notes-1", Kind: models.GRPC_EXPORT, AppPort: closed,
			GrpcReq: models.GrpcReq{Headers: models.GrpcHeaders{
				PseudoHeaders:   map[string]string{":authority": net.JoinHostPort("localhost", strconv.Itoa(int(closed))), ":path": "/notes.Notes/Add", ":method": "POST", ":scheme": "http"},
				OrdinaryHeaders: map[string]string{"authorization": "Bearer " + canary},
			}},
		},
	} {
		t.Run(want, func(t *testing.T) {
			logtest.Each(t, func(t *testing.T, on bool) {
				logger, logs := logtest.Observe()
				h := NewHooks(logger, &config.Config{Test: config.Test{APITimeout: 5}}, &unpublishedPortInstr{prInstr: &prInstr{}, reason: "it is not published"})
				_, err := h.SimulateRequest(context.Background(), tc, "test-set-0")
				lines := []string{want}
				if tc.Kind == models.HTTP {
					if err != nil {
						t.Fatalf("the app did not answer: %v", err)
					}
					lines = append(lines, "Sending request to user app", "Replaced host with config host", "Final resolved target")
				}
				logtest.Check(t, logs, canary, on, lines...)
			})
		})
	}
}

// A test's URL rewritten to the base path, or to another host, is logged
// without its query's values, unless KEPLOY_LOG_CONTENT is set.
func TestReplayer_ARewrittenURLIsLoggedWithoutItsQuery(t *testing.T) {
	logtest.Each(t, func(t *testing.T, on bool) {
		logger, logs := logtest.Observe()
		cfg := &config.Config{}
		cfg.Test.BasePath = "http://app.local:8080"
		r := &Replayer{logger: logger, config: cfg}
		tc := &models.TestCase{Name: "get-notes-1", Kind: models.HTTP, HTTPReq: models.HTTPReq{Method: "GET", URL: "http://recorded:8080/notes?ref=" + canary}}
		if err := r.applyBasePath(tc); err != nil {
			t.Fatal(err)
		}
		if err := r.replaceHostInTestCase(tc, "127.0.0.1", "the app's container"); err != nil {
			t.Fatal(err)
		}
		logtest.Check(t, logs, canary, on, "test case request origin", "")
	})
}

// A value a template captures from the app's answer is logged by its key and
// place, unless KEPLOY_LOG_CONTENT is set.
func TestResolveExpected_ACapturedValueIsLoggedWithoutIt(t *testing.T) {
	logtest.Each(t, func(t *testing.T, on bool) {
		logger, logs := logtest.Observe()
		r := &Replayer{logger: logger, config: &config.Config{}}
		run := templates.NewRun(logger, map[string]interface{}{"token": "T0rec"}, nil)
		sent := &models.TestCase{Kind: models.HTTP, Name: "test-1", HTTPResp: models.HTTPResp{StatusCode: 200,
			Header: map[string]string{"Content-Type": "application/json"}, Body: `{"token":"{{capture string .token}}"}`}}
		answer := &models.HTTPResp{StatusCode: 200, Header: map[string]string{"Content-Type": "application/json"}, Body: `{"token":"` + canary + `"}`}
		r.resolveExpected(run, &templates.CheckResult{}, sent, answer, "test-set-0")
		logtest.Check(t, logs, canary, on, "captured a template value from the app's answer")
	})
}

// A call that fails a set is logged by its protocol, its summary and its
// destination, not how it differs from its closest recording, unless
// KEPLOY_LOG_CONTENT is set.
func TestReplayer_ACallThatFailsASetIsLoggedWithoutItsDiff(t *testing.T) {
	calls := []models.UnmatchedCall{{Protocol: "HTTP", ActualSummary: "POST /notes", Destination: "api.test",
		Diff: `body.note: recorded "` + canary + `-rec", live "` + canary + `-live"`}}
	logtest.Each(t, func(t *testing.T, on bool) {
		logger, logs := logtest.Observe()
		r := &Replayer{logger: logger, config: &config.Config{}}
		r.logSetCalls("test-set-0", calls)
		r.logUnclaimed("test-set-0", calls)
		logtest.Check(t, logs, canary, on,
			"test-set failed: the application made a call before its first test that no mock answered (KEPLOY_PROVEN=1)",
			"test-set failed: the application made a call after its last test that no mock answered (KEPLOY_PROVEN=1)")
	})
}
