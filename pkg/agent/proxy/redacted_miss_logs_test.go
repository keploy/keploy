package proxy

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	httpint "go.keploy.io/server/v3/pkg/agent/proxy/integrations/http"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// redactedRecording is an HTTP call to api.example whose obfuscator wrote
// REDACTED_abc where the call held a secret, and the pattern for it; it
// states its lifetime, as the recorder writes every HTTP mock.
func redactedRecording(path, contentType, body string) *models.Mock {
	return &models.Mock{Version: "api.keploy.io/v1beta1", Name: "mock-0", Kind: models.HTTP, Spec: models.MockSpec{
		Metadata: map[string]string{"type": models.HTTPClient, "operation": "POST", models.MetadataLifetimeKey: models.MetadataLifetimePerTest},
		HTTPReq: &models.HTTPReq{Method: "POST", URL: "http://api.example" + path, Header: map[string]string{
			"Host": "api.example", "Content-Type": contentType,
		}, Body: body},
		HTTPResp: &models.HTTPResp{StatusCode: 200, Header: map[string]string{"Content-Type": "application/json"}, Body: `{}`},
	}, Noise: []string{`^REDACTED_abc$`}}
}

// A miss of a call that sends a secret where its recording holds a value
// the obfuscator redacted logs no part of the secret: not the mock-mismatch
// line (Warn, Debug under the switch) with its next step, which tells how
// to serve the call to every test at a recording that states its lifetime,
// not the "failed to mock" line, not the HTTP parser's own lines; and the
// misses kept for the test carry none in their reports. Switch off and on.
func TestAMissAtARedactedRecordingLogsNoSecret(t *testing.T) {
	const secret = "zq9Plm3Xw7Rt"
	for _, tc := range []struct {
		name string
		mock *models.Mock
		// path, contentType and body are the call's.
		path, contentType, body string
		parts                   []string
	}{
		// A secret path segment (with a %2F in it too), and a body field
		// under the redacted field's key, one level down.
		{"a path segment and a moved field", redactedRecording("/x/REDACTED_abc/y", "application/json", `{"assertion":"REDACTED_abc","x":"alpha1"}`),
			"/x/zq9Plm%2F3Xw7Rt/y", "application/json", `{"data":{"assertion":"` + secret + `"},"x":"bravo2"}`, []string{"zq9Plm", "3Xw7Rt"}},
		// With the switch off the request matcher serves an XML body
		// that changed: this call goes to another path too.
		{"an XML element", redactedRecording("/x", "application/xml", `<req><assertion>REDACTED_abc</assertion><x>alpha1</x></req>`),
			"/x/other", "application/xml", `<req><assertion>` + secret + `</assertion><x>bravo2</x></req>`, []string{secret}},
		// A JSON body that is no object, at a recorded object: the matcher
		// logs, at Error, that it does not read as one.
		{"a JSON body that is no object", redactedRecording("/x", "application/json", `{"assertion":"REDACTED_abc","x":"alpha1"}`),
			"/x", "application/json", `[{"assertion":"` + secret + `","x":"bravo2"}]`, []string{secret}},
	} {
		for _, on := range []bool{false, true} {
			mode, env := "off", ""
			if on {
				mode, env = "on", "1"
			}
			t.Run(tc.name+"/"+mode, func(t *testing.T) {
				t.Setenv(provenswitch.Env, env)
				const dst = "10.96.0.9:80"
				app, src := acceptedConn(t)
				p := replayProxyTo(t, dst, models.OutgoingOptions{}, tc.mock)
				core, logs := observer.New(zap.DebugLevel)
				p.logger = zap.New(core)
				p.errChannel = make(chan error, 4)
				p.Integrations = map[integrations.IntegrationType]integrations.Integrations{integrations.HTTP: httpint.New(zap.New(core))}
				p.integrationsPriority = []ParserPriority{{Priority: 1, ParserType: integrations.HTTP}}
				done := make(chan error, 1)
				go func() { done <- p.handleConnection(context.Background(), src) }()
				call := "POST " + tc.path + " HTTP/1.1\r\nHost: api.example\r\nContent-Type: " + tc.contentType + "\r\nContent-Length: " + strconv.Itoa(len(tc.body)) + "\r\n\r\n" + tc.body
				if _, err := app.Write([]byte(call)); err != nil {
					t.Fatal(err)
				}
				// keploy answers the call it has no mock for, or closes:
				// either way the miss was reported by then.
				if resp, err := http.ReadResponse(bufio.NewReader(app), nil); err == nil {
					_ = resp.Body.Close()
				}
				_ = app.Close()
				_ = awaitEvent(t, done, "handleConnection returning after the client left")

				if n := logs.FilterMessageSnippet("mock mismatch: no matching mock for outgoing call").Len(); n != 1 {
					t.Fatalf("mismatch log lines = %d, want 1: %v", n, logs.All())
				}
				for _, e := range logs.All() {
					for _, s := range tc.parts {
						if strings.Contains(e.Message, s) {
							t.Errorf("log %q carries %q", e.Message, s)
						}
						for k, v := range e.ContextMap() {
							if strings.Contains(fmt.Sprint(v), s) {
								t.Errorf("log %q field %s carries %q: %v", e.Message, k, s, v)
							}
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
				for _, s := range tc.parts {
					if strings.Contains(string(all), s) {
						t.Errorf("the miss kept for the test carries %q: %s", s, all)
					}
				}
				// The row reaches the hint: the next step names the call's
				// path prefix (with the switch off only).
				if !on && !strings.Contains(kept[0].NextSteps, "pathPrefix:") {
					t.Errorf("the miss does not tell how to serve the call to every test: %s", kept[0].NextSteps)
				}
			})
		}
	}
}
