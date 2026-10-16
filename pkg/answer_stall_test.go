package pkg

import (
	"compress/gzip"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"testing/iotest"
	"time"

	"github.com/andybalholm/brotli"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/utils/stall"
	"go.uber.org/zap"
	"google.golang.org/grpc"
)

// waitArmed waits for w to arm its next look at the stall on clk, or to have
// cut the exchange, after which it looks no more.
func waitArmed(t *testing.T, clk *stall.ManualClock, w *answerWatch) {
	t.Helper()
	for clk.Pending() == 0 && !w.didStall() {
		time.Sleep(time.Millisecond)
	}
}

// The stall rule is on only under the proven switch with no bound of the
// user's (Unbounded): otherwise nothing is watched.
func TestTheAnswerStallRuleIsOnOnlyWithNoBoundOfTheUsers(t *testing.T) {
	t.Setenv(provenswitch.Env, "")
	if w := newAnswerWatch(SimulationConfig{Unbounded: true}); w != nil {
		t.Fatal("a watch without the switch")
	}
	t.Setenv(provenswitch.Env, "1")
	if w := newAnswerWatch(SimulationConfig{}); w != nil {
		t.Fatal("a watch with the user's own bound")
	}
	w := newAnswerWatch(SimulationConfig{Unbounded: true, StallClock: &stall.ManualClock{}})
	if w == nil {
		t.Fatal("no watch under the switch with no bound of the user's")
	}
	w.stop()
}

// The rule measures, in keploy's runnable time, how long keploy waited on the
// app with no progress: a byte of the answer, or of the request the app made
// room for, starts it again; the grace passing with neither, while keploy
// waits, cuts the exchange. Wall time passing while keploy could not run (a
// starved host) cuts nothing, and neither does time keploy spent not waiting
// on the app (a stream it is slow to read).
func TestTheAnswerStallRuleCutsOnlyAGraceOfRunnableTimeWaitedWithNoProgress(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	clk := &stall.ManualClock{}
	w := newAnswerWatch(SimulationConfig{Unbounded: true, StallGrace: 10 * time.Second, StallClock: clk})
	defer w.stop()
	var cuts atomic.Int32
	w.onCut(func() { cuts.Add(1) })
	keploy, app := net.Pipe()
	defer app.Close()
	w.closeOnCut(keploy)
	ctx := w.bind(context.Background())

	// Keploy does not wait on the app: no time accrues, however long.
	waitArmed(t, clk, w)
	for i := 0; i < 5; i++ {
		clk.Advance(9 * time.Second)
		waitArmed(t, clk, w)
	}
	if w.didStall() {
		t.Fatal("cut while keploy was not waiting on the app")
	}

	done := w.waiting()
	waitArmed(t, clk, w)
	clk.Advance(9 * time.Second)
	waitArmed(t, clk, w)
	for i := 0; i < 50; i++ {
		clk.Pass() // wall time, keploy starved
		waitArmed(t, clk, w)
	}
	if w.didStall() {
		t.Fatal("cut before the grace of runnable time passed")
	}

	// A byte of the answer: the stall starts again.
	w.gotAnswer(1)
	clk.Advance(9 * time.Second)
	waitArmed(t, clk, w)
	if w.didStall() {
		t.Fatal("a byte of the answer did not start the stall again")
	}

	// A byte of the request taken: progress too.
	w.sentBody(1)
	w.progress()
	clk.Advance(9 * time.Second)
	waitArmed(t, clk, w)
	if w.didStall() {
		t.Fatal("a request byte taken did not start the stall again")
	}

	// The wait ends, and keploy is slow to wait again (a stream it reads
	// late): that time is not the app's, and a wait that begins after it
	// starts the stall again.
	done()
	clk.Advance(30 * time.Second)
	waitArmed(t, clk, w)
	done = w.waiting()
	defer done()
	clk.Advance(9 * time.Second)
	waitArmed(t, clk, w)
	if w.didStall() {
		t.Fatal("the time keploy did not wait on the app was counted")
	}

	clk.Advance(time.Second)
	for !w.didStall() {
		time.Sleep(time.Millisecond)
	}
	if n := cuts.Load(); n != 1 {
		t.Fatalf("cut %d times, want once", n)
	}
	if ctx.Err() == nil {
		t.Fatal("the cut left the request's context live")
	}
	if _, err := keploy.Read(make([]byte, 1)); err == nil {
		t.Fatal("the cut left the connection open")
	}
	// A connection of the exchange that comes after the cut (dialled
	// late) is closed at once.
	late, lateApp := net.Pipe()
	defer lateApp.Close()
	w.closeOnCut(late)
	if _, err := late.Read(make([]byte, 1)); err == nil {
		t.Fatal("a connection added after the cut was left open")
	}
}

