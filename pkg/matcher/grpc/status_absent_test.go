package grpc

import (
	"context"
	"net"
	"strings"
	"sync/atomic"
	"testing"

	"go.keploy.io/server/v3/pkg"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/encoding"
	"google.golang.org/grpc/status"
)

// splitTrailersRecording is the answer keploy before v3.6.148 recorded for a
// call whose trailers a peer split across a HEADERS frame and CONTINUATION
// frames: its headers copied in as its trailers, with no grpc-status (the
// call itself ended INTERNAL). Read off that recorder, fed a real grpc-go
// call through a tee.
func splitTrailersRecording() models.GrpcResp {
	resp := models.GrpcResp{
		Headers: models.GrpcHeaders{
			PseudoHeaders:   map[string]string{":status": "200"},
			OrdinaryHeaders: map[string]string{"content-type": "application/grpc+proto"},
		},
		Trailers: models.GrpcHeaders{
			PseudoHeaders:   map[string]string{":status": "200"},
			OrdinaryHeaders: map[string]string{"content-type": "application/grpc+proto"},
		},
	}
	// The app's message, field 1 "ok", as the recorder keeps it.
	resp.SetMessages([]models.GrpcLengthPrefixedMessage{pkg.CreateLengthPrefixedMessageFromPayload([]byte{0, 0, 0, 0, 4, 0x0a, 2, 'o', 'k'})})
	return resp
}

func withGrpcStatus(resp models.GrpcResp, status string, present bool) *models.GrpcResp {
	out := resp
	out.Trailers = models.GrpcHeaders{PseudoHeaders: map[string]string{}, OrdinaryHeaders: map[string]string{}}
	if present {
		out.Trailers.OrdinaryHeaders["grpc-status"] = status
	}
	return &out
}

// Under the proven switch an answer with no grpc-status is not an OK answer:
// a gRPC client reads it as an error (grpc-go: UNKNOWN), and a recording that
// lacks it may have lost it (the recorder before v3.6.148, for trailers split
// across CONTINUATION frames). It matches no answer that carries a status,
// OK or UNKNOWN; two answers without one, the same. Two codes compare as
// codes, and a status that is no number matches only itself. With the
// switch off nothing changes: an absent status reads as 0, as it did.
func TestMatch_ProvenSwitchAnAnswerWithNoGrpcStatusIsNotOK(t *testing.T) {
	const none = "\x00none"
	for _, c := range []struct {
		name             string
		recorded, live   string
		want, wantOff    bool
		wantExp, wantAct int
	}{
		{"recorded none, live OK", none, "0", false, true, -1, 0},
		{"recorded OK, live none", "0", none, false, true, 0, -1},
		{"recorded none, live UNKNOWN", none, "2", false, false, -1, 2},
		{"recorded none, live INTERNAL", none, "13", false, false, -1, 13},
		{"both none", none, none, true, true, -1, -1},
		{"same code", "13", "13", true, true, 13, 13},
		{"same code written two ways", "0", "00", true, true, 0, 0},
		{"codes differ", "0", "13", false, false, 0, 13},
		{"not numbers, differ", "abc", "xyz", false, true, -1, -1},
		{"not numbers, same", "abc", "abc", true, true, -1, -1},
		{"not a number, live none", "abc", none, false, false, -1, -1},
	} {
		t.Run(c.name, func(t *testing.T) {
			rec := splitTrailersRecording()
			tc := &models.TestCase{Name: "status", GrpcResp: *withGrpcStatus(rec, c.recorded, c.recorded != none)}
			live := withGrpcStatus(rec, c.live, c.live != none)

			t.Setenv(provenswitch.Env, "")
			if ok, _ := Match(tc, live, nil, false, zap.NewNop(), false); ok != c.wantOff {
				t.Fatalf("switch off: matched %v, want %v (unchanged)", ok, c.wantOff)
			}

			t.Setenv(provenswitch.Env, "1")
			ok, res := Match(tc, live, nil, false, zap.NewNop(), false)
			if ok != c.want {
				t.Fatalf("KEPLOY_PROVEN=1: matched %v, want %v", ok, c.want)
			}
			if res.StatusCode.Normal != c.want {
				t.Fatalf("KEPLOY_PROVEN=1: StatusCode.Normal %v, want %v", res.StatusCode.Normal, c.want)
			}
			if res.StatusCode.Expected != c.wantExp || res.StatusCode.Actual != c.wantAct {
				t.Fatalf("KEPLOY_PROVEN=1: StatusCode %d -> %d, want %d -> %d (no status is -1, never 0)",
					res.StatusCode.Expected, res.StatusCode.Actual, c.wantExp, c.wantAct)
			}
			if !c.want && !hasCategory(res, models.StatusCodeChanged) {
				t.Fatalf("KEPLOY_PROVEN=1: a status difference not said as StatusCodeChanged: %v", res.FailureInfo.Category)
			}
		})
	}
}

