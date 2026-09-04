package pkg

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"golang.org/x/net/context"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// rawSrvCodec passes bytes through untouched, so the fake server can speak
// gRPC without any .proto — the same trick the real parser uses.
type rawSrvCodec struct{}

func (rawSrvCodec) Name() string { return "proto" }
func (rawSrvCodec) Marshal(v interface{}) ([]byte, error) {
	if m, ok := v.(*rawMessage); ok {
		return m.data, nil
	}
	return nil, fmt.Errorf("unexpected type %T", v)
}
func (rawSrvCodec) Unmarshal(data []byte, v interface{}) error {
	if m, ok := v.(*rawMessage); ok {
		m.data = append([]byte(nil), data...)
		return nil
	}
	return fmt.Errorf("unexpected type %T", v)
}

// streamingServer answers every method by echoing back `respond` messages and
// finishing with `st`. It records how many request messages it received.
type streamingServer struct {
	respond  [][]byte
	st       error
	gotReqs  chan int
	holdOpen chan struct{} // when non-nil, never returns: models a bidi stream the server holds open
}

func (s *streamingServer) handle(_ interface{}, stream grpc.ServerStream) error {
	n := 0
	for {
		var m rawMessage
		if err := stream.RecvMsg(&m); err != nil {
			break
		}
		n++
	}
	if s.gotReqs != nil {
		s.gotReqs <- n
	}
	for _, r := range s.respond {
		if err := stream.SendMsg(&rawMessage{data: r}); err != nil {
			return err
		}
	}
	if s.holdOpen != nil {
		<-s.holdOpen // never completes within the test's patience
	}
	return s.st
}

func startStreamingServer(t *testing.T, srv *streamingServer) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	g := grpc.NewServer(
		grpc.ForceServerCodec(rawSrvCodec{}),
		grpc.UnknownServiceHandler(srv.handle),
	)
	go func() { _ = g.Serve(ln) }()
	t.Cleanup(func() { g.Stop(); _ = ln.Close() })
	return ln.Addr().String()
}

// pbMsg builds a trivial protobuf payload: field 1, length-delimited string.
func pbMsg(s string) []byte { return append([]byte{0x0a, byte(len(s))}, s...) }

func grpcTestCase(addr string, reqMsgs []models.GrpcLengthPrefixedMessage) *models.TestCase {
	req := models.GrpcReq{
		Headers: models.GrpcHeaders{
			PseudoHeaders: map[string]string{
				":authority": addr,
				":path":      "/pkg.Svc/Method",
				":method":    "POST",
			},
			OrdinaryHeaders: map[string]string{"content-type": "application/grpc"},
		},
	}
	req.SetMessages(reqMsgs)
	return &models.TestCase{Name: "stream-replay", Kind: models.GRPC_EXPORT, GrpcReq: req}
}

// TestSimulateGRPC_DrainsEveryResponseMessage is the core of streaming replay.
//
// A single RecvMsg captured the first message of a server-streaming response
// and abandoned the rest — and by tearing the stream down early it also meant
// the app handler aborted, so dependency calls behind messages 2..N never ran.
func TestSimulateGRPC_DrainsEveryResponseMessage(t *testing.T) {
	want := [][]byte{pbMsg("one"), pbMsg("two"), pbMsg("three")}
	addr := startStreamingServer(t, &streamingServer{respond: want})

	tc := grpcTestCase(addr, []models.GrpcLengthPrefixedMessage{{}})
	resp, err := SimulateGRPC(context.Background(), tc, "set", zap.NewNop(), SimulationConfig{APITimeout: 10})
	if err != nil {
		t.Fatalf("SimulateGRPC: %v", err)
	}
	got := resp.AllMessages()
	if len(got) != len(want) {
		t.Fatalf("captured %d response messages, want %d. A single RecvMsg takes the first and "+
			"abandons the rest of the stream.", len(got), len(want))
	}
	for i := range want {
		if int(got[i].MessageLength) != len(want[i]) {
			t.Fatalf("message %d has length %d, want %d", i, got[i].MessageLength, len(want[i]))
		}
	}
}

