package pkg

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// slowApp is an app whose answer comes only once the test releases it: a
// loaded machine, or an app that waits on a slow dependency.
func slowApp(t *testing.T) (url string, entered <-chan struct{}, release func()) {
	t.Helper()
	in, out := make(chan struct{}, 1), make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		in <- struct{}{}
		<-out
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	var released bool
	rel := func() {
		if !released {
			released = true
			close(out)
		}
	}
	t.Cleanup(func() { rel(); srv.Close() })
	return srv.URL, in, rel
}

// Without a bound of the user's (Unbounded, under the proven switch: row W1)
// a test waits for the app's answer, however long it takes: an answer that
// comes after what used to be keploy's default is the app's answer. With
// one, the bound holds, as before.
func TestAnUnboundedTestWaitsForTheAppsAnswer(t *testing.T) {
	for _, unbounded := range []bool{true, false} {
		url, entered, release := slowApp(t)
		tc := &models.TestCase{Name: "t", Kind: models.HTTP, HTTPReq: models.HTTPReq{Method: "GET", URL: url + "/x"}}
		done := make(chan error, 1)
		go func() {
			_, err := SimulateHTTP(context.Background(), tc, "set", zap.NewNop(), SimulationConfig{APITimeout: 1, Unbounded: unbounded})
			done <- err
		}()
		<-entered
		if unbounded {
			// The app answers past the 1 s APITimeout (induced load).
			time.Sleep(1500 * time.Millisecond)
			release()
			if err := <-done; err != nil {
				t.Fatalf("an unbounded test gave up on the app's late answer: %v", err)
			}
			continue
		}
		if err := <-done; err == nil {
			t.Fatal("a test bounded by APITimeout waited past it")
		}
		release()
	}
}

// The same for a gRPC call: unbounded, the response drain ends on the app's
// end of the stream, however late it comes.
func TestAnUnboundedGRPCTestWaitsForTheAppsEndOfStream(t *testing.T) {
	hold := make(chan struct{})
	addr := startStreamingServer(t, &streamingServer{respond: [][]byte{pbMsg("first")}, holdOpen: hold})
	tc := grpcTestCase(addr, []models.GrpcLengthPrefixedMessage{{}})
	done := make(chan error, 1)
	go func() {
		_, err := SimulateGRPC(context.Background(), tc, "set", zap.NewNop(), SimulationConfig{APITimeout: 1, Unbounded: true})
		done <- err
	}()
	// The app ends its stream past the 1 s APITimeout (induced load).
	time.Sleep(1500 * time.Millisecond)
	close(hold)
	if err := <-done; err != nil {
		t.Fatalf("an unbounded gRPC test gave up on the app's late end of stream: %v", err)
	}
}

// errReadStopped is what a write to a stream the comparison stopped reading
// returns.
var errReadStopped = errors.New("the comparison stopped reading")

// heldOpen is what CompareHTTPStream made of a stream the app holds open past
// its recording, and how many keepalives beyond the recording it read.
type heldOpen struct {
	ok   bool
	info *StreamMismatchInfo
	err  error
	read int
}

// readHeldOpen streams body to CompareHTTPStream and then keepalive, one write
// at a time, as an app that keeps the stream open past its recording does,
// until the comparison stops reading. A write returns once the comparison has
// read it, so read counts the keepalives it took: no clock. One that takes
// every one of limit and still reads is the hang (row W1): the app's stream
// is then ended for it, and the test fails.
func readHeldOpen(t *testing.T, expected models.HTTPResp, cfg HTTPStreamConfig, body, keepalive string, limit int) heldOpen {
	t.Helper()
	pr, pw := io.Pipe()
	done := make(chan heldOpen, 1)
	go func() {
		ok, _, info, err := CompareHTTPStream(expected, pr, cfg, map[string]struct{}{}, zap.NewNop())
		_ = pr.CloseWithError(errReadStopped)
		done <- heldOpen{ok: ok, info: info, err: err}
	}()
	if _, err := pw.Write([]byte(body)); err != nil {
		t.Fatalf("the comparison stopped reading inside the recorded frames: %v", err)
	}
	n := 0
	for ; n < limit; n++ {
		if _, err := pw.Write([]byte(keepalive)); err != nil {
			break
		}
	}
	if n == limit {
		_ = pw.Close()
		<-done
		t.Fatalf("the comparison read %d keepalives beyond the recording and still waited: with no clock it waits as long as the app keeps the stream open", limit)
	}
	got := <-done
	got.read = n
	return got
}