func hasCategory(res *models.Result, want models.FailureCategory) bool {
	for _, c := range res.FailureInfo.Category {
		if c == want {
			return true
		}
	}
	return false
}

// A recording with no grpc-status, replayed under the switch, says why it
// fails once, by test case: what the recording lacks and how to get a
// recording that has it. Nothing is said for a probe (no failure logs), nor
// with the switch off.
func TestMatch_ProvenSwitchSaysARecordingHasNoGrpcStatus(t *testing.T) {
	rec := splitTrailersRecording()
	tc := &models.TestCase{Name: "test-7", GrpcResp: rec}
	live := withGrpcStatus(rec, "0", true)
	warned := func(emit bool) []observer.LoggedEntry {
		core, logs := observer.New(zapcore.WarnLevel)
		_, _ = Match(tc, live, nil, false, zap.New(core), emit)
		return logs.FilterMessageSnippet("no grpc-status").All()
	}

	t.Setenv(provenswitch.Env, "")
	if got := warned(true); len(got) != 0 {
		t.Fatalf("switch off: said %v", got)
	}
	t.Setenv(provenswitch.Env, "1")
	if got := warned(false); len(got) != 0 {
		t.Fatalf("KEPLOY_PROVEN=1, no failure logs: said %v", got)
	}
	got := warned(true)
	if len(got) != 1 {
		t.Fatalf("KEPLOY_PROVEN=1: said %d times, want once", len(got))
	}
	if !strings.Contains(got[0].Message, "record") || got[0].ContextMap()["testcase"] != "test-7" {
		t.Fatalf("KEPLOY_PROVEN=1: %q %v does not name the test case and say to record it again", got[0].Message, got[0].ContextMap())
	}
	for _, c := range []struct {
		name     string
		recorded *models.GrpcResp
		live     *models.GrpcResp
	}{
		{"a recording that has its status", withGrpcStatus(rec, "0", true), withGrpcStatus(rec, "13", true)},
		// The answer's trailers are the recording's own (a stream the app
		// held open): the status is not what failed it.
		{"an answer with the recording's trailers", &rec, &rec},
	} {
		core, logs := observer.New(zapcore.WarnLevel)
		_, _ = Match(&models.TestCase{Name: "x", GrpcResp: *c.recorded}, c.live, nil, false, zap.New(core), true)
		if got := logs.FilterMessageSnippet("no grpc-status").All(); len(got) != 0 {
			t.Fatalf("KEPLOY_PROVEN=1: said for %s: %v", c.name, got)
		}
	}
}

// replayCodec passes a message's bytes through, so the app below speaks gRPC
// without a .proto.
type replayCodec struct{}

type replayMsg struct{ b []byte }

