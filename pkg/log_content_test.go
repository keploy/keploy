package pkg

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/utils/logcontent/logtest"
	"go.uber.org/zap"
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

// A test request the app refuses fails with net/http's error, which names the
// request's URL: the lines that say so, and the error returned for the replay
// to log, name it without its query, unless KEPLOY_LOG_CONTENT is set. The
// error still reads as a refusal. The line that says the request is sent
// names its headers, not their values.
func TestSimulateHTTP_ARefusedRequestIsLoggedWithoutItsQuery(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	addr := l.Addr().String()
	_ = l.Close() // the port now refuses
	tc := &models.TestCase{Name: "reset", Kind: models.HTTP,
		HTTPReq: models.HTTPReq{Method: "GET", URL: "http://" + addr + "/reset?token=" + canary,
			Header: map[string]string{"Authorization": "Bearer " + canary}}}
	for name, send := range map[string]struct {
		sent string
		send func(*zap.Logger) error
	}{
		"SimulateHTTP": {"Sending request to user app", func(logger *zap.Logger) error {
			_, err := SimulateHTTP(context.Background(), tc, "test-set", logger, SimulationConfig{APITimeout: 3})
			return err
		}},
		"SimulateHTTPStreaming": {"Sending streaming request to user app", func(logger *zap.Logger) error {
			_, err := SimulateHTTPStreaming(context.Background(), tc, "test-set", logger, SimulationConfig{APITimeout: 3})
			return err
		}},
	} {
		t.Run(name, func(t *testing.T) {
			logtest.Each(t, func(t *testing.T, on bool) {
				logger, logs := logtest.Observe()
				err := send.send(logger)
				if !isPreResponseConnRefused(err) {
					t.Fatalf("%s = %v; want the app's refusal", name, err)
				}
				logtest.Check(t, logs, canary, on, send.sent,
					"test request refused by app (app not yet accepting connections); re-sending — the request was never processed, so this is not a retry of an assertion",
					"failed to send testcase request to app")
				if got := strings.Contains(err.Error(), canary); got != on {
					t.Fatalf("the error the replay logs carries the query: %v, want %v: %v", got, on, err)
				}
				if !strings.Contains(err.Error(), addr+"/reset") {
					t.Fatalf("the error does not name the request: %v", err)
				}
				sent := logs.FilterMessage(send.sent).All()[0].ContextMap()
				if names := sent["header_names"]; !slices.Contains(names.([]interface{}), "Authorization") {
					t.Fatalf("the line that says the request is sent names the headers %v, want Authorization among them", names)
				}
				// fmt's print of a request holds its pointers, so with the
				// switch off it is left out, not fingerprinted.
				if _, ok := sent["request"]; ok != on {
					t.Fatalf("the line that says the request is sent has a request field: %v, want %v", ok, on)
				}
			})
		})
	}
}

// Resolving a test's target costs a Debug level that is off nothing for the
// switch: it runs for every test request sent.
func TestResolveTestTarget_TheSwitchCostsADisabledLevelNothing(t *testing.T) {
	logger := logtest.Disabled()
	logtest.CheckDisabledCost(t, func() {
		if _, err := ResolveTestTarget("http://127.0.0.1:8080/notes?ref="+canary, nil, nil, "", 0, 0, true, logger); err != nil {
			t.Fatal(err)
		}
	})
}

// Under the stall rule, a test the app stops answering fails with an error
// that names its request, which the replay logs: by its path, without the
// query, unless KEPLOY_LOG_CONTENT is set. A plain answer that never comes, a
// stream that sends nothing past its headers, and one that sends keepalives
// alone, which the stream's comparison names.
func TestSimulateHTTP_AStalledAnswerNamesItsRequestWithoutItsQuery(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	hold := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/events":
			w.Header().Set("Content-Type", "text/event-stream")
			w.(http.Flusher).Flush()
		case "/pings":
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte(": ping\n\n"))
			w.(http.Flusher).Flush()
		}
		select {
		case <-hold:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(hold) })
	cfg := SimulationConfig{APITimeout: 1, Unbounded: true, StallGrace: 300 * time.Millisecond}
	stream := func(tc *models.TestCase) error {
		resp, err := SimulateHTTPStreaming(context.Background(), tc, "test-set", zap.NewNop(), cfg)
		if err != nil {
			return err
		}
		defer resp.Reader.Close()
		_, _, _, err = CompareHTTPStream(tc.HTTPResp, resp.Reader, resp.StreamConfig, map[string]struct{}{}, zap.NewNop())
		return err
	}
	frame := []models.HTTPStreamChunk{{Data: []models.HTTPStreamDataField{{Key: "data", Value: "a"}}}}
	for name, tc := range map[string]struct {
		path   string
		frames []models.HTTPStreamChunk
		send   func(*models.TestCase) error
	}{
		"an answer that never comes": {"/never", frame, func(tc *models.TestCase) error {
			_, err := SimulateHTTP(context.Background(), tc, "test-set", zap.NewNop(), cfg)
			return err
		}},
		"a stream that sends nothing":  {"/events", frame, stream},
		"a stream of keepalives alone": {"/pings", nil, stream},
	} {
		t.Run(name, func(t *testing.T) {
			logtest.Each(t, func(t *testing.T, on bool) {
				c := &models.TestCase{Name: "stalled", Kind: models.HTTP,
					HTTPReq: models.HTTPReq{Method: "GET", URL: srv.URL + tc.path + "?token=" + canary},
					HTTPResp: models.HTTPResp{StatusCode: 200, Header: map[string]string{"Content-Type": "text/event-stream"},
						StreamBody: tc.frames}}
				err := tc.send(c)
				if !errors.Is(err, ErrAnswerStalled) || !strings.Contains(err.Error(), "for GET "+tc.path) {
					t.Fatalf("%v; want a stalled answer that names GET %s", err, tc.path)
				}
				if got := strings.Contains(err.Error(), canary); got != on {
					t.Fatalf("the error the replay logs carries the query: %v, want %v: %v", got, on, err)
				}
			})
		})
	}
}