// A clean end of a stream (io.EOF) is the app's end, never taken for the
// cut, even after it: the cut's own error is never io.EOF. Any other error
// after the cut is what the rule waited for.
func TestAStreamsCleanEndIsNeverTakenForTheCut(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	clk := &stall.ManualClock{}
	w := newAnswerWatch(SimulationConfig{Unbounded: true, StallGrace: time.Second, StallClock: clk})
	defer w.stop()
	done := w.waiting()
	waitArmed(t, clk, w)
	clk.Advance(time.Second)
	for !w.didStall() {
		time.Sleep(time.Millisecond)
	}
	done()
	ended := &answerReader{ReadCloser: io.NopCloser(strings.NewReader("")), w: w}
	if _, err := ended.Read(make([]byte, 1)); err != io.EOF {
		t.Fatalf("%v; want the app's own end, io.EOF", err)
	}
	cut := &answerReader{ReadCloser: io.NopCloser(iotest.ErrReader(context.Canceled)), w: w}
	if _, err := cut.Read(make([]byte, 1)); !errors.Is(err, ErrAnswerStalled) {
		t.Fatalf("%v; want what the rule waited for", err)
	}
}

// What the rule waited for: a request the app stopped taking (how much of it
// had gone), an answer that never came, its headers alone, one stopped
// part-way. None of them has anything to compare: ErrAnswerStalled. A stream
// cut after some of its frames came is an ErrStreamHeldOpen, saying what of
// its recording came; one none of whose frames came is an ErrAnswerStalled.
func TestTheAnswerStallSaysWhatItWaitedFor(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	clk := &stall.ManualClock{}
	w := newAnswerWatch(SimulationConfig{Unbounded: true, StallGrace: 10 * time.Second, StallClock: clk})
	defer w.stop()
	w.describe("POST /upload", 1000)
	w.sentBody(300)
	defer w.waiting()()
	waitArmed(t, clk, w)
	clk.Advance(10 * time.Second)
	for !w.didStall() {
		time.Sleep(time.Millisecond)
	}
	err := w.err()
	if !errors.Is(err, ErrAnswerStalled) || errors.Is(err, ErrStreamHeldOpen) {
		t.Fatalf("%v: want an ErrAnswerStalled", err)
	}
	for _, want := range []string{"stopped taking the test's request", "at most 300 of its 1000 bytes; 700 were never taken", "10s of keploy's runnable time", "POST /upload"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("%q does not say %q", err, want)
		}
	}

	for _, c := range []struct {
		e    *AnswerStallError
		want string
	}{
		{&AnswerStallError{Target: "GET /x", Waited: time.Minute}, "the app never answered; waited 1m0s of keploy's runnable time for GET /x"},
		{&AnswerStallError{Target: "GET /x", Waited: time.Minute, Answered: true}, "the app sent its answer's headers and nothing of its body; waited 1m0s of keploy's runnable time for GET /x"},
		{&AnswerStallError{Target: "GET /x", Waited: time.Minute, Answered: true, Read: 42}, "the app's answer stopped part-way, after 42 bytes of its body; waited 1m0s of keploy's runnable time for GET /x"},
		{keptOpenWithNothing("/pkg.Svc/Method", false, 0), "the app never answered, and sent only keepalives, for /pkg.Svc/Method"},
		{keptOpenWithNothing("", true, 0), "the app sent its answer's headers and nothing of its body, then only keepalives"},
		{keptOpenWithNothing("GET /e", true, 9), "the app's answer stopped part-way, after 9 bytes of its body, then only keepalives, for GET /e"},
		{keptOpenWithKeepalivesOnly(), "the app sent its answer's headers and nothing of its body but keepalives"},
		{&AnswerStallError{Target: "POST /x", Waited: time.Minute, Answered: true, Untaken: true, Sent: 3, Request: 10}, "the app sent its answer's headers and stopped taking the test's request (it took at most 3 of its 10 bytes; 7 were never taken); waited 1m0s of keploy's runnable time for POST /x"},
		{&AnswerStallError{Target: "POST /chunked", Waited: time.Minute, Untaken: true, Sent: 7}, "the app stopped taking the test's request and never answered (it took at most 7 bytes of it); waited 1m0s of keploy's runnable time for POST /chunked"},
	} {
		if got := c.e.Error(); got != c.want {
			t.Fatalf("%q, want %q", got, c.want)
		}
		if !errors.Is(c.e, ErrAnswerStalled) || errors.Is(c.e, ErrStreamHeldOpen) {
			t.Fatalf("%q: want an ErrAnswerStalled: nothing to compare", c.want)
		}
	}
	answered := &AnswerStallError{Target: "GET /events", Waited: time.Minute, Answered: true}
	stream := streamCut(answered, 1, 3, "frame")
	if !errors.Is(stream, ErrStreamHeldOpen) || errors.Is(stream, ErrAnswerStalled) || !strings.Contains(stream.Error(), "1 of the 3 recorded frames came") {
		t.Fatalf("%v: want an ErrStreamHeldOpen saying what came", stream)
	}
	past := streamCut(answered, 3, 3, "frame")
	if !strings.Contains(past.Error(), "every recorded frame came, and the stream did not end") {
		t.Fatalf("%v", past)
	}
	for _, recorded := range []int{0, 3} {
		if none := streamCut(answered, 0, recorded, "frame"); !errors.Is(none, ErrAnswerStalled) || errors.Is(none, ErrStreamHeldOpen) {
			t.Fatalf("%v: a stream none of whose frames came (of %d recorded) must fail closed, an ErrAnswerStalled", none, recorded)
		}
	}
	if other := errors.New("x"); streamCut(other, 0, 1, "frame") != other {
		t.Fatal("streamCut changed an error that is no stall")
	}
}