func (replayCodec) Name() string { return "keploy-status-absent-test" }
func (replayCodec) Marshal(v any) ([]byte, error) {
	return v.(*replayMsg).b, nil
}
func (replayCodec) Unmarshal(data []byte, v any) error {
	v.(*replayMsg).b = append([]byte(nil), data...)
	return nil
}

var _ encoding.Codec = replayCodec{}

// The replay path end to end: a recording with no grpc-status (the recorder
// before v3.6.148, on a call that ended INTERNAL), replayed against an app
// that now answers the same message and OK. keploy's replay client reads the
// app's answer (SimulateGRPC) and the comparison judges it: under the switch
// the test fails, where it passed (a failed call recorded, a successful one
// replayed, and the test green).
func TestReplayOfARecordingWithNoGrpcStatusAgainstAnOKAppFails(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer(grpc.ForceServerCodec(replayCodec{}), grpc.UnknownServiceHandler(func(_ any, stream grpc.ServerStream) error {
		var m replayMsg
		for stream.RecvMsg(&m) == nil {
		}
		return stream.SendMsg(&replayMsg{b: []byte{0x0a, 2, 'o', 'k'}})
	}))
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(srv.Stop)

	req := models.GrpcReq{Headers: models.GrpcHeaders{
		PseudoHeaders:   map[string]string{":authority": ln.Addr().String(), ":path": "/svc.Echo/Call", ":method": "POST"},
		OrdinaryHeaders: map[string]string{"content-type": "application/grpc"},
	}}
	req.SetMessages([]models.GrpcLengthPrefixedMessage{pkg.CreateLengthPrefixedMessageFromPayload([]byte{0, 0, 0, 0, 3, 0x0a, 1, 'q'})})
	tc := &models.TestCase{Name: "split-trailers", Kind: models.GRPC_EXPORT, GrpcReq: req, GrpcResp: splitTrailersRecording()}

	for _, on := range []bool{false, true} {
		env := ""
		if on {
			env = "1"
		}
		t.Setenv(provenswitch.Env, env)
		live, err := pkg.SimulateGRPC(context.Background(), tc, "set", zap.NewNop(), pkg.SimulationConfig{APITimeout: 10})
		if err != nil {
			t.Fatalf("KEPLOY_PROVEN=%q: SimulateGRPC: %v", env, err)
		}
		if got := live.Trailers.OrdinaryHeaders["grpc-status"]; got != "0" {
			t.Fatalf("KEPLOY_PROVEN=%q: the app's answer has grpc-status %q, want 0", env, got)
		}
		ok, _ := Match(tc, live, nil, false, zap.NewNop(), false)
		if ok == on {
			t.Fatalf("KEPLOY_PROVEN=%q: matched %v; a recording with no grpc-status must fail against an OK answer under the switch, and the switch off is unchanged", env, ok)
		}
	}
}

// trailersOnlyRecording is what enterprise's low-latency (proxyless) recorder
// kept, before its fix, of a Trailers-Only answer (one HEADERS frame that
// ends the stream: grpc-go's answer to a call that ends before it sends
// headers or a message, OK included): the block as the answer's headers
// alone, no trailers, and no message. Read off that recorder, fed real
// grpc-go calls through a tee (a server stream that returned no message, and
// one that failed NOT_FOUND).
func trailersOnlyRecording(code, message string) models.GrpcResp {
	headers := map[string]string{"content-type": "application/grpc", "grpc-status": code}
	if message != "" {
		headers["grpc-message"] = message
	}
	return models.GrpcResp{
		Headers:  models.GrpcHeaders{PseudoHeaders: map[string]string{":status": "200"}, OrdinaryHeaders: headers},
		Trailers: models.GrpcHeaders{PseudoHeaders: map[string]string{}, OrdinaryHeaders: map[string]string{}},
	}
}

