package proxy

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	httpint "go.keploy.io/server/v3/pkg/agent/proxy/integrations/http"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// A per-test recording that redacted a secret answers a call, and the same
// call comes again and misses: the miss's report and every log line still
// show the secret redacted, though a read of the pool at the miss no longer
// holds the recording (an answered per-test recording leaves it). The call
// comes again in the same test, with the pool empty or holding only a
// recording of another call that redacted nothing, and in the next test of
// the set, whose staging no longer holds the recording; switch off and on,
// and under exact serving.
func TestARepeatOfACallAnAnsweredRedactingRecordingServedStaysRedacted(t *testing.T) {
	const secret = "zq9Plm3Xw7Rt"
	t0 := time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC)
	recording := func() *models.Mock {
		m := redactedRecording("/x", "application/json", `{"assertion":"REDACTED_abc","x":"alpha1"}`)
		m.Spec.HTTPResp.Body = `{"from":"recording"}`
		m.Spec.HTTPResp.Header["Content-Length"] = strconv.Itoa(len(m.Spec.HTTPResp.Body))
		m.Spec.ReqTimestampMock, m.Spec.ResTimestampMock = t0, t0.Add(time.Millisecond)
		m.TestModeInfo = models.TestModeInfo{ID: 1, SortOrder: 1, IsFiltered: true, Lifetime: models.LifetimePerTest}
		return m
	}
	// other is a session recording of another call, which redacted nothing.
	other := func() *models.Mock {
		return &models.Mock{Version: "api.keploy.io/v1beta1", Name: "mock-s", Kind: models.HTTP, Spec: models.MockSpec{
			Metadata:         map[string]string{"type": models.HTTPClient, "operation": "GET", models.MetadataLifetimeKey: models.MetadataLifetimeSession},
			HTTPReq:          &models.HTTPReq{Method: "GET", URL: "http://api.example/other", Header: map[string]string{"Host": "api.example"}},
			HTTPResp:         &models.HTTPResp{StatusCode: 200, Header: map[string]string{"Content-Type": "application/json"}, Body: `{}`},
			ReqTimestampMock: t0, ResTimestampMock: t0.Add(time.Millisecond),
		}, TestModeInfo: models.TestModeInfo{ID: 2, SortOrder: 2, Lifetime: models.LifetimeSession}}
	}
	type scenario struct {
		name  string
		exact bool // under exact serving only (the switch on)
		// shown: the miss renders the call (it has a closest recording).
		shown bool
		// stage stages the first test with rec, and returns what stages
		// the pool the repeat is made in.
		stage func(mm *MockManager, rec *models.Mock) (again func())
	}
	scenarios := []scenario{
		{"in the test, nothing left", false, false, func(mm *MockManager, rec *models.Mock) func() {
			mm.SetMocksWithWindow([]*models.Mock{rec}, nil, t0.Add(-time.Second), t0.Add(time.Second))
			return func() {}
		}},
		{"in the test, another call's recording left", false, true, func(mm *MockManager, rec *models.Mock) func() {
			mm.SetMocksWithWindow([]*models.Mock{rec}, []*models.Mock{other()}, t0.Add(-time.Second), t0.Add(time.Second))
			return func() {}
		}},
		// The agent stages the next test less what answered before
		// (filterOutDeleted).
		{"in the next test", false, true, func(mm *MockManager, rec *models.Mock) func() {
			mm.SetMocksWithWindow([]*models.Mock{rec}, []*models.Mock{other()}, t0.Add(-time.Second), t0.Add(time.Second))
			return func() {
				mm.SetMocksWithWindow(nil, []*models.Mock{other()}, t0.Add(5*time.Second), t0.Add(15*time.Second))
			}
		}},
		{"exact serving, in the test", true, true, func(mm *MockManager, rec *models.Mock) func() {
			mm.SetMocksWithWindow([]*models.Mock{rec}, []*models.Mock{other()}, models.BaseTime, time.Now())
			mm.ServeExactTest("T1", []*models.Mock{rec})
			return func() {}
		}},
		{"exact serving, in the next test", true, true, func(mm *MockManager, rec *models.Mock) func() {
			mm.SetMocksWithWindow([]*models.Mock{rec}, []*models.Mock{other()}, models.BaseTime, time.Now())
			mm.ServeExactTest("T1", []*models.Mock{rec})
			return func() { mm.ServeExactTest("T2", nil) }
		}},
	}
	for _, sc := range scenarios {
		for _, on := range []bool{false, true} {
			if sc.exact && !on {
				continue
			}
			mode, env := "off", ""
			if on {
				mode, env = "on", "1"
			}
			t.Run(sc.name+"/"+mode, func(t *testing.T) {
				t.Setenv(provenswitch.Env, env)
				const dst = "10.96.0.9:80"
				p := replayProxyTo(t, dst, models.OutgoingOptions{})
				mm := NewMockManager(nil, nil, zap.NewNop())
				t.Cleanup(mm.Close)
				p.mockManager = mm
				again := sc.stage(mm, recording())
				core, logs := observer.New(zap.DebugLevel)
				p.logger = zap.New(core)
				p.errChannel = make(chan error, 4)
				p.Integrations = map[integrations.IntegrationType]integrations.Integrations{integrations.HTTP: httpint.New(zap.New(core))}
				p.integrationsPriority = []ParserPriority{{Priority: 1, ParserType: integrations.HTTP}}
				body := `{"assertion":"` + secret + `","x":"alpha1"}`
				send := func() (int, string) {
					app, src := acceptedConn(t)
					done := make(chan error, 1)
					go func() { done <- p.handleConnection(context.Background(), src) }()
					call := "POST /x HTTP/1.1\r\nHost: api.example\r\nContent-Type: application/json\r\nContent-Length: " + strconv.Itoa(len(body)) + "\r\n\r\n" + body
					if _, err := app.Write([]byte(call)); err != nil {
						t.Fatal(err)
					}
					status, got := 0, ""
					if resp, err := http.ReadResponse(bufio.NewReader(app), nil); err == nil {
						b, _ := io.ReadAll(resp.Body)
						_ = resp.Body.Close()
						status, got = resp.StatusCode, string(b)
					}
					_ = app.Close()
					_ = awaitEvent(t, done, "handleConnection returning after the client left")
					return status, got
				}
				if status, got := send(); status != 200 || got != `{"from":"recording"}` {
					t.Fatalf("the redacting recording did not answer the first call: %d %q; logs: %v", status, got, logs.All())
				}
				again()
				if status, got := send(); got == `{"from":"recording"}` {
					t.Fatalf("the repeat was answered by the recording again (%d)", status)
				}

				if n := logs.FilterMessageSnippet("mock mismatch: no matching mock for outgoing call").Len(); n != 1 {
					t.Fatalf("mismatch log lines = %d, want 1: %v", n, logs.All())
				}
				for _, e := range logs.All() {
					if strings.Contains(e.Message, secret) {
						t.Errorf("log %q carries the secret", e.Message)
					}
					for k, v := range e.ContextMap() {
						if strings.Contains(fmt.Sprint(v), secret) {
							t.Errorf("log %q field %s carries the secret: %v", e.Message, k, v)
						}
					}
				}
				var kept []*models.MockMismatchReport
				for len(p.errChannel) > 0 {
					var pe models.ParserError
					if err := <-p.errChannel; errors.As(err, &pe) && pe.MismatchReport != nil {
						kept = append(kept, pe.MismatchReport)
					}
				}
				if len(kept) != 1 {
					t.Fatalf("misses kept for the test = %d, want 1", len(kept))
				}
				all, err := json.Marshal(kept[0])
				if err != nil {
					t.Fatal(err)
				}
				if strings.Contains(string(all), secret) {
					t.Errorf("the miss kept for the test carries the secret: %s", all)
				}
				// Precision: the rest of the call still shows.
				if sc.shown && !strings.Contains(string(all), "alpha1") {
					t.Errorf("the miss kept for the test does not show the call's other field: %s", all)
				}
			})
		}
	}
}