// stallApp is an app that does what the hangs this rule ends did, each held
// until the test ends: a stream past or short of its recording with no
// keepalive at all, an answer that never comes, and a request body it never
// reads. And a slow answer, within the rule's grace, and one that trickles,
// each piece within it.
func stallApp(t *testing.T) string {
	t.Helper()
	hold := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/sse-past":
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: a\n\ndata: b\n\n"))
			w.(http.Flusher).Flush()
			<-hold
		case "/ndjson-short":
			w.Header().Set("Content-Type", "application/x-ndjson")
			_, _ = w.Write([]byte("{\"id\":1}\n"))
			w.(http.Flusher).Flush()
			<-hold
		case "/never", "/upload-unread":
			<-hold
		case "/sse-none":
			w.Header().Set("Content-Type", "text/event-stream")
			w.(http.Flusher).Flush()
			<-hold
		case "/sse-keepalive-none":
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte(": ping\n\n"))
			w.(http.Flusher).Flush()
			<-hold
		case "/ndjson-keepalive-none":
			w.Header().Set("Content-Type", "application/x-ndjson")
			_, _ = w.Write([]byte("\n\n"))
			w.(http.Flusher).Flush()
			<-hold
		case "/plain-short":
			w.Header().Set("Content-Type", "text/plain")
			_, _ = w.Write([]byte("alpha\n"))
			w.(http.Flusher).Flush()
			<-hold
		case "/multipart-short":
			w.Header().Set("Content-Type", "multipart/x-mixed-replace; boundary=frame")
			_, _ = w.Write([]byte("--frame\r\nContent-Type: text/plain\r\n\r\none\r\n--frame\r\n"))
			w.(http.Flusher).Flush()
			<-hold
		case "/binary-short", "/binary-differs", "/binary-none":
			w.Header().Set("Content-Type", "application/octet-stream")
			switch r.URL.Path {
			case "/binary-short":
				_, _ = w.Write([]byte("abc"))
			case "/binary-differs":
				_, _ = w.Write([]byte("abX"))
			}
			w.(http.Flusher).Flush()
			<-hold
		case "/sse-gzip", "/sse-br":
			w.Header().Set("Content-Type", "text/event-stream")
			var enc interface {
				Write([]byte) (int, error)
				Flush() error
			}
			if r.URL.Path == "/sse-gzip" {
				w.Header().Set("Content-Encoding", "gzip")
				enc = gzip.NewWriter(w)
			} else {
				w.Header().Set("Content-Encoding", "br")
				enc = brotli.NewWriter(w)
			}
			_, _ = enc.Write([]byte("data: a\n\ndata: b\n\n"))
			_ = enc.Flush()
			w.(http.Flusher).Flush()
			<-hold
		case "/partial":
			w.Header().Set("Content-Length", "100")
			_, _ = w.Write([]byte("0123456789"))
			w.(http.Flusher).Flush()
			<-hold
		case "/partial-gzip":
			w.Header().Set("Content-Encoding", "gzip")
			gz := gzip.NewWriter(w)
			_, _ = gz.Write([]byte("0123456789"))
			_ = gz.Flush()
			w.(http.Flusher).Flush()
			<-hold
		case "/ndjson-done":
			w.Header().Set("Content-Type", "application/x-ndjson")
			_, _ = w.Write([]byte("{\"id\":1}\n{\"id\":2}\n"))
		case "/upload-slow":
			// Reads the body a piece at a time, each within the grace,
			// the whole far longer than it.
			buf := make([]byte, 4<<20)
			for {
				if _, err := io.ReadFull(r.Body, buf); err != nil {
					break
				}
				time.Sleep(100 * time.Millisecond)
			}
			_, _ = w.Write([]byte(`{"ok":true}`))
		case "/slow":
			time.Sleep(100 * time.Millisecond)
			_, _ = w.Write([]byte(`{"ok":true}`))
		case "/trickle":
			w.Header().Set("Content-Type", "application/x-ndjson")
			for i := 0; i < 6; i++ {
				_, _ = w.Write([]byte("{\"id\":1}\n"))
				w.(http.Flusher).Flush()
				time.Sleep(100 * time.Millisecond)
			}
		}
	}))
	t.Cleanup(func() { close(hold); srv.Close() })
	return srv.URL
}