// liveAnswer is an answer as keploy's replay client reads it
// (pkg.SimulateGRPC): its status in its trailers.
func liveAnswer(code string, present bool) *models.GrpcResp {
	resp := &models.GrpcResp{
		Headers:  models.GrpcHeaders{PseudoHeaders: map[string]string{":status": "200"}, OrdinaryHeaders: map[string]string{"content-type": "application/grpc"}},
		Trailers: models.GrpcHeaders{PseudoHeaders: map[string]string{}, OrdinaryHeaders: map[string]string{}},
	}
	if present {
		resp.Trailers.OrdinaryHeaders["grpc-status"] = code
	}
	return resp
}

// Under the proven switch a Trailers-Only recording's status is read off its
// one header block, as a gRPC client reads it: it has its status, and it
// matches the same status, OK or an error, and no other. Read as absent,
// the empty stream an app answers OK, byte for byte as recorded, failed. It
// is not a recording without a status, so nothing says to record it again.
// With the switch off nothing changes: the status in its headers is not read.
func TestMatch_ProvenSwitchReadsATrailersOnlyRecordingsStatusOffItsHeaders(t *testing.T) {
	for _, c := range []struct {
		name             string
		recorded         models.GrpcResp
		live             *models.GrpcResp
		want, wantOff    bool
		wantExp, wantAct int
	}{
		{"OK, live OK", trailersOnlyRecording("0", ""), liveAnswer("0", true), true, true, 0, 0},
		{"NOT_FOUND, live NOT_FOUND", trailersOnlyRecording("5", "no such thing"), liveAnswer("5", true), true, false, 5, 5},
		{"NOT_FOUND, live OK", trailersOnlyRecording("5", "no such thing"), liveAnswer("0", true), false, true, 5, 0},
		{"OK, live NOT_FOUND", trailersOnlyRecording("0", ""), liveAnswer("5", true), false, false, 0, 5},
		{"OK, live none", trailersOnlyRecording("0", ""), liveAnswer("", false), false, true, 0, -1},
		// The answer's status is the one grpc-go read: never off the
		// headers of the app's answer.
		{"OK, live none with a status in its headers", trailersOnlyRecording("0", ""), &models.GrpcResp{Headers: trailersOnlyRecording("0", "").Headers}, false, true, 0, -1},
	} {
		t.Run(c.name, func(t *testing.T) {
			tc := &models.TestCase{Name: "trailers-only", GrpcResp: c.recorded}

			t.Setenv(provenswitch.Env, "")
			if ok, _ := Match(tc, c.live, nil, false, zap.NewNop(), false); ok != c.wantOff {
				t.Fatalf("switch off: matched %v, want %v (unchanged)", ok, c.wantOff)
			}

			t.Setenv(provenswitch.Env, "1")
			core, logs := observer.New(zapcore.WarnLevel)
			ok, res := Match(tc, c.live, nil, false, zap.New(core), true)
			if ok != c.want || res.StatusCode.Normal != c.want {
				t.Fatalf("KEPLOY_PROVEN=1: matched %v (status normal %v), want %v", ok, res.StatusCode.Normal, c.want)
			}
			if res.StatusCode.Expected != c.wantExp || res.StatusCode.Actual != c.wantAct {
				t.Fatalf("KEPLOY_PROVEN=1: StatusCode %d -> %d, want %d -> %d", res.StatusCode.Expected, res.StatusCode.Actual, c.wantExp, c.wantAct)
			}
			if got := logs.FilterMessageSnippet("no grpc-status").All(); len(got) != 0 {
				t.Fatalf("KEPLOY_PROVEN=1: said a recording that has its status has none: %v", got)
			}
		})
	}
}

