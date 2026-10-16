package pkg

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

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

// The rule measures the app's missing progress in keploy's runnable time: a
// byte the app sends back, or a byte of the request it takes, starts it again;
// the grace passing with neither cuts the exchange. Wall time passing while
// keploy could not run (a starved host) cuts nothing.
func TestTheAnswerStallRuleCutsOnlyAGraceOfRunnableTimeWithNoProgress(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	clk := &stall.ManualClock{}
	w := newAnswerWatch(SimulationConfig{Unbounded: true, StallGrace: 10 * time.Second, StallClock: clk})
	defer w.stop()
	var cuts atomic.Int32
	w.onCut(func() { cuts.Add(1) })
	keploy, app := net.Pipe()
	defer app.Close()
	c := w.wrap(keploy)

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

	// The app answers a byte: the stall starts again.
	go func() { _, _ = app.Write([]byte("x")) }()
	if _, err := c.Read(make([]byte, 1)); err != nil {
		t.Fatal(err)
	}
	clk.Advance(9 * time.Second)
	waitArmed(t, clk, w)
	if w.didStall() {
		t.Fatal("progress did not start the stall again")
	}

	// The app takes a byte of the request: progress too.
	go func() { _, _ = app.Read(make([]byte, 1)) }()
	if _, err := c.Write([]byte("y")); err != nil {
		t.Fatal(err)
	}
	clk.Advance(9 * time.Second)
	waitArmed(t, clk, w)
	if w.didStall() {
		t.Fatal("a request byte taken did not start the stall again")
	}

	clk.Advance(time.Second)
	for !w.didStall() {
		time.Sleep(time.Millisecond)
	}
	if n := cuts.Load(); n != 1 {
		t.Fatalf("cut %d times, want once", n)
	}
	if _, err := c.Read(make([]byte, 1)); err == nil {
		t.Fatal("the cut left the connection open")
	}
}

// What the rule waited for: a request the app stopped taking (a write keploy
// still waited on at the cut, and how much of the request had gone), or an
// answer that never came. Neither began an answer: ErrAnswerStalled. A stream
// cut after its answer began is an ErrStreamHeldOpen, saying what of its
// recording came.
func TestTheAnswerStallSaysWhatItWaitedFor(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	clk := &stall.ManualClock{}
	w := newAnswerWatch(SimulationConfig{Unbounded: true, StallGrace: 10 * time.Second, StallClock: clk})
	defer w.stop()
	w.describe("POST /upload", 1000)
	keploy, app := net.Pipe()
	defer app.Close()
	c := w.wrap(keploy)
	w.sentBody(300)
	written := make(chan error, 1)
	go func() {
		_, err := c.Write(make([]byte, 64)) // the app never reads it
		written <- err
	}()
	for w.writing.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	waitArmed(t, clk, w)
	clk.Advance(10 * time.Second)
	if err := <-written; err == nil {
		t.Fatal("the cut did not end the write")
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

	never := &AnswerStallError{Target: "GET /x", Waited: time.Minute}
	if got, want := never.Error(), "the app never answered; waited 1m0s of keploy's runnable time for GET /x"; got != want {
		t.Fatalf("%q, want %q", got, want)
	}
	stream := streamCut(&AnswerStallError{Target: "GET /events", Waited: time.Minute, Answered: true}, 1, 3, "frame")
	if !errors.Is(stream, ErrStreamHeldOpen) || errors.Is(stream, ErrAnswerStalled) || !strings.Contains(stream.Error(), "1 of the 3 recorded frames came") {
		t.Fatalf("%v: want an ErrStreamHeldOpen saying what came", stream)
	}
	past := streamCut(&AnswerStallError{Target: "GET /events", Waited: time.Minute, Answered: true}, 3, 3, "frame")
	if !strings.Contains(past.Error(), "every recorded frame came, and the stream did not end") {
		t.Fatalf("%v", past)
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

func fmtErr(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