// Under the proven switch, with no clock on the app's answer (row W1), a
// stream the app keeps open past its recording ends on its first keepalive
// beyond the recorded ones: the read does not wait for an end that never
// comes, and the test has no verdict (ErrStreamHeldOpen), never a pass. The
// recorded keepalives are the recording's: an SSE keepalive it holds is one
// of its frames. Bytes beyond a recorded binary stream fail it at once. A
// stream that ends with its recording passes, as before.
func TestAStreamHeldOpenPastItsRecordingEndsOnItsKeepalives(t *testing.T) {
	sse := HTTPStreamConfig{Mode: HTTPStreamModeSSE}
	ndjson := HTTPStreamConfig{Mode: HTTPStreamModeNDJSON}
	sseWithPing := models.HTTPResp{StreamBody: append(frames("data", "a", "b"),
		models.HTTPStreamChunk{Data: []models.HTTPStreamDataField{{Key: "comment", Value: " ping"}}})}
	for _, tc := range []struct {
		name      string
		cfg       HTTPStreamConfig
		expected  models.HTTPResp
		body      string
		keepalive string
	}{
		{"SSE", sse, models.HTTPResp{StreamBody: frames("data", "a", "b")}, "data: a\n\ndata: b\n\n", ": ping\n\n"},
		{"SSE with a recorded keepalive", sse, sseWithPing, "data: a\n\ndata: b\n\n: ping\n\n", ": ping\n\n"},
		{"NDJSON", ndjson, models.HTTPResp{StreamBody: frames("raw", `{"id":1}`, `{"id":2}`)}, "{\"id\":1}\n{\"id\":2}\n", "\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(provenswitch.Env, "1")
			got := readHeldOpen(t, tc.expected, tc.cfg, tc.body, tc.keepalive, 50)
			if !errors.Is(got.err, ErrStreamHeldOpen) {
				t.Fatalf("a stream held open past its recording gave %v (ok %v, %+v), want ErrStreamHeldOpen", got.err, got.ok, got.info)
			}
			if got.read != 1 {
				t.Fatalf("the read ended on keepalive %d beyond the recording, want the first", got.read)
			}
			if !got.ok || got.info != nil {
				t.Fatalf("the recorded frames that came did not match: %+v", got.info)
			}

			// A stream that ends with its recording passes.
			ok, _, info, err := CompareHTTPStream(tc.expected, strings.NewReader(tc.body), tc.cfg, map[string]struct{}{}, zap.NewNop())
			if !ok || err != nil {
				t.Fatalf("a stream that ended with its recording gave %v, %v (%+v)", ok, err, info)
			}

			// Without the switch the comparison stops at the last recorded
			// frame and reads no keepalive, as before.
			t.Setenv(provenswitch.Env, "")
			if got := readHeldOpen(t, tc.expected, tc.cfg, tc.body, tc.keepalive, 50); !got.ok || got.err != nil || got.read != 0 {
				t.Fatalf("without the switch: %+v, want a match that read no keepalive", got)
			}
		})
	}

	t.Run("binary", func(t *testing.T) {
		t.Setenv(provenswitch.Env, "1")
		got := readHeldOpen(t, models.HTTPResp{StreamBody: frames("raw", "abcd")}, HTTPStreamConfig{Mode: HTTPStreamModeBinary}, "abcd", "ef", 50)
		if got.ok || got.err != nil || got.info == nil {
			t.Fatalf("bytes beyond a recorded binary stream gave %+v, want a size mismatch", got)
		}
		if got.read != 1 {
			t.Fatalf("the read ended %d writes beyond the recording, want the first", got.read)
		}
	})
}