// A recording whose trailers lack grpc-status and whose headers carry one is
// read as a client reads it, off its trailers: no status. It fails against
// an answer with one, and it is not a recording with no status anywhere, so
// it is not told to be recorded again.
func TestMatch_ProvenSwitchReadsTrailersThatLackTheStatusAsNone(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	rec := trailersOnlyRecording("0", "")
	rec.Trailers.OrdinaryHeaders["x-done"] = "yes"
	core, logs := observer.New(zapcore.WarnLevel)
	ok, res := Match(&models.TestCase{Name: "x", GrpcResp: rec}, liveAnswer("0", true), nil, false, zap.New(core), true)
	if ok || res.StatusCode.Expected != -1 {
		t.Fatalf("matched %v, recorded status %d; want a failure, the recorded status none (-1)", ok, res.StatusCode.Expected)
	}
	if got := logs.FilterMessageSnippet("no grpc-status").All(); len(got) != 0 {
		t.Fatalf("said a recording with a status in its headers has none: %v", got)
	}
}

// The replay path end to end for Trailers-Only recordings (the low-latency
// recorder before its fix): an app's server stream that returns no message,
// and one that fails NOT_FOUND, each answering in one HEADERS frame. keploy's
// replay client reads the app's answer (SimulateGRPC) and the comparison
// judges it. Under the switch the app answering as recorded passes and the
// app that now answers OK where it failed NOT_FOUND fails. With the switch
// off nothing changes.
func TestReplayOfATrailersOnlyRecordingIsJudgedOnItsStatus(t *testing.T) {
	var missingNowOK atomic.Bool
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer(grpc.ForceServerCodec(replayCodec{}), grpc.UnknownServiceHandler(func(_ any, stream grpc.ServerStream) error {
		var m replayMsg
		for stream.RecvMsg(&m) == nil {
		}
		if method, _ := grpc.MethodFromServerStream(stream); method == "/svc.Echo/Missing" && !missingNowOK.Load() {
			return status.Error(codes.NotFound, "no such thing")
		}
		return nil // no message: the answer is one HEADERS frame, OK
	}))
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(srv.Stop)

	testCase := func(path string, recorded models.GrpcResp) *models.TestCase {
		req := models.GrpcReq{Headers: models.GrpcHeaders{
			PseudoHeaders:   map[string]string{":authority": ln.Addr().String(), ":path": path, ":method": "POST"},
			OrdinaryHeaders: map[string]string{"content-type": "application/grpc"},
		}}
		req.SetMessages([]models.GrpcLengthPrefixedMessage{pkg.CreateLengthPrefixedMessageFromPayload([]byte{0, 0, 0, 0, 3, 0x0a, 1, 'q'})})
		return &models.TestCase{Name: path, Kind: models.GRPC_EXPORT, GrpcReq: req, GrpcResp: recorded}
	}
	for _, c := range []struct {
		name          string
		tc            *models.TestCase
		missingNowOK  bool
		want, wantOff bool
	}{
		{"an empty stream, answered as recorded", testCase("/svc.Echo/Empty", trailersOnlyRecording("0", "")), false, true, true},
		{"NOT_FOUND, answered as recorded", testCase("/svc.Echo/Missing", trailersOnlyRecording("5", "no such thing")), false, true, false},
		{"NOT_FOUND, now answered OK", testCase("/svc.Echo/Missing", trailersOnlyRecording("5", "no such thing")), true, false, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			missingNowOK.Store(c.missingNowOK)
			for _, on := range []bool{false, true} {
				env, want := "", c.wantOff
				if on {
					env, want = "1", c.want
				}
				t.Setenv(provenswitch.Env, env)
				live, err := pkg.SimulateGRPC(context.Background(), c.tc, "set", zap.NewNop(), pkg.SimulationConfig{APITimeout: 10})
				if err != nil {
					t.Fatalf("KEPLOY_PROVEN=%q: SimulateGRPC: %v", env, err)
				}
				if ok, res := Match(c.tc, live, nil, false, zap.NewNop(), false); ok != want {
					t.Fatalf("KEPLOY_PROVEN=%q: matched %v, want %v (status %d -> %d)", env, ok, want, res.StatusCode.Expected, res.StatusCode.Actual)
				}
			}
		})
	}
}
