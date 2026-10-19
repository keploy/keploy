package pkg

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils/logcontent/logtest"
)

// canary is the app's: in a streaming test's request, and in the frames it
// was recorded with and answers with.
const canary = "kp-canary-stream-0e6d"

func rawChunks(lines ...string) []models.HTTPStreamChunk {
	out := make([]models.HTTPStreamChunk, len(lines))
	for i, l := range lines {
		out[i] = models.HTTPStreamChunk{Data: []models.HTTPStreamDataField{{Key: "raw", Value: l}}}
	}
	return out
}

// A streaming test is sent, and its frames compared, with lines that carry
// neither the request nor the frames, unless KEPLOY_LOG_CONTENT is set.
func TestSimulateHTTPStreaming_TheLinesCarryNoRequestOrFrame(t *testing.T) {
	logtest.Each(t, func(t *testing.T, on bool) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("id:1\ndata:{\"note\":\"" + canary + "-live\"}\n\n"))
		}))
		defer server.Close()
		tc := &models.TestCase{
			Name: "sse-notes",
			HTTPReq: models.HTTPReq{Method: "GET", URL: server.URL + "/events?ref=" + canary,
				Header: map[string]string{"Accept": "text/event-stream", "Authorization": "Bearer " + canary}},
			HTTPResp: models.HTTPResp{
				Header: map[string]string{"Content-Type": "text/event-stream"},
				Body:   "id:1\ndata:{\"note\":\"" + canary + "-recorded\"}\n",
				StreamBody: []models.HTTPStreamChunk{{Data: []models.HTTPStreamDataField{
					{Key: "id", Value: "1"}, {Key: "data", Value: `{"note":"` + canary + `-recorded"}`},
				}}},
			},
		}
		logger, logs := logtest.Observe()
		resp, err := SimulateHTTPStreaming(context.Background(), tc, "test-set", logger, SimulationConfig{APITimeout: 3})
		if err != nil {
			t.Fatal(err)
		}
		matched, _, _, err := CompareHTTPStream(tc.HTTPResp, resp.Reader, resp.StreamConfig, map[string]struct{}{}, logger)
		_ = resp.Reader.Close()
		if err != nil || matched {
			t.Fatalf("the stream matched (%v), want its frame's mismatch", err)
		}
		logtest.Check(t, logs, canary, on, "Sending streaming request to user app", "SSE frame mismatch")
	})
}

// Each kind of stream compares its frames with lines that carry neither the
// recorded nor the live one, unless KEPLOY_LOG_CONTENT is set.
func TestCompareHTTPStream_AMismatchIsLoggedWithoutItsFrames(t *testing.T) {
	for name, tc := range map[string]struct {
		cfg      HTTPStreamConfig
		expected models.HTTPResp
		live     string
		want     string
	}{
		"ndjson": {HTTPStreamConfig{Mode: HTTPStreamModeNDJSON},
			models.HTTPResp{StreamBody: rawChunks(`{"note":"` + canary + `-recorded"}`)},
			`{"note":"` + canary + `-live"}` + "\n", "NDJSON stream mismatch"},
		"plain text": {HTTPStreamConfig{Mode: HTTPStreamModePlainText},
			models.HTTPResp{StreamBody: rawChunks(canary + "-recorded")},
			canary + "-live\n", "plain-text stream mismatch"},
	} {
		t.Run(name, func(t *testing.T) {
			logtest.Each(t, func(t *testing.T, on bool) {
				logger, logs := logtest.Observe()
				matched, _, _, err := CompareHTTPStream(tc.expected, strings.NewReader(tc.live), tc.cfg, map[string]struct{}{}, logger)
				if err != nil || matched {
					t.Fatalf("the stream matched (%v), want its frame's mismatch", err)
				}
				logtest.Check(t, logs, canary, on, tc.want)
			})
		})
	}
}

// A test's target is logged without its URL's query, as replaceWith rewrites
// it and as the scheme's port is taken for it, unless KEPLOY_LOG_CONTENT is
// set.
func TestResolveTestTarget_TheTargetIsLoggedWithoutItsQuery(t *testing.T) {
	logtest.Each(t, func(t *testing.T, on bool) {
		logger, logs := logtest.Observe()
		if _, err := ResolveTestTarget("http://recorded.local/notes?ref="+canary, map[string]string{"recorded.local": "app.local"}, nil, "", 0, 0, true, logger); err != nil {
			t.Fatal(err)
		}
		logtest.Check(t, logs, canary, on, "Applied replaceWith substitution", "URL has no explicit port, using scheme default", "Final resolved target")
	})
}