// stallConfig is a replay's with no bound of the user's, and the rule's grace
// short for the test, on keploy's own runnable clock.
func stallConfig() SimulationConfig {
	return SimulationConfig{APITimeout: 1, Unbounded: true, StallGrace: 300 * time.Millisecond}
}

// Under the proven switch with no apiTimeout of the user's, every wait for
// the app's answer that had no end now ends on the stall rule, the test not
// judged, naming what it waited for (reproduced first, each hanging):
// R1 an SSE stream past its recording and R2 an NDJSON one short of it, with
// no keepalive at all, judged on what came; R5 an answer that never comes and
// R6 a request body the app never reads, with nothing to compare.
func TestEveryAnswerThatNeverEndedEndsOnTheStallRule(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	url := stallApp(t)
	stream := func(path, ctype string, chunks []models.HTTPStreamChunk) *models.TestCase {
		return &models.TestCase{Name: path, Kind: models.HTTP,
			HTTPReq:  models.HTTPReq{Method: "GET", URL: url + path},
			HTTPResp: models.HTTPResp{StatusCode: 200, Header: map[string]string{"Content-Type": ctype}, StreamBody: chunks}}
	}
	readStream := func(t *testing.T, tc *models.TestCase) (bool, error) {
		resp, err := SimulateHTTPStreaming(context.Background(), tc, "s", zap.NewNop(), stallConfig())
		if err != nil {
			t.Fatalf("SimulateHTTPStreaming: %v", err)
		}
		defer resp.Reader.Close()
		ok, _, info, err := CompareHTTPStream(tc.HTTPResp, resp.Reader, resp.StreamConfig, map[string]struct{}{}, zap.NewNop())
		if info != nil {
			t.Fatalf("a mismatch in what came: %+v", info)
		}
		return ok, err
	}

	t.Run("R1 SSE past its recording", func(t *testing.T) {
		ok, err := readStream(t, stream("/sse-past", "text/event-stream", frames("data", "a", "b")))
		if !ok || !errors.Is(err, ErrStreamHeldOpen) || !strings.Contains(fmtErr(err), "every recorded frame came, and the stream did not end") || !strings.Contains(fmtErr(err), "GET /sse-past") {
			t.Fatalf("%v, %v; want judged on what came, not judged: past the recording", ok, err)
		}
	})
	t.Run("R2 NDJSON short of its recording", func(t *testing.T) {
		ok, err := readStream(t, stream("/ndjson-short", "application/x-ndjson", frames("raw", `{"id":1}`, `{"id":2}`)))
		if !ok || !errors.Is(err, ErrStreamHeldOpen) || !strings.Contains(fmtErr(err), "1 of the 2 recorded lines came") {
			t.Fatalf("%v, %v; want judged on what came, not judged: short of the recording", ok, err)
		}
	})
	// A stream none of whose frames came has nothing to compare: it fails
	// closed (ErrAnswerStalled), whether the stall rule ended it or the
	// app's keepalives did, never a pass on the recording.
	t.Run("a stream that sends nothing", func(t *testing.T) {
		for _, c := range []struct {
			path, ctype string
			chunks      []models.HTTPStreamChunk
			says        string
		}{
			{"/sse-none", "text/event-stream", frames("data", "a"), "the app sent its answer's headers and nothing of its body; waited 300ms of keploy's runnable time for GET /sse-none"},
			{"/sse-keepalive-none", "text/event-stream", nil, "the app sent its answer's headers and nothing of its body but keepalives, for GET /sse-keepalive-none"},
			{"/ndjson-keepalive-none", "application/x-ndjson", frames("raw", `{"id":1}`), "the app sent its answer's headers and nothing of its body but keepalives, for GET /ndjson-keepalive-none"},
			{"/binary-none", "application/octet-stream", frames("raw", "abcdef"), "the app sent its answer's headers and nothing of its body; waited 300ms of keploy's runnable time for GET /binary-none"},
		} {
			ok, err := readStream(t, stream(c.path, c.ctype, c.chunks))
			if ok || !errors.Is(err, ErrAnswerStalled) || errors.Is(err, ErrStreamHeldOpen) || fmtErr(err) != c.says {
				t.Fatalf("%s: %v, %v; want it failing closed, an ErrAnswerStalled saying %q", c.path, ok, err, c.says)
			}
		}
	})
	t.Run("plain text and multipart short of their recordings", func(t *testing.T) {
		ok, err := readStream(t, stream("/plain-short", "text/plain", frames("raw", "alpha", "beta")))
		if !ok || !errors.Is(err, ErrStreamHeldOpen) || !strings.Contains(fmtErr(err), "1 of the 2 recorded lines came") {
			t.Fatalf("plain text: %v, %v; want judged on what came, not judged", ok, err)
		}
		tc := stream("/multipart-short", "multipart/x-mixed-replace; boundary=frame", nil)
		tc.HTTPResp.Body = "--frame\r\nContent-Type: text/plain\r\n\r\none\r\n--frame\r\nContent-Type: text/plain\r\n\r\ntwo\r\n--frame--\r\n"
		ok, err = readStream(t, tc)
		if !ok || !errors.Is(err, ErrStreamHeldOpen) || !strings.Contains(fmtErr(err), "1 of the 2 recorded parts came") {
			t.Fatalf("multipart: %v, %v; want judged on what came, not judged", ok, err)
		}
	})
	t.Run("a binary stream", func(t *testing.T) {
		ok, err := readStream(t, stream("/binary-short", "application/octet-stream", frames("raw", "abcdef")))
		if !ok || !errors.Is(err, ErrStreamHeldOpen) || !strings.Contains(fmtErr(err), "3 of the 6 recorded bytes came") {
			t.Fatalf("%v, %v; want judged on what came, not judged", ok, err)
		}
		// A byte that came and differs fails the test, by its content:
		// the cut, not the app, set how much came.
		tc := stream("/binary-differs", "application/octet-stream", frames("raw", "abcdef"))
		resp, err := SimulateHTTPStreaming(context.Background(), tc, "s", zap.NewNop(), stallConfig())
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Reader.Close()
		ok, _, info, err := CompareHTTPStream(tc.HTTPResp, resp.Reader, resp.StreamConfig, map[string]struct{}{}, zap.NewNop())
		if ok || err != nil || info == nil || info.Reason != "content mismatch starting near byte 0 of 3 bytes" {
			t.Fatalf("%v, %+v, %v; want the test failed on the byte that differs", ok, info, err)
		}
	})
	t.Run("a compressed stream past its recording", func(t *testing.T) {
		for _, path := range []string{"/sse-gzip", "/sse-br"} {
			tc := stream(path, "text/event-stream", frames("data", "a", "b"))
			tc.HTTPReq.Header = map[string]string{"Accept-Encoding": "gzip, br"}
			ok, err := readStream(t, tc)
			if !ok || !errors.Is(err, ErrStreamHeldOpen) || !strings.Contains(fmtErr(err), "every recorded frame came, and the stream did not end") {
				t.Fatalf("%s: %v, %v; want judged on what came, not judged", path, ok, err)
			}
		}
	})
	// A stream the app ended is judged, however late keploy reads it: the
	// time keploy does not wait on the app is not the app's stall.
	t.Run("a finished stream read late", func(t *testing.T) {
		tc := stream("/ndjson-done", "application/x-ndjson", frames("raw", `{"id":1}`, `{"id":2}`))
		resp, err := SimulateHTTPStreaming(context.Background(), tc, "s", zap.NewNop(), stallConfig())
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Reader.Close()
		time.Sleep(4 * stallConfig().StallGrace)
		ok, _, info, err := CompareHTTPStream(tc.HTTPResp, resp.Reader, resp.StreamConfig, map[string]struct{}{}, zap.NewNop())
		if !ok || err != nil || info != nil {
			t.Fatalf("%v, %+v, %v; want the finished stream judged, and passed", ok, info, err)
		}
	})
	// A stream read to its end ends its watch, closed or not.
	t.Run("a stream read to its end, never closed", func(t *testing.T) {
		tc := stream("/ndjson-done", "application/x-ndjson", frames("raw", `{"id":1}`, `{"id":2}`))
		resp, err := SimulateHTTPStreaming(context.Background(), tc, "s", zap.NewNop(), stallConfig())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.ReadAll(resp.Reader); err != nil {
			t.Fatal(err)
		}
		select {
		case <-resp.Reader.(*answerReader).w.done:
		default:
			t.Fatal("the watch of a stream read to its end goes on")
		}
		_ = resp.Reader.Close()
	})
	t.Run("an answer that stops part-way", func(t *testing.T) {
		tc := &models.TestCase{Name: "partial", Kind: models.HTTP, HTTPReq: models.HTTPReq{Method: "GET", URL: url + "/partial"}}
		resp, err := SimulateHTTP(context.Background(), tc, "s", zap.NewNop(), stallConfig())
		if want := "the app's answer stopped part-way, after 10 bytes of its body; waited 300ms of keploy's runnable time for GET /partial"; !errors.Is(err, ErrAnswerStalled) || fmtErr(err) != want || !AnswerBegan(err) {
			t.Fatalf("%v; want %q", err, want)
		}
		// Its start goes with the error, for the replay to judge it, and
		// the part of its body that came.
		if resp == nil || resp.StatusCode != 200 || resp.Header["Content-Length"] != "100" || resp.Body != "0123456789" {
			t.Fatalf("returned %+v; want the status, headers and part of the body that came", resp)
		}
	})
	t.Run("an encoded answer that stops part-way", func(t *testing.T) {
		tc := &models.TestCase{Name: "partial-gzip", Kind: models.HTTP, HTTPReq: models.HTTPReq{Method: "GET", URL: url + "/partial-gzip", Header: map[string]string{"Accept-Encoding": "gzip"}}}
		resp, err := SimulateHTTP(context.Background(), tc, "s", zap.NewNop(), stallConfig())
		if !errors.Is(err, ErrAnswerStalled) || !AnswerBegan(err) {
			t.Fatalf("%v; want an answer that began and stalled", err)
		}
		// The part that came is kept decoded, as a whole body would be.
		if resp == nil || resp.Header["Content-Encoding"] != "gzip" || resp.Body != "0123456789" {
			t.Fatalf("returned %+v; want the part of the body that came, decoded", resp)
		}
	})
	t.Run("R5 an answer that never comes", func(t *testing.T) {
		tc := &models.TestCase{Name: "never", Kind: models.HTTP, HTTPReq: models.HTTPReq{Method: "GET", URL: url + "/never"}}
		_, err := SimulateHTTP(context.Background(), tc, "s", zap.NewNop(), stallConfig())
		if !errors.Is(err, ErrAnswerStalled) || !strings.Contains(fmtErr(err), "the app never answered; waited 300ms of keploy's runnable time for GET /never") {
			t.Fatalf("%v; want the app never answered", err)
		}
		if !IsAppNoAnswer(err) {
			t.Fatal("a stalled answer is no answer of the app's")
		}
	})
	t.Run("R6 a request body the app never reads", func(t *testing.T) {
		tc := &models.TestCase{Name: "upload", Kind: models.HTTP, HTTPReq: models.HTTPReq{Method: "POST", URL: url + "/upload-unread", Body: strings.Repeat("x", 16<<20)}}
		_, err := SimulateHTTP(context.Background(), tc, "s", zap.NewNop(), stallConfig())
		if !errors.Is(err, ErrAnswerStalled) || !strings.Contains(fmtErr(err), "stopped taking the test's request") || !strings.Contains(fmtErr(err), "were never taken") {
			t.Fatalf("%v; want the request the app stopped taking", err)
		}
	})

	// The rule measures missing progress only: an app slow within the grace,
	// or one whose stream trickles in pieces each within it, is judged.
	t.Run("an answer slow within the grace", func(t *testing.T) {
		tc := &models.TestCase{Name: "slow", Kind: models.HTTP, HTTPReq: models.HTTPReq{Method: "GET", URL: url + "/slow"}}
		if _, err := SimulateHTTP(context.Background(), tc, "s", zap.NewNop(), stallConfig()); err != nil {
			t.Fatalf("SimulateHTTP: %v", err)
		}
	})
	t.Run("an upload the app reads slowly", func(t *testing.T) {
		tc := &models.TestCase{Name: "upload-slow", Kind: models.HTTP, HTTPReq: models.HTTPReq{Method: "POST", URL: url + "/upload-slow", Body: strings.Repeat("x", 32<<20)}}
		if _, err := SimulateHTTP(context.Background(), tc, "s", zap.NewNop(), stallConfig()); err != nil {
			t.Fatalf("SimulateHTTP: %v; want judged: the app took each piece of the request within the grace", err)
		}
	})
	t.Run("a stream that trickles", func(t *testing.T) {
		lines := make([]string, 6)
		for i := range lines {
			lines[i] = `{"id":1}`
		}
		ok, err := readStream(t, stream("/trickle", "application/x-ndjson", frames("raw", lines...)))
		if !ok || err != nil {
			t.Fatalf("%v, %v; want judged: each piece came within the grace", ok, err)
		}
	})
}

