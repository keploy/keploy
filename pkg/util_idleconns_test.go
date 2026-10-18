package pkg

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// keepAliveApp is an app that keeps each connection open between requests
// for as long as its client does (HTTP/1.1 keep-alive, no idle timeout of
// its own), as Python's http.server under a ThreadingTCPServer does, and
// says when one of its connections closes. It answers with status; a stream
// it holds open, after its first event, until its client goes.
func keepAliveApp(t *testing.T, stream, holdOpen bool, status int) (url string, closed <-chan struct{}) {
	t.Helper()
	ch := make(chan struct{}, 16)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if stream {
			w.Header().Set("Content-Type", "text/event-stream")
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, "data: ok\n\n")
		if holdOpen {
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		}
	}))
	srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateClosed {
			ch <- struct{}{}
		}
	}
	srv.Start()
	t.Cleanup(srv.Close)
	return srv.URL, ch
}

// Each test request keploy sends the app is made by a client of its own, and
// the connection it opens closes once the answer has been read: no later
// request uses that client, so a connection it kept would stay open to the
// app for the rest of keploy's run, one for each test, and an app that waits
// for its open connections as it stops (Python's ThreadingTCPServer joins
// each one's thread) could not stop: keploy's stop at a set's end then killed
// it, and the set was not judged. So for a recorded request that asks to keep
// the connection alive and for one that says nothing, through SimulateHTTP
// and through SimulateHTTPStreaming; for an answer of another status; and for
// a stream closed before its end and held open by the app, as the comparison
// closes one it stopped reading (a mismatch, a stall, a cancelled run). A
// non-streamed answer is
// read whole and closed inside SimulateHTTP, before it is compared, so its
// comparison's outcome, a mismatch included, cannot leave it open.
func TestATestRequestsConnectionClosesOnceItsAnswerIsRead(t *testing.T) {
	for _, tc := range []struct {
		name       string
		connection string
		stream     bool
		status     int
		partial    bool
	}{
		{"keep-alive", "keep-alive", false, http.StatusOK, false},
		{"no Connection header", "", false, http.StatusOK, false},
		{"keep-alive, another status", "keep-alive", false, http.StatusInternalServerError, false},
		{"keep-alive, streamed", "keep-alive", true, http.StatusOK, false},
		{"no Connection header, streamed", "", true, http.StatusOK, false},
		{"keep-alive, streamed, another status", "keep-alive", true, http.StatusBadGateway, false},
		{"keep-alive, a stream closed before its end", "keep-alive", true, http.StatusOK, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			url, closed := keepAliveApp(t, tc.stream, tc.partial, tc.status)
			header := map[string]string{}
			if tc.connection != "" {
				header["Connection"] = tc.connection
			}
			test := &models.TestCase{Name: "t", Kind: models.HTTP, HTTPReq: models.HTTPReq{Method: "GET", URL: url + "/x", Header: header}}
			cfg := SimulationConfig{APITimeout: 5}
			if tc.stream {
				resp, err := SimulateHTTPStreaming(context.Background(), test, "set", zap.NewNop(), cfg)
				if err != nil {
					t.Fatal(err)
				}
				if tc.partial {
					if _, err := resp.Reader.Read(make([]byte, 4)); err != nil {
						t.Fatal(err)
					}
				} else if _, err := io.ReadAll(resp.Reader); err != nil {
					t.Fatal(err)
				}
				if err := resp.Reader.Close(); err != nil {
					t.Fatal(err)
				}
			} else if _, err := SimulateHTTP(context.Background(), test, "set", zap.NewNop(), cfg); err != nil {
				t.Fatal(err)
			}
			// The bound only turns a connection left open into a failure.
			select {
			case <-closed:
			case <-time.After(30 * time.Second):
				t.Fatal("the connection keploy's test request opened is still open to the app once its answer was read")
			}
		})
	}
}

// A stream closed before its end whose rest has come already: net/http drains
// the rest after the Close and keeps the connection (Go 1.27,
// maxPostCloseReadTime); only the client's CloseIdleConnections closes it.
func TestATestRequestsStreamClosedBeforeItsEndClosesWithItsRestCome(t *testing.T) {
	url, closed := keepAliveApp(t, true, false, http.StatusOK)
	test := &models.TestCase{Name: "t", Kind: models.HTTP, HTTPReq: models.HTTPReq{Method: "GET", URL: url + "/x", Header: map[string]string{"Connection": "keep-alive"}}}
	resp, err := SimulateHTTPStreaming(context.Background(), test, "set", zap.NewNop(), SimulationConfig{APITimeout: 5})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := resp.Reader.Read(make([]byte, 4)); err != nil {
		t.Fatal(err)
	}
	if err := resp.Reader.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-closed:
	case <-time.After(30 * time.Second):
		t.Fatal("a stream closed before its end, its rest come, left its connection open to the app")
	}
}

// Under the proven switch with no apiTimeout the answer watch wraps the body
// (answerBody, answerReader); its Close reaches connClosingBody all the same.
func TestATestRequestsConnectionClosesUnderTheAnswerWatch(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	for _, stream := range []bool{false, true} {
		url, closed := keepAliveApp(t, stream, false, http.StatusOK)
		test := &models.TestCase{Name: "t", Kind: models.HTTP, HTTPReq: models.HTTPReq{Method: "GET", URL: url + "/x", Header: map[string]string{"Connection": "keep-alive"}}}
		cfg := SimulationConfig{APITimeout: 5, Unbounded: true}
		if stream {
			resp, err := SimulateHTTPStreaming(context.Background(), test, "set", zap.NewNop(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := io.ReadAll(resp.Reader); err != nil {
				t.Fatal(err)
			}
			if err := resp.Reader.Close(); err != nil {
				t.Fatal(err)
			}
		} else if _, err := SimulateHTTP(context.Background(), test, "set", zap.NewNop(), cfg); err != nil {
			t.Fatal(err)
		}
		select {
		case <-closed:
		case <-time.After(30 * time.Second):
			t.Fatalf("streamed %v: under the answer watch the connection is still open to the app", stream)
		}
	}
}
