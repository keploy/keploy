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

// A call that fails a set is logged by its protocol, its shape and its
// destination, not the values it sent or how it differs from its closest
// recording, unless KEPLOY_LOG_CONTENT is set.
func TestReplayer_ACallThatFailsASetIsLoggedWithoutItsContent(t *testing.T) {
	for name, call := range map[string]models.UnmatchedCall{
		"its diff": {Protocol: "HTTP", ActualSummary: "POST /notes", Destination: "api.test",
			Diff: `body.note: recorded "` + canary + `-rec", live "` + canary + `-live"`},
		"its statement's literals": {Protocol: "MySQL", Destination: "db:3306",
			ActualSummary: "COM_QUERY SELECT body FROM notes WHERE tag = '" + canary + "'"},
	} {
		t.Run(name, func(t *testing.T) {
			logtest.Each(t, func(t *testing.T, on bool) {
				logger, logs := logtest.Observe()
				r := &Replayer{logger: logger, config: &config.Config{}}
				r.logSetCalls("test-set-0", []models.UnmatchedCall{call})
				r.logUnclaimed("test-set-0", []models.UnmatchedCall{call})
				logtest.Check(t, logs, canary, on,
					"test-set failed: the application made a call before its first test that no mock answered (KEPLOY_PROVEN=1)",
					"test-set failed: the application made a call after its last test that no mock answered (KEPLOY_PROVEN=1)")
				for _, e := range logs.All() {
					if got, want := e.ContextMap()["call_shape"], models.MissShape(call); got != want || want == "" {
						t.Fatalf("%q names the call %v, want its shape %q", e.Message, got, want)
					}
				}
			})
		})
	}
}

// A call another test recorded is named by its shape, not the values it
// sent, unless KEPLOY_LOG_CONTENT is set.
func TestReplayer_ACallAnotherTestRecordedIsNamedWithoutItsContent(t *testing.T) {
	logtest.Each(t, func(t *testing.T, on bool) {
		logger, logs := logtest.Observe()
		r := &Replayer{logger: logger, config: &config.Config{}}
		results := []models.TestResult{{TestCaseID: "test-2", FailureInfo: models.FailureInfo{UnmatchedCalls: []models.UnmatchedCall{{
			Protocol: "MySQL", ActualSummary: "COM_QUERY SELECT body FROM notes WHERE tag = '" + canary + "'",
			ClosestMock: "mock-auth", NotOwned: true}}}}}
		r.logRecordedIn("test-set-0", results, map[string][]string{"mock-auth": {"test-1"}})
		const msg = "tests made a call that another test recorded: likely, at record time that test made it on a connection it opened, whose handshake was recorded there (KEPLOY_PROVEN=1); each is named under unmatched_calls.recorded_in"
		logtest.Check(t, logs, canary, on, msg)
		shapes, _ := logs.FilterMessage(msg).All()[0].ContextMap()["call_shapes"].([]interface{})
		if want := "test-2: COM_QUERY SELECT body FROM notes WHERE tag = ? (mock-auth, recorded in test-1)"; len(shapes) != 1 || shapes[0] != want {
			t.Fatalf("the line names the calls %v, want [%q]", shapes, want)
		}
		if got := results[0].FailureInfo.UnmatchedCalls[0].RecordedIn; got != "test-1" {
			t.Fatalf("the call is said to be recorded in %q, want test-1", got)
		}
	})
}

// A test whose URL has no scheme and host for the base path to replace fails
// unsent, and the line that says so names the URL without its query, unless
// KEPLOY_LOG_CONTENT is set.
func TestStart_AURLTheBasePathCannotTakeIsLoggedWithoutItsQuery(t *testing.T) {
	logtest.Each(t, func(t *testing.T, on bool) {
		logger, logs := logtest.Observe()
		tc := bpRequest("test-1", closedLocalPort(t), "")
		tc.HTTPReq.URL = "/reset?token=" + canary
		run := newBasePathRunOf(t, "http://localhost:9000", nil, tc)
		run.replayer.logger = logger
		_, _ = run.start(t)
		logtest.Check(t, logs, canary, on, "failed the test without sending it")
		want := strconv.Quote("/reset")
		if on {
			want = strconv.Quote("/reset?token=" + canary)
		}
		if got := run.failureOf("test-1"); !strings.Contains(got, want) {
			t.Fatalf("the report says %q, want it to name the URL as %s", got, want)
		}
	})
}