// The gRPC waits that had no end: R3 a stream past its recording with no
// keepalive (a default grpc-go server pings after two hours) and R4 a large
// client-streaming request the app never reads, which never sends its
// END_STREAM, so no PING is ever counted. R3 is judged on what came; R4 has
// no answer, and says how much of the request the app never took.
func TestTheGRPCWaitsThatNeverEndedEndOnTheStallRule(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	hold := make(chan struct{})
	t.Cleanup(func() { close(hold) })

	t.Run("R3 a stream past its recording", func(t *testing.T) {
		addr := startStreamingServer(t, &streamingServer{respond: [][]byte{pbMsg("one")}, holdOpen: hold})
		tc := recordedAs(grpcTestCase(addr, []models.GrpcLengthPrefixedMessage{{}}), pbMsg("one"))
		resp, err := SimulateGRPC(context.Background(), tc, "s", zap.NewNop(), stallConfig())
		if !errors.Is(err, ErrStreamHeldOpen) || !strings.Contains(fmtErr(err), "every recorded message came, and the stream did not end") || !strings.Contains(fmtErr(err), "/pkg.Svc/Method") {
			t.Fatalf("%v; want judged on what came, not judged", err)
		}
		if got := len(resp.AllMessages()); got != 1 {
			t.Fatalf("returned %d messages, want the one that came", got)
		}
	})
	t.Run("R2' a stream short of its recording", func(t *testing.T) {
		addr := startStreamingServer(t, &streamingServer{respond: [][]byte{pbMsg("one")}, holdOpen: hold})
		tc := recordedAs(grpcTestCase(addr, []models.GrpcLengthPrefixedMessage{{}}), pbMsg("one"), pbMsg("two"))
		resp, err := SimulateGRPC(context.Background(), tc, "s", zap.NewNop(), stallConfig())
		if !errors.Is(err, ErrStreamHeldOpen) || !strings.Contains(fmtErr(err), "1 of the 2 recorded messages came") {
			t.Fatalf("%v; want judged on what came, not judged", err)
		}
		if got := len(resp.AllMessages()); got != 2 {
			t.Fatalf("returned %d messages, want what came and the recording's rest", got)
		}
		if got := resp.Trailers.OrdinaryHeaders["grpc-status"]; got != "0" {
			t.Fatalf("grpc-status %q: keploy's own cut taken for the app's status", got)
		}
	})
	t.Run("R4 a large request the app never reads", func(t *testing.T) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		g := grpc.NewServer(grpc.ForceServerCodec(rawSrvCodec{}), grpc.UnknownServiceHandler(func(_ interface{}, _ grpc.ServerStream) error {
			<-hold
			return nil
		}))
		go func() { _ = g.Serve(ln) }()
		t.Cleanup(g.Stop)
		var reqs []models.GrpcLengthPrefixedMessage
		big := strings.Repeat("y", 32<<10)
		for i := 0; i < 64; i++ {
			reqs = append(reqs, models.GrpcLengthPrefixedMessage{DecodedData: "1: {\"" + big + "\"}"})
		}
		_, err = SimulateGRPC(context.Background(), grpcTestCase(ln.Addr().String(), reqs), "s", zap.NewNop(), stallConfig())
		if !errors.Is(err, ErrAnswerStalled) || !strings.Contains(fmtErr(err), "stopped taking the test's request") || !strings.Contains(fmtErr(err), "were never taken") {
			t.Fatalf("%v; want the request the app stopped taking", err)
		}
	})
}