// TestSimulateGRPC_SendsEveryRequestMessage: a client-streaming call records N
// request messages; sending only the first replays a different request than
// the one captured.
func TestSimulateGRPC_SendsEveryRequestMessage(t *testing.T) {
	gotReqs := make(chan int, 1)
	addr := startStreamingServer(t, &streamingServer{respond: [][]byte{pbMsg("ack")}, gotReqs: gotReqs})

	reqs := []models.GrpcLengthPrefixedMessage{
		{MessageLength: 5, DecodedData: `1: {"one"}`},
		{MessageLength: 5, DecodedData: `1: {"two"}`},
		{MessageLength: 7, DecodedData: `1: {"three"}`},
	}
	tc := grpcTestCase(addr, reqs)
	if _, err := SimulateGRPC(context.Background(), tc, "set", zap.NewNop(), SimulationConfig{APITimeout: 10}); err != nil {
		t.Fatalf("SimulateGRPC: %v", err)
	}
	select {
	case n := <-gotReqs:
		if n != len(reqs) {
			t.Fatalf("the server received %d request messages, want %d", n, len(reqs))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server never reported")
	}
}

// TestSimulateGRPC_TrailersAreRealNotFabricated is the ordering bug.
//
// grpc-go only populates Trailer() once RecvMsg has returned a non-nil error.
// Reading it right after the first message returns an EMPTY map, and the
// fixup below then fabricates grpc-status "0" — so a stream that failed
// replays as a clean success, which is the worst possible outcome because it
// looks correct.
func TestSimulateGRPC_TrailersAreRealNotFabricated(t *testing.T) {
	addr := startStreamingServer(t, &streamingServer{
		respond: [][]byte{pbMsg("partial")},
		st:      status.Error(codes.Internal, "boom"),
	})

	tc := grpcTestCase(addr, []models.GrpcLengthPrefixedMessage{{}})
	resp, err := SimulateGRPC(context.Background(), tc, "set", zap.NewNop(), SimulationConfig{APITimeout: 10})
	if err != nil {
		t.Fatalf("SimulateGRPC: %v", err)
	}
	gotStatus := resp.Trailers.OrdinaryHeaders["grpc-status"]
	wantStatus := fmt.Sprintf("%d", codes.Internal)
	if gotStatus != wantStatus {
		t.Fatalf("grpc-status = %q, want %q. Trailers must be read AFTER the drain reaches "+
			"io.EOF; reading them earlier yields an empty map and the fixup fabricates \"0\", "+
			"so a failed RPC replays as a pass.", gotStatus, wantStatus)
	}
}

// TestSimulateGRPC_HeldOpenStreamIsBounded: draining to io.EOF is not
// self-limiting the way one RecvMsg was. A bidi stream the server holds open
// — server reflection is exactly that shape — must fail this ONE test case
// rather than block until the whole test-run context dies.
func TestSimulateGRPC_HeldOpenStreamIsBounded(t *testing.T) {
	hold := make(chan struct{})
	t.Cleanup(func() { close(hold) })
	addr := startStreamingServer(t, &streamingServer{respond: [][]byte{pbMsg("first")}, holdOpen: hold})

	tc := grpcTestCase(addr, []models.GrpcLengthPrefixedMessage{{}})
	done := make(chan struct{})
	start := time.Now()
	go func() {
		defer close(done)
		_, _ = SimulateGRPC(context.Background(), tc, "set", zap.NewNop(), SimulationConfig{APITimeout: 2})
	}()
	select {
	case <-done:
		if elapsed := time.Since(start); elapsed > 10*time.Second {
			t.Fatalf("returned after %v, far beyond the 2s APITimeout", elapsed)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("SimulateGRPC never returned against a stream the server holds open. The drain " +
			"loop must be bounded by APITimeout, or one bad test case hangs the entire test set.")
	}
}

// keep the framing helpers honest about their assumptions
var _ = binary.BigEndian
var _ = io.EOF
var _ = metadata.MD{}
