//go:build linux

package replay

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// --strict-failure keeps a test whose answer is wrong FAILED when its mock set
// diverged, where it would be demoted to OBSOLETE (resolveTestOutcome, and the
// streamed path's own verdict): its diff is the explanation of that verdict,
// and is printed. It was suppressed as the "re-record" noise of a demoted
// test, so the run failed on a test the console said nothing about. Nor does
// its mock-mismatch line say it is marked obsolete.
func TestRunTestSet_AStrictFailureTestPrintsItsDiff(t *testing.T) {
	const body = `{"orders":[{"id":"ord_LIT","status":"paid"}]}`
	for _, path := range []string{"HTTP", "streamed"} {
		t.Run(path, func(t *testing.T) {
			setSwitch(t, false)
			app := newTmplApp(t, func(a *tmplApp, mux *http.ServeMux) {
				mux.HandleFunc("/orders", func(w http.ResponseWriter, r *http.Request) {
					a.record(r)
					if path == "HTTP" {
						writeJSON(w, http.StatusOK, `{"orders":[{"id":"ord_LIT","status":"refunded"}]}`)
						return
					}
					// The stream's frames match; its status does not.
					w.Header().Set("Content-Type", "application/x-ndjson")
					w.WriteHeader(http.StatusTeapot)
					_, _ = io.WriteString(w, body+"\n")
				})
			})
			tc := tmplCase(1, app.addr, "GET", "/orders", nil, "", recorded(200, nil, body, nil))
			want := "refunded"
			if path == "streamed" {
				tc = tmplCase(1, app.addr, "GET", "/orders", nil, "", models.HTTPResp{
					StatusCode: 200, StatusMessage: "OK",
					Header:     map[string]string{"Content-Type": "application/x-ndjson"},
					Body:       body,
					StreamBody: []models.HTTPStreamChunk{{Data: []models.HTTPStreamDataField{{Key: "raw", Value: body}}}},
				})
				want = "418"
			}
			h := newTmplHarness(t, &tmplConf{}, []*models.TestCase{tc}, func(cfg *config.Config) { cfg.Test.StrictFailure = true })
			// A recorded mock of the test it did not consume: its mock set
			// diverged.
			h.r.mappingDB = &mappedDB{tests: map[string][]models.MockEntry{"test-1": {{Name: "mock-1", Kind: string(models.HTTP)}}}}
			h.r.hookImpl = &consumingHooks{TestHooks: h.r.hookImpl}
			core, logs := observer.New(zap.ErrorLevel)
			h.r.logger = zap.New(core)

			console := captureConsole(t, func() { h.run(t) })
			assertStatus(t, h, "test-1", models.TestStatusFailed, "--strict-failure's verdict")
			if !strings.Contains(console, "Testrun failed for testcase") || !strings.Contains(console, want) {
				t.Fatalf("the console said nothing of why test-1 failed:\n%s", console)
			}
			mismatch := logs.FilterMessageSnippet("mock mapping mismatch detected")
			if mismatch.Len() != 1 || !strings.Contains(mismatch.All()[0].Message, "FAILED") {
				t.Fatalf("the mock-mismatch line does not say test-1 is FAILED: %+v", mismatch.All())
			}
		})
	}
}
