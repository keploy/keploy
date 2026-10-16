package replay

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// heldOpenLines is what the stand-in app streams, as recorded.
var heldOpenLines = []string{`{"id":1}`, `{"id":2}`}

// heldOpenApps is an app with a streamed HTTP answer and a gRPC side that,
// in mode holdPast, keep their streams open past their recorded answers, in
// mode stallShort, short of them, and in mode sendNothing with nothing of
// them, sending keepalives: NDJSON blank lines, and gRPC PINGs (grpc-go
// sends one after a second without activity). Each
// sends at most a bounded number of keepalives, and exhausted counts the
// streams that sent them all and were still read: what a replay that waits
// for the end does.
type heldOpenApps struct {
	http      *tmplApp
	grpcAddr  string
	grpcPort  uint16
	mode      atomic.Int32
	exhausted atomic.Int32
}

// The apps' modes: ending as recorded, holding the stream open past the
// recorded answer, stalling short of it, and with none of it.
const (
	asRecorded int32 = iota
	holdPast
	stallShort
	sendNothing
	// wrongStart: sendNothing, with the HTTP side's status 500 against
	// its recorded 200.
	wrongStart
)

func newHeldOpenApps(t *testing.T) *heldOpenApps {
	t.Helper()
	apps := &heldOpenApps{}
	apps.http = newTmplApp(t, func(a *tmplApp, mux *http.ServeMux) {
		mux.HandleFunc("/stream", func(w http.ResponseWriter, r *http.Request) {
			a.record(r)
			w.Header().Set("Content-Type", "application/x-ndjson")
			mode := apps.mode.Load()
			if mode == wrongStart {
				w.WriteHeader(http.StatusInternalServerError)
				mode = sendNothing
			}
			for i, line := range heldOpenLines {
				if mode == stallShort && i > 0 || mode == sendNothing {
					break
				}
				_, _ = w.Write([]byte(line + "\n"))
				w.(http.Flusher).Flush()
			}
			if mode == asRecorded {
				return
			}
			// The app's own pace between keepalives, as a heartbeat has.
			for i := 0; i < 200; i++ {
				if _, err := w.Write([]byte("\n")); err != nil {
					return
				}
				w.(http.Flusher).Flush()
				select {
				case <-r.Context().Done():
					return
				case <-time.After(10 * time.Millisecond):
				}
			}
			apps.exhausted.Add(1)
		})
	})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	apps.grpcAddr = ln.Addr().String()
	_, port, _ := net.SplitHostPort(apps.grpcAddr)
	p, _ := strconv.Atoi(port)
	apps.grpcPort = uint16(p)
	server := grpc.NewServer(grpc.ForceServerCodec(bpRawCodec{}),
		grpc.KeepaliveParams(keepalive.ServerParameters{Time: time.Second, Timeout: time.Minute}),
		grpc.UnknownServiceHandler(func(_ interface{}, stream grpc.ServerStream) error {
			var in []byte
			for stream.RecvMsg(&in) == nil {
			}
			mode := apps.mode.Load()
			out := []byte{0x0a, 0x02, 'o', 'k'}
			// Two messages as recorded, one short of the recording, or
			// none.
			sends := 2
			switch mode {
			case stallShort:
				sends = 1
			case sendNothing, wrongStart:
				sends = 0
			}
			for i := 0; i < sends; i++ {
				if err := stream.SendMsg(&out); err != nil {
					return err
				}
			}
			if mode != asRecorded {
				// grpc-go's keepalives go on while it waits.
				select {
				case <-stream.Context().Done():
				case <-time.After(30 * time.Second):
					apps.exhausted.Add(1)
				}
				return nil
			}
			// The answer ends with a status and a trailer of the app's: a
			// stream it holds open never sends them.
			stream.SetTrailer(metadata.Pairs("x-done", "yes"))
			return status.Error(codes.Aborted, "done")
		}))
	go func() { _ = server.Serve(ln) }()
	t.Cleanup(server.Stop)
	return apps
}