// The app's keepalives are no progress: a gRPC app that sends them every
// second, and never reads a large request (R4) or never answers one, still
// ends on the stall rule, or on its keepalives (row W1). One that sent
// nothing of its answer, its headers at most, fails closed
// (ErrAnswerStalled), never a pass on the recording, with no bound of the
// user's or with one.
func TestTheGRPCAppsKeepalivesAreNoProgress(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	hold := make(chan struct{})
	t.Cleanup(func() { close(hold) })
	drain := func(stream grpc.ServerStream) {
		var m rawMessage
		for stream.RecvMsg(&m) == nil {
		}
	}

	t.Run("R4 against a server whose keepalives come every second", func(t *testing.T) {
		counted := startKeepaliveServer(t, 1000, func(_ interface{}, _ grpc.ServerStream) error {
			<-hold
			return nil
		})
		addr := counted.Addr().String()
		var reqs []models.GrpcLengthPrefixedMessage
		big := strings.Repeat("y", 32<<10)
		for i := 0; i < 64; i++ {
			reqs = append(reqs, models.GrpcLengthPrefixedMessage{DecodedData: "1: {\"" + big + "\"}"})
		}
		cfg := stallConfig()
		cfg.StallGrace = 3 * time.Second
		_, err := simulateGRPCBounded(t, grpcTestCase(addr, reqs), cfg)
		if !errors.Is(err, ErrAnswerStalled) || !strings.Contains(fmtErr(err), "stopped taking the test's request") || !strings.Contains(fmtErr(err), "were never taken") {
			t.Fatalf("%v; want the request the app stopped taking", err)
		}
		if n := counted.pings.Load(); n < 2 {
			t.Fatalf("the app sent %d keepalive(s) during the wait; the test needs it to send them", n)
		}
	})

	t.Run("a large request the app reads slowly", func(t *testing.T) {
		slow := startGRPCServer(t, 1000, time.Second, func(_ interface{}, stream grpc.ServerStream) error {
			var m rawMessage
			for stream.RecvMsg(&m) == nil {
				time.Sleep(25 * time.Millisecond)
			}
			return stream.SendMsg(&rawMessage{data: pbMsg("one")})
		}).Addr().String()
		var reqs []models.GrpcLengthPrefixedMessage
		big := strings.Repeat("y", 32<<10)
		for i := 0; i < 64; i++ {
			reqs = append(reqs, models.GrpcLengthPrefixedMessage{DecodedData: "1: {\"" + big + "\"}"})
		}
		tc := recordedAs(grpcTestCase(slow, reqs), pbMsg("one"))
		if _, err := SimulateGRPC(context.Background(), tc, "s", zap.NewNop(), stallConfig()); err != nil {
			t.Fatalf("SimulateGRPC: %v; want judged: the app took each message within the grace, its keepalives aside", err)
		}
	})

	t.Run("nothing of the answer, the stall rule's grace", func(t *testing.T) {
		addr := startStreamingServer(t, &streamingServer{holdOpen: hold})
		addrHeaders := startGRPCServer(t, 1000, 0, func(_ interface{}, stream grpc.ServerStream) error {
			drain(stream)
			_ = stream.SendHeader(nil)
			<-hold
			return nil
		}).Addr().String()
		for _, c := range []struct {
			addr, says string
			began      bool
		}{
			{addr, "the app never answered; waited 300ms of keploy's runnable time for /pkg.Svc/Method", false},
			{addrHeaders, "the app sent its answer's headers and nothing of its body; waited 300ms of keploy's runnable time for /pkg.Svc/Method", true},
		} {
			tc := recordedAs(grpcTestCase(c.addr, []models.GrpcLengthPrefixedMessage{{}}), pbMsg("one"))
			resp, err := simulateGRPCBounded(t, tc, stallConfig())
			if !errors.Is(err, ErrAnswerStalled) || errors.Is(err, ErrStreamHeldOpen) || fmtErr(err) != c.says || AnswerBegan(err) != c.began {
				t.Fatalf("%v, %v; want it failing closed: %q", resp, err, c.says)
			}
			requireHeadersOnly(t, resp, c.began)
		}
	})

	t.Run("nothing of the answer, the app's keepalives", func(t *testing.T) {
		never := startKeepaliveServer(t, 1000, func(_ interface{}, stream grpc.ServerStream) error {
			drain(stream)
			<-hold
			return nil
		}).Addr().String()
		headers := startKeepaliveServer(t, 1000, func(_ interface{}, stream grpc.ServerStream) error {
			drain(stream)
			_ = stream.SendHeader(nil)
			<-hold
			return nil
		}).Addr().String()
		for _, c := range []struct {
			addr, says string
			cfg        SimulationConfig
			began      bool
		}{
			{never, "the app never answered, and sent only keepalives, for /pkg.Svc/Method", SimulationConfig{APITimeout: 60}, false},
			{headers, "the app sent its answer's headers and nothing of its body, then only keepalives, for /pkg.Svc/Method", SimulationConfig{APITimeout: 60}, true},
			{never, "the app never answered, and sent only keepalives, for /pkg.Svc/Method", SimulationConfig{Unbounded: true}, false},
		} {
			tc := recordedAs(grpcTestCase(c.addr, []models.GrpcLengthPrefixedMessage{{}}), pbMsg("one"))
			resp, err := simulateGRPCBounded(t, tc, c.cfg)
			if !errors.Is(err, ErrAnswerStalled) || errors.Is(err, ErrStreamHeldOpen) || fmtErr(err) != c.says || AnswerBegan(err) != c.began {
				t.Fatalf("%+v: %v, %v; want it failing closed: %q", c.cfg, resp, err, c.says)
			}
			requireHeadersOnly(t, resp, c.began)
		}
	})
}

