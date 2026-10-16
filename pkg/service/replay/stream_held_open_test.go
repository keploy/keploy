package replay

import (
	"context"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// heldOpenLines is what the stand-in app streams, as recorded.
var heldOpenLines = []string{`{"id":1}`, `{"id":2}`}

// heldOpenApps is an app with a streamed HTTP answer and a gRPC side that,
// in mode holdPast, keep their streams open past their recorded answers, and
// in mode stallShort, short of them, sending keepalives: NDJSON blank lines,
// and gRPC PINGs (grpc-go sends one after a second without activity). Each
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
// recorded answer, and stalling short of it.
const (
	asRecorded int32 = iota
	holdPast
	stallShort
)

func newHeldOpenApps(t *testing.T) *heldOpenApps {
	t.Helper()
	apps := &heldOpenApps{}
	apps.http = newTmplApp(t, func(a *tmplApp, mux *http.ServeMux) {
		mux.HandleFunc("/stream", func(w http.ResponseWriter, r *http.Request) {
			a.record(r)
			w.Header().Set("Content-Type", "application/x-ndjson")
			mode := apps.mode.Load()
			for i, line := range heldOpenLines {
				if mode == stallShort && i > 0 {
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
			if mode != stallShort {
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
}