// cases are the two tests, recorded against the apps as they end their
// streams.
func (a *heldOpenApps) cases(t *testing.T) []*models.TestCase {
	t.Helper()
	stream := tmplCase(1, a.http.addr, "GET", "/stream", nil, "", models.HTTPResp{
		StatusCode: 200,
		Header:     map[string]string{"Content-Type": "application/x-ndjson", "Date": "Mon, 05 Oct 2026 10:00:00 GMT"},
	})
	for _, line := range heldOpenLines {
		stream.HTTPResp.StreamBody = append(stream.HTTPResp.StreamBody, models.HTTPStreamChunk{
			Data: []models.HTTPStreamDataField{{Key: "raw", Value: line}},
		})
	}
	stream.Noise = map[string][]string{"header.Date": {}}

	req := models.GrpcReq{Headers: models.GrpcHeaders{
		PseudoHeaders: map[string]string{
			":authority": a.grpcAddr, ":path": "/orders.Orders/Get", ":method": "POST", ":scheme": "http",
		},
		OrdinaryHeaders: map[string]string{"content-type": "application/grpc"},
	}}
	req.SetMessages([]models.GrpcLengthPrefixedMessage{{}})
	call := &models.TestCase{Version: models.GetVersion(), Kind: models.GRPC_EXPORT, Name: "test-2", AppPort: a.grpcPort, GrpcReq: req}
	resp, err := pkg.SimulateGRPC(context.Background(), call, "test-set-0", zap.NewNop(), pkg.SimulationConfig{APITimeout: 5})
	if err != nil {
		t.Fatalf("recording the gRPC test: %v", err)
	}
	call.GrpcResp = *resp
	return []*models.TestCase{stream, call}
}

// Under the proven switch, with no clock on the app's answer (row W1), a test
// whose app keeps its stream open on keepalives, past the recording or short
// of it, ends on them instead of waiting for an end that never comes: it has
// no verdict (FailureInfo.StreamHeldOpen), and neither has its set, never a
// pass. The same app ending its streams passes, its flow-control pings
// notwithstanding.
func TestUnderTheSwitchAStreamHeldOpenPastItsRecordingIsNotJudged(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	apps := newHeldOpenApps(t)
	cases := apps.cases(t)

	h := newTmplHarness(t, &tmplConf{}, cases, nil)
	if status := h.run(t); status != models.TestSetStatusPassed {
		t.Fatalf("control: the apps ending their streams gave a set %s, want PASSED (%s, %s)", status, why(h.result(t, "test-1")), why(h.result(t, "test-2")))
	}

	for _, mode := range []struct {
		name string
		mode int32
		says string
	}{
		{"past its recording", holdPast, "past its recording"},
		{"short of its recording", stallShort, "sent no more of its recording"},
	} {
		t.Run(mode.name, func(t *testing.T) {
			apps.mode.Store(mode.mode)
			t.Cleanup(func() { apps.mode.Store(asRecorded) })
			h := newTmplHarness(t, &tmplConf{}, cases, nil)
			status := h.run(t)
			if n := apps.exhausted.Load(); n != 0 {
				t.Fatalf("%d stream(s) sent every keepalive they had and were still read: the replay waits for an end that never comes", n)
			}
			for _, name := range []string{"test-1", "test-2"} {
				r := h.result(t, name)
				if !strings.Contains(r.FailureInfo.StreamHeldOpen, mode.says) || len(r.VerdictUnknown()) == 0 {
					t.Fatalf("%s: %s, %+v; want no verdict: the app kept its stream open %s", name, r.Status, r.FailureInfo, mode.name)
				}
			}
			if status != models.TestSetStatusInternalErr {
				t.Fatalf("the set is %s, want INTERNAL_ERR: not judged", status)
			}
			reason := h.report.report.FailureReason
			for _, want := range []string{"kept its stream open on keepalives", "test-1", "test-2"} {
				if !strings.Contains(reason, want) {
					t.Fatalf("failure reason %q does not say %q", reason, want)
				}
			}
		})
	}

	// Keepalives and nothing of the answer: nothing to compare, so each
	// test fails closed, FAILED and marked not judged (AnswerStalled, the
	// coordinator's ruling (a)), never a pass on the recording; the set is
	// not judged.
	t.Run("with nothing of its answer", func(t *testing.T) {
		apps.mode.Store(sendNothing)
		t.Cleanup(func() { apps.mode.Store(asRecorded) })
		h := newTmplHarness(t, &tmplConf{}, cases, nil)
		core, logs := observer.New(zapcore.InfoLevel)
		h.r.logger = zap.New(core)
		status := h.run(t)
		if n := apps.exhausted.Load(); n != 0 {
			t.Fatalf("%d stream(s) sent every keepalive they had and were still read", n)
		}
		for name, says := range map[string]string{
			"test-1": "the app sent its answer's headers and nothing of its body but keepalives, for GET /stream",
			"test-2": "only keepalives, for /orders.Orders/Get",
		} {
			r := h.result(t, name)
			if r.Status != models.TestStatusFailed || !strings.Contains(r.FailureInfo.AnswerStalled, says) || r.FailureInfo.StreamHeldOpen != "" || len(r.VerdictUnknown()) == 0 {
				t.Fatalf("%s: %s, %+v; want FAILED, not judged: %q", name, r.Status, r.FailureInfo, says)
			}
		}
		if status != models.TestSetStatusInternalErr {
			t.Fatalf("the set is %s, want INTERNAL_ERR: not judged", status)
		}
		reason := h.report.report.FailureReason
		for _, want := range []string{"never answered 2 test(s)", "test-1", "test-2"} {
			if !strings.Contains(reason, want) {
				t.Fatalf("failure reason %q does not say %q", reason, want)
			}
		}
		// The console says the stream is not judged and why, not that its
		// read failed.
		if n := logs.FilterMessage("not judged: the app sent its answer's headers and nothing of its body but keepalives, for GET /stream").Len(); n != 1 {
			t.Fatalf("the console said %d times that test-1 is not judged, want once", n)
		}
		if n := logs.FilterMessage("failed to read streaming response").Len(); n != 0 {
			t.Fatalf("the console called test-1's stream a failed read %d time(s)", n)
		}
	})

	// The same with the stream's start differing (a 500 against the
	// recorded 200): what came is judged, a FAILED with no mark, never "not
	// judged"; the gRPC side, nothing of its answer, still fails closed.
	t.Run("with a start that differs and nothing more", func(t *testing.T) {
		apps.mode.Store(wrongStart)
		t.Cleanup(func() { apps.mode.Store(asRecorded) })
		h := newTmplHarness(t, &tmplConf{}, cases, nil)
		h.run(t)
		r := h.result(t, "test-1")
		if r.Status != models.TestStatusFailed || r.FailureInfo.AnswerStalled != "" || len(r.VerdictUnknown()) != 0 || r.Result.StatusCode.Normal {
			t.Fatalf("test-1: %s, %+v, status %+v; want a judged FAILED on the 500", r.Status, r.FailureInfo, r.Result.StatusCode)
		}
		if r := h.result(t, "test-2"); r.Status != models.TestStatusFailed || r.FailureInfo.AnswerStalled == "" {
			t.Fatalf("test-2: %s, %+v; want FAILED, not judged", r.Status, r.FailureInfo)
		}
		if reason := h.report.report.FailureReason; !strings.Contains(reason, "never answered 1 test(s) (test-2)") {
			t.Fatalf("failure reason %q; want test-2 alone not judged", reason)
		}
	})
}