// simulateGRPCBounded is SimulateGRPC, failing the test, not hanging it, when
// the call does not end within a minute: a stall rule that never fires.
func simulateGRPCBounded(t *testing.T, tc *models.TestCase, cfg SimulationConfig) (*models.GrpcResp, error) {
	t.Helper()
	type result struct {
		resp *models.GrpcResp
		err  error
	}
	done := make(chan result, 1)
	go func() {
		resp, err := SimulateGRPC(context.Background(), tc, "s", zap.NewNop(), cfg)
		done <- result{resp, err}
	}()
	select {
	case r := <-done:
		return r.resp, r.err
	case <-time.After(time.Minute):
		t.Fatal("HANG: the call did not end on its stall rule")
		return nil, nil
	}
}

// requireHeadersOnly checks what a stalled gRPC call returned: with its answer
// begun, the headers that came and no message, for the replay to judge them
// (AnswerBegan); with none, nothing.
func requireHeadersOnly(t *testing.T, resp *models.GrpcResp, began bool) {
	t.Helper()
	switch {
	case !began && resp != nil:
		t.Fatalf("an answer that never began returned %+v", resp)
	case began && (resp == nil || resp.Headers.PseudoHeaders[":status"] != "200" || len(resp.AllMessages()) > 1 || len(resp.AllMessages()) == 1 && resp.AllMessages()[0].DecodedData != ""):
		t.Fatalf("an answer that began returned %+v; want its headers and no message", resp)
	}
}

func fmtErr(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// A start is never made up: with no metadata in hand there is none to
// judge.
func TestHeadersOnlyMakesUpNoStart(t *testing.T) {
	if got := headersOnly(nil); got != nil {
		t.Fatalf("headersOnly(nil) = %+v; want none", got)
	}
}