// resultOf is the named test's result in the base-path run's report.
func (b *bpRun) resultOf(t *testing.T, name string) models.TestResult {
	t.Helper()
	b.report.mu.Lock()
	defer b.report.mu.Unlock()
	for _, r := range b.report.results {
		if r.TestCaseID == name {
			return r
		}
	}
	t.Fatalf("no result for %s", name)
	return models.TestResult{}
}

// Under the proven switch with no apiTimeout of the user's, a test the app
// never answers ends on the stall rule (pkg.AnswerStallGrace of keploy's
// runnable time) instead of waiting for good (rule 15). Nothing was compared,
// so its row is built as a test with no answer is, FAILED, which a reader of
// the status alone sees as a failure, never a pass; it is marked not judged
// (AnswerStalled), which JUnit, the NDJSON and the set read: the set is
// INTERNAL_ERR, not judged, and the run exits non-zero either way. The
// console says it is not judged and what was waited for, not an ordinary
// failure.
func TestUnderTheSwitchATestTheAppNeverAnswersIsNotJudged(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	saved := pkg.AnswerStallGrace
	pkg.AnswerStallGrace = 300 * time.Millisecond
	t.Cleanup(func() { pkg.AnswerStallGrace = saved })

	app := newBPApp(t)
	control := newBasePathRun(t, app.URL, "test-1", "test-2")
	if code, err := control.start(t); err != nil || code != 0 || control.report.report.Status != string(models.TestSetStatusPassed) {
		t.Fatalf("control: exit %d, %v, %+v; want every test answered and PASSED", code, err, control.report.report)
	}

	app.hold["/test-2"] = true
	run := newBasePathRun(t, app.URL, "test-1", "test-2")
	code, err := run.start(t)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	r := run.resultOf(t, "test-2")
	if r.Status != models.TestStatusFailed {
		t.Fatalf("the row is %s; a reader of the status alone must see no pass", r.Status)
	}
	want := "the app never answered; waited 300ms of keploy's runnable time for GET /test-2"
	if r.FailureInfo.AnswerStalled != want {
		t.Fatalf("AnswerStalled %q, want %q", r.FailureInfo.AnswerStalled, want)
	}
	if why := r.VerdictUnknown(); len(why) != 1 || why[0] != want {
		t.Fatalf("VerdictUnknown %v; want the test not judged, saying what was waited for", why)
	}
	if rep := run.report.report; rep == nil || rep.Status != string(models.TestSetStatusInternalErr) || !strings.Contains(rep.FailureReason, "never answered 1 test(s) (test-2)") {
		t.Fatalf("report %+v; want INTERNAL_ERR, not judged, naming test-2", rep)
	}
	if code == 0 {
		t.Fatal("a run with a test not judged exited 0")
	}
	// Without the mark: the row's FAILED alone is counted a failure, and
	// that count arms the run's exit code.
	if n := run.replayer.totalTestFailed; n != 1 {
		t.Fatalf("the run counted %d failed test(s), want the stalled row's FAILED", n)
	}
	saved2 := utils.ErrCode
	t.Cleanup(func() { utils.ErrCode = saved2 })
	utils.ErrCode = 0
	armRunExitCode(nil, true, run.replayer.totalTestFailed)
	if utils.ErrCode == 0 {
		t.Fatal("a FAILED row read without its mark left the exit code 0")
	}
	if said := run.said(zapcore.ErrorLevel, "not judged: "+want); len(said) != 1 {
		t.Fatalf("the console said %d times that test-2 is not judged and what was waited for, want once", len(said))
	}
	if said := run.said(zapcore.ErrorLevel, "failed to simulate request"); len(said) != 0 {
		t.Fatalf("the console called the stalled answer an ordinary failure: %v", said)
	}
}

// Under the switch an answer that began (its status and headers came) and
// then stalled with its body part-way is judged on its start first: a start
// that differs from the recording (a 500 against a recorded 200) is a judged
// failure, its set FAILED, never "not judged"; only a start that matches
// fails closed, FAILED with the not-judged mark, its set not judged.
func TestUnderTheSwitchAnAnswerThatBeganIsJudgedOnItsStart(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	saved := pkg.AnswerStallGrace
	pkg.AnswerStallGrace = 300 * time.Millisecond
	t.Cleanup(func() { pkg.AnswerStallGrace = saved })

	var status atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", strconv.Itoa(len(bpBody)))
		if r.URL.Path != "/test-2" {
			_, _ = w.Write([]byte(bpBody))
			return
		}
		w.WriteHeader(int(status.Load()))
		_, _ = w.Write([]byte(bpBody[:5]))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)

	for _, c := range []struct {
		name    string
		status  int32
		judged  bool
		set     models.TestSetStatus
		stalled string
	}{
		{"a start that differs", 500, true, models.TestSetStatusFailed, ""},
		{"a start that matches", 200, false, models.TestSetStatusInternalErr, "the app's answer stopped part-way, after 5 bytes of its body; waited 300ms of keploy's runnable time for GET /test-2"},
	} {
		t.Run(c.name, func(t *testing.T) {
			status.Store(c.status)
			run := newBasePathRun(t, srv.URL, "test-1", "test-2")
			if _, err := run.start(t); err != nil {
				t.Fatalf("Start: %v", err)
			}
			r := run.resultOf(t, "test-2")
			if r.Status != models.TestStatusFailed || r.FailureInfo.AnswerStalled != c.stalled || (len(r.VerdictUnknown()) == 0) != c.judged {
				t.Fatalf("test-2: %s, AnswerStalled %q, VerdictUnknown %v; want FAILED, %q, judged %v", r.Status, r.FailureInfo.AnswerStalled, r.VerdictUnknown(), c.stalled, c.judged)
			}
			if c.judged && r.Result.StatusCode.Normal {
				t.Fatalf("test-2's status compared normal: %+v; want the 500 judged against the recorded 200", r.Result.StatusCode)
			}
			if rep := run.report.report; rep == nil || rep.Status != string(c.set) {
				t.Fatalf("report %+v; want the set %s", rep, c.set)
			}
		})
	}
}
