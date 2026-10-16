package pkg

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/status"
)

// h2App is a gRPC app written frame by frame, so a test says exactly what it
// sends: messages, trailers, and keepalives (PINGs), each sent only once the
// client has answered the one before. Nothing in it waits on a clock.
type h2App struct {
	addr string
	// keepalives counts the PINGs it sent.
	keepalives atomic.Int32
	// exhausted closes when the script sent every keepalive it had and the
	// client still had not ended the stream: the hang.
	exhausted chan struct{}
}

// h2Stream is the call the script answers.
type h2Stream struct {
	app   *h2App
	mu    sync.Mutex
	fr    *http2.Framer
	hbuf  bytes.Buffer
	enc   *hpack.Encoder
	id    uint32
	acks  chan struct{}
	reset chan struct{}
}

func (s *h2Stream) write(f func() error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return f()
}

func (s *h2Stream) headers(end bool, kv ...string) error {
	return s.write(func() error {
		s.hbuf.Reset()
		for i := 0; i+1 < len(kv); i += 2 {
			if err := s.enc.WriteField(hpack.HeaderField{Name: kv[i], Value: kv[i+1]}); err != nil {
				return err
			}
		}
		return s.fr.WriteHeaders(http2.HeadersFrameParam{StreamID: s.id, BlockFragment: s.hbuf.Bytes(), EndHeaders: true, EndStream: end})
	})
}

func (s *h2Stream) message(b []byte) error {
	frame := make([]byte, 5+len(b))
	binary.BigEndian.PutUint32(frame[1:5], uint32(len(b)))
	copy(frame[5:], b)
	return s.write(func() error { return s.fr.WriteData(s.id, false, frame) })
}

func (s *h2Stream) trailers(code codes.Code) error {
	return s.headers(true, "grpc-status", strconv.Itoa(int(code)))
}

// trailersOnly is an answer of trailers alone: one HEADERS frame that ends the
// stream.
func (s *h2Stream) trailersOnly(code codes.Code) error {
	return s.headers(true, ":status", "200", "content-type", "application/grpc", "grpc-status", strconv.Itoa(int(code)))
}

// keepalive sends a PING and waits for its acknowledgement; false once the
// client reset the stream or the connection is gone.
func (s *h2Stream) keepalive() bool {
	select {
	case <-s.reset:
		return false
	default:
	}
	if err := s.write(func() error { return s.fr.WritePing(false, [8]byte{}) }); err != nil {
		return false
	}
	s.app.keepalives.Add(1)
	select {
	case <-s.acks:
		return true
	case <-s.reset:
		return false
	}
}

// pingNow sends a PING without waiting for its acknowledgement.
func (s *h2Stream) pingNow() error {
	if err := s.write(func() error { return s.fr.WritePing(false, [8]byte{}) }); err != nil {
		return err
	}
	s.app.keepalives.Add(1)
	return nil
}

// holdOpen sends keepalives until the client ends the stream; when it sent
// limit and the client had not, the app is exhausted.
func (s *h2Stream) holdOpen(limit int) {
	for i := 0; i < limit; i++ {
		if !s.keepalive() {
			return
		}
	}
	close(s.app.exhausted)
}

// startH2App serves gRPC calls on a listener of its own: on each, once the
// request has ended, script answers it.
func startH2App(t *testing.T, script func(s *h2Stream)) *h2App {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	app := &h2App{addr: ln.Addr().String(), exhausted: make(chan struct{})}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go app.serve(c, script)
		}
	}()
	return app
}

func (a *h2App) serve(c net.Conn, script func(s *h2Stream)) {
	defer c.Close()
	if _, err := io.ReadFull(c, make([]byte, len(http2.ClientPreface))); err != nil {
		return
	}
	s := &h2Stream{app: a, fr: http2.NewFramer(c, c), acks: make(chan struct{}, 64), reset: make(chan struct{})}
	s.enc = hpack.NewEncoder(&s.hbuf)
	var resetOnce sync.Once
	closeReset := func() { resetOnce.Do(func() { close(s.reset) }) }
	defer closeReset()
	if err := s.write(func() error { return s.fr.WriteSettings() }); err != nil {
		return
	}
	started := false
	for {
		f, err := s.fr.ReadFrame()
		if err != nil {
			return
		}
		switch f := f.(type) {
		case *http2.SettingsFrame:
			if !f.IsAck() {
				_ = s.write(func() error { return s.fr.WriteSettingsAck() })
			}
		case *http2.PingFrame:
			if f.IsAck() {
				s.acks <- struct{}{}
				continue
			}
			data := f.Data
			_ = s.write(func() error { return s.fr.WritePing(true, data) })
		case *http2.HeadersFrame:
			s.id = f.StreamID
		case *http2.DataFrame:
			if f.StreamEnded() && !started {
				started = true
				go script(s)
			}
		case *http2.RSTStreamFrame:
			closeReset()
		}
	}
}

// heldOpenCall replays tc against the app under the proven switch, with no
// clock on the app's answer, and returns what SimulateGRPC returned; or fails
// once the app has sent every keepalive it had and the call still waits.
func heldOpenCall(t *testing.T, tc *models.TestCase, exhausted <-chan struct{}) (*models.GrpcResp, error) {
	t.Helper()
	t.Setenv(provenswitch.Env, "1")
	type result struct {
		resp *models.GrpcResp
		err  error
	}
	done := make(chan result, 1)
	go func() {
		resp, err := SimulateGRPC(context.Background(), tc, "set", zap.NewNop(), SimulationConfig{APITimeout: 1, Unbounded: true})
		done <- result{resp, err}
	}()
	select {
	case r := <-done:
		return r.resp, r.err
	case <-exhausted:
		t.Fatal("the app kept the stream open past its recording, sent keepalive after keepalive, and the call still waits: with no clock it hangs")
		return nil, nil
	}
}

// recordedAs is tc with msgs as its recorded answer.
func recordedAs(tc *models.TestCase, msgs ...[]byte) *models.TestCase {
	recorded := make([]models.GrpcLengthPrefixedMessage, len(msgs))
	for i, m := range msgs {
		recorded[i] = createLengthPrefixedMessage(m)
	}
	tc.GrpcResp.SetMessages(recorded)
	return tc
}

// withStatus is tc with code as its recorded answer's status.
func withStatus(tc *models.TestCase, code codes.Code) *models.TestCase {
	tc.GrpcResp.Trailers.OrdinaryHeaders = map[string]string{"grpc-status": strconv.Itoa(int(code))}
	return tc
}

func grpcStatusOf(resp *models.GrpcResp) string {
	if resp == nil {
		return ""
	}
	return resp.Trailers.OrdinaryHeaders["grpc-status"]
}

// Under the proven switch, with no clock on the app's answer (row W1), a gRPC
// stream the app keeps open past its recording ends on its keepalives: every
// recorded message came, then PINGs and no end. The first PING after the
// request's end may be the app's last flow-control ping, so the second ends
// the wait: SimulateGRPC returns the messages that came before it, with
// ErrStreamHeldOpen (no verdict). A message beyond the recording among them
// is returned with them, so the comparison fails on it, as before.
func TestAGRPCStreamHeldOpenPastItsRecordingEndsOnItsKeepalives(t *testing.T) {
	for _, tc := range []struct {
		name     string
		sent     [][]byte
		recorded [][]byte
	}{
		{"every recorded message, then keepalives", [][]byte{pbMsg("one"), pbMsg("two")}, [][]byte{pbMsg("one"), pbMsg("two")}},
		{"a message beyond the recording, then keepalives", [][]byte{pbMsg("one"), pbMsg("two")}, [][]byte{pbMsg("one")}},
		{"an answer of trailers alone recorded, then keepalives", nil, [][]byte{nil}},
	} {
		recordedCode := codes.OK
		if tc.sent == nil {
			recordedCode = codes.NotFound
		}
		t.Run(tc.name, func(t *testing.T) {
			app := startH2App(t, func(s *h2Stream) {
				if s.headers(false, ":status", "200", "content-type", "application/grpc") != nil {
					return
				}
				for _, m := range tc.sent {
					if s.message(m) != nil {
						return
					}
				}
				s.holdOpen(200)
			})
			resp, err := heldOpenCall(t, withStatus(recordedAs(grpcTestCase(app.addr, []models.GrpcLengthPrefixedMessage{{}}), tc.recorded...), recordedCode), app.exhausted)
			if !errors.Is(err, ErrStreamHeldOpen) || errors.Is(err, ErrStreamStalled) {
				t.Fatalf("SimulateGRPC: %v; want ErrStreamHeldOpen", err)
			}
			if got := len(resp.AllMessages()); got != max(len(tc.sent), 1) {
				t.Fatalf("returned %d messages, want what came (%d)", got, len(tc.sent))
			}
			if n := app.keepalives.Load(); n < 2 {
				t.Fatalf("the call ended after the app's keepalive %d; the first after the request may be a flow-control ping, so not before the second", n)
			}
		})
	}
}

// A stream that stalls short of its recording ends on the app's keepalives
// too (row W1, round 2): two in a row with no message between them, and the
// app neither ended the stream nor sent the rest of its answer. It has no
// verdict (ErrStreamStalled, an ErrStreamHeldOpen): what came is returned,
// then the recording's rest, so only what came is compared.
func TestAGRPCStreamThatStallsShortOfItsRecordingEndsOnItsKeepalives(t *testing.T) {
	app := startH2App(t, func(s *h2Stream) {
		if s.headers(false, ":status", "200", "content-type", "application/grpc") != nil || s.message(pbMsg("one")) != nil {
			return
		}
		s.holdOpen(200)
	})
	resp, err := heldOpenCall(t, recordedAs(grpcTestCase(app.addr, []models.GrpcLengthPrefixedMessage{{}}), pbMsg("one"), pbMsg("two")), app.exhausted)
	if !errors.Is(err, ErrStreamStalled) || !errors.Is(err, ErrStreamHeldOpen) {
		t.Fatalf("SimulateGRPC: %v; want ErrStreamStalled, an ErrStreamHeldOpen", err)
	}
	got := resp.AllMessages()
	if len(got) != 2 || int(got[0].MessageLength) != len(pbMsg("one")) || int(got[1].MessageLength) != len(pbMsg("two")) {
		t.Fatalf("returned %+v, want the message that came, then the recording's rest", got)
	}
	if n := app.keepalives.Load(); n < 3 {
		t.Fatalf("the call ended after the app's keepalive %d; the first after the request may be a flow-control ping, so not before the third", n)
	}
}

// An answer that is slow to come is the app's, judged, while its app sends
// fewer than two keepalives after the first: an answer of trailers alone,
// recorded with its error status, and an empty message (google.protobuf.Empty),
// recorded as one empty message with status 0, which a recording of trailers
// alone is not. A third keepalive with nothing is a stall: not judged, the
// count keploy waits by.
func TestASlowAnswerIsJudgedUntilTheAppStallsOnItsKeepalives(t *testing.T) {
	for _, tc := range []struct {
		name       string
		keepalives int
		answer     func(s *h2Stream) error
		recorded   *models.TestCase
		stalled    bool
		// headersFirst: the app sends its response headers before its
		// keepalives.
		headersFirst bool
	}{
		{"trailers alone", 2, func(s *h2Stream) error { return s.trailersOnly(codes.NotFound) },
			withStatus(recordedAs(&models.TestCase{}, nil), codes.NotFound), false, false},
		{"an empty message", 2, func(s *h2Stream) error {
			if err := s.headers(false, ":status", "200", "content-type", "application/grpc"); err != nil {
				return err
			}
			if err := s.message(nil); err != nil {
				return err
			}
			return s.trailers(codes.OK)
		}, withStatus(recordedAs(&models.TestCase{}, nil), codes.OK), false, false},
		{"an empty message after its headers", 2, func(s *h2Stream) error {
			if err := s.message(nil); err != nil {
				return err
			}
			return s.trailers(codes.OK)
		}, withStatus(recordedAs(&models.TestCase{}, nil), codes.OK), false, true},
		{"trailers alone after three keepalives", 3, func(s *h2Stream) error { return s.trailersOnly(codes.NotFound) },
			withStatus(recordedAs(&models.TestCase{}, nil), codes.NotFound), true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app := startH2App(t, func(s *h2Stream) {
				if tc.headersFirst {
					if s.headers(false, ":status", "200", "content-type", "application/grpc") != nil {
						return
					}
				}
				for i := 0; i < tc.keepalives; i++ {
					if !s.keepalive() {
						return
					}
				}
				_ = tc.answer(s)
			})
			call := grpcTestCase(app.addr, []models.GrpcLengthPrefixedMessage{{}})
			call.GrpcResp = tc.recorded.GrpcResp
			resp, err := heldOpenCall(t, call, app.exhausted)
			if tc.stalled {
				if !errors.Is(err, ErrStreamStalled) {
					t.Fatalf("SimulateGRPC: %v; want ErrStreamStalled", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("SimulateGRPC: %v; want the app's answer, judged", err)
			}
			if got, want := grpcStatusOf(resp), call.GrpcResp.Trailers.OrdinaryHeaders["grpc-status"]; got != want {
				t.Fatalf("grpc-status %q, want the app's %s", got, want)
			}
		})
	}
}

// What decides is the order the app sent its frames in, not which the
// replay takes first: an app that sends its keepalive and then ends the
// stream held it open past its recording, however soon the end follows, and
// what came after the keepalive is not its answer.
func TestAnEndAfterTheKeepaliveIsNotWaitedFor(t *testing.T) {
	app := startH2App(t, func(s *h2Stream) {
		if s.headers(false, ":status", "200", "content-type", "application/grpc") != nil || s.message(pbMsg("one")) != nil {
			return
		}
		// The first PING is answered before the rest goes, so the replay
		// has the message ahead of it, or nearly always, by the second.
		if !s.keepalive() || s.pingNow() != nil || s.message(pbMsg("two")) != nil {
			return
		}
		_ = s.trailers(codes.OK)
	})
	for i := 0; i < 20; i++ {
		resp, err := heldOpenCall(t, recordedAs(grpcTestCase(app.addr, []models.GrpcLengthPrefixedMessage{{}}), pbMsg("one")), app.exhausted)
		if !errors.Is(err, ErrStreamHeldOpen) {
			t.Fatalf("call %d: SimulateGRPC: %v; want ErrStreamHeldOpen", i, err)
		}
		if got := len(resp.AllMessages()); got != 1 {
			t.Fatalf("call %d: returned %d messages, want the one before the keepalive", i, got)
		}
	}
}

// A PING after the request's end is not a keepalive while it may be the
// app's last flow-control ping: an app that pings once after its last message
// and then ends the stream is judged on its answer.
func TestOnePingAfterTheRequestIsNoKeepaliveYet(t *testing.T) {
	app := startH2App(t, func(s *h2Stream) {
		if s.headers(false, ":status", "200", "content-type", "application/grpc") != nil || s.message(pbMsg("one")) != nil {
			return
		}
		if s.keepalive() {
			_ = s.trailers(codes.OK)
		}
	})
	resp, err := heldOpenCall(t, recordedAs(grpcTestCase(app.addr, []models.GrpcLengthPrefixedMessage{{}}), pbMsg("one")), app.exhausted)
	if err != nil {
		t.Fatalf("SimulateGRPC: %v", err)
	}
	if got := grpcStatusOf(resp); got != "0" {
		t.Fatalf("grpc-status %q, want the app's 0", got)
	}
}

// keepaliveCounter counts the PINGs (not acknowledgements) a real gRPC server
// writes on the connections it accepts.
type keepaliveCounter struct {
	net.Listener
	pings     atomic.Int32
	limit     int32
	exhausted chan struct{}
	once      sync.Once
}

func (l *keepaliveCounter) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &pingCountingConn{Conn: c, l: l}, nil
}

type pingCountingConn struct {
	net.Conn
	l    *keepaliveCounter
	walk frameWalk
}

func (c *pingCountingConn) Write(b []byte) (int, error) {
	for p := b; len(p) > 0; {
		var header bool
		header, _, p = c.walk.next(p)
		if header && c.walk.typ == http2.FramePing && !c.walk.flags.Has(http2.FlagPingAck) {
			if c.l.pings.Add(1) >= c.l.limit {
				c.l.once.Do(func() { close(c.l.exhausted) })
			}
		}
	}
	return c.Conn.Write(b)
}

// startKeepaliveServer is a real gRPC server with keepalives on (grpc-go sends
// one after a second without activity, its least), answering every call with
// handle. Its flow-control pings are on, as by default.
func startKeepaliveServer(t *testing.T, limit int32, handle grpc.StreamHandler) *keepaliveCounter {
	return startGRPCServer(t, limit, time.Second, handle)
}

// startGRPCServer is startKeepaliveServer with keepalives every keepaliveTime
// of inactivity; grpc-go's default (two hours) when it is zero.
func startGRPCServer(t *testing.T, limit int32, keepaliveTime time.Duration, handle grpc.StreamHandler) *keepaliveCounter {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	counter := &keepaliveCounter{Listener: ln, limit: limit, exhausted: make(chan struct{})}
	opts := []grpc.ServerOption{grpc.ForceServerCodec(rawSrvCodec{}), grpc.UnknownServiceHandler(handle)}
	if keepaliveTime > 0 {
		opts = append(opts, grpc.KeepaliveParams(keepalive.ServerParameters{Time: keepaliveTime, Timeout: time.Minute}))
	}
	g := grpc.NewServer(opts...)
	go func() { _ = g.Serve(counter) }()
	t.Cleanup(g.Stop)
	return counter
}

// The same with a real gRPC server and its own keepalives: a stream it holds
// open past its recording ends on them, not judged. One it ends is judged on
// its answer, its flow-control pings notwithstanding: an answer of trailers
// alone (an error status) among them, which a recording keeps as one empty
// message, or as none.
func TestARealGRPCServersKeepalivesEndAStreamHeldOpen(t *testing.T) {
	drain := func(stream grpc.ServerStream) {
		for {
			var m rawMessage
			if stream.RecvMsg(&m) != nil {
				return
			}
		}
	}
	t.Run("held open", func(t *testing.T) {
		srv := startKeepaliveServer(t, 6, func(_ interface{}, stream grpc.ServerStream) error {
			drain(stream)
			if err := stream.SendMsg(&rawMessage{data: pbMsg("one")}); err != nil {
				return err
			}
			<-stream.Context().Done()
			return nil
		})
		resp, err := heldOpenCall(t, recordedAs(grpcTestCase(srv.Addr().String(), []models.GrpcLengthPrefixedMessage{{}}), pbMsg("one")), srv.exhausted)
		if !errors.Is(err, ErrStreamHeldOpen) {
			t.Fatalf("SimulateGRPC: %v; want ErrStreamHeldOpen", err)
		}
		if got := len(resp.AllMessages()); got != 1 {
			t.Fatalf("returned %d messages, want the one that came", got)
		}
	})
	for _, tc := range []struct {
		name     string
		answer   func(stream grpc.ServerStream) error
		recorded *models.TestCase
		status   string
	}{
		{"an answer", func(stream grpc.ServerStream) error {
			return stream.SendMsg(&rawMessage{data: pbMsg("one")})
		}, recordedAs(&models.TestCase{}, pbMsg("one")), "0"},
		{"trailers alone, recorded as one empty message", func(grpc.ServerStream) error {
			return status.Error(codes.NotFound, "no such thing")
		}, withStatus(recordedAs(&models.TestCase{}, nil), codes.NotFound), "5"},
		{"trailers alone, recorded as none", func(grpc.ServerStream) error {
			return status.Error(codes.NotFound, "no such thing")
		}, &models.TestCase{GrpcResp: models.GrpcResp{NoMessages: true}}, "5"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The app's flow-control pings, and no keepalive: a judged
			// answer does not race the app's keepalive clock on a loaded
			// machine.
			srv := startGRPCServer(t, 6, 0, func(_ interface{}, stream grpc.ServerStream) error {
				drain(stream)
				return tc.answer(stream)
			})
			call := grpcTestCase(srv.Addr().String(), []models.GrpcLengthPrefixedMessage{{}})
			call.GrpcResp = tc.recorded.GrpcResp
			resp, err := heldOpenCall(t, call, srv.exhausted)
			if err != nil {
				t.Fatalf("SimulateGRPC: %v; want the app's answer, judged", err)
			}
			if got := grpcStatusOf(resp); got != tc.status {
				t.Fatalf("grpc-status %q, want the app's %s", got, tc.status)
			}
		})
	}
}

// grpcMessage is a gRPC message on the wire: its 5-byte prefix, then b.
func grpcMessage(b []byte) []byte {
	out := make([]byte, 5+len(b))
	binary.BigEndian.PutUint32(out[1:5], uint32(len(b)))
	copy(out[5:], b)
	return out
}

// frames is what fr writes.
func framesOf(t *testing.T, write func(fr *http2.Framer) error) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := write(http2.NewFramer(&b, nil)); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// The decision pingWatch makes, frame by frame, as the app sent them: a PING
// of the app's that is no acknowledgement, the second after the request's
// end, once every recorded message has come whole and the app has not ended
// the stream, holds it open; nothing else does. Each sequence is also fed a
// byte at a time, as reads and writes may split it anywhere.
func TestPingWatchHoldsAStreamOpenOnTheSecondPingAfterTheRequestEnds(t *testing.T) {
	ping := framesOf(t, func(fr *http2.Framer) error { return fr.WritePing(false, [8]byte{1}) })
	ack := framesOf(t, func(fr *http2.Framer) error { return fr.WritePing(true, [8]byte{1}) })
	settings := framesOf(t, func(fr *http2.Framer) error { return fr.WriteSettings() })
	headers := framesOf(t, func(fr *http2.Framer) error {
		return fr.WriteHeaders(http2.HeadersFrameParam{StreamID: 1, BlockFragment: []byte{0x88}, EndHeaders: true})
	})
	trailers := framesOf(t, func(fr *http2.Framer) error {
		return fr.WriteHeaders(http2.HeadersFrameParam{StreamID: 1, BlockFragment: []byte{0x88}, EndHeaders: true, EndStream: true})
	})
	reset := framesOf(t, func(fr *http2.Framer) error { return fr.WriteRSTStream(1, http2.ErrCodeCancel) })
	data := func(b []byte) []byte {
		return framesOf(t, func(fr *http2.Framer) error { return fr.WriteData(1, false, b) })
	}
	padded := func(b []byte) []byte {
		return framesOf(t, func(fr *http2.Framer) error { return fr.WriteDataPadded(1, false, b, make([]byte, 7)) })
	}
	one, two := grpcMessage(pbMsg("one")), grpcMessage(pbMsg("two"))
	both := append(append([]byte(nil), one...), two...)

	request := append([]byte(http2.ClientPreface), settings...)
	request = append(request, headers...)
	request = append(request, framesOf(t, func(fr *http2.Framer) error { return fr.WriteData(1, false, grpcMessage(nil)) })...)
	requestEnd := framesOf(t, func(fr *http2.Framer) error { return fr.WriteData(1, true, nil) })
	headersEnd := append(append([]byte(http2.ClientPreface), settings...), framesOf(t, func(fr *http2.Framer) error {
		return fr.WriteHeaders(http2.HeadersFrameParam{StreamID: 1, BlockFragment: []byte{0x88}, EndHeaders: true, EndStream: true})
	})...)

	type step struct {
		write bool // the client's, else the app's
		b     []byte
		held  bool // held open once this step is taken
		drain int  // else, the drain has had this many messages
	}
	w := func(b []byte) step { return step{write: true, b: b} }
	r := func(b []byte, held bool) step { return step{b: b, held: held} }
	d := func(n int) step { return step{drain: n} }
	for _, tc := range []struct {
		name     string
		complete int
		steps    []step
		heldAt   int
		// cutByKeepalive: the drain had every message when the keepalive
		// came.
		cutByKeepalive bool
		// stalled: held short of the recording.
		stalled bool
	}{
		{"a message, then two pings", 1, []step{w(request), w(requestEnd), r(settings, false), r(headers, false), r(data(one), false), r(ping, false), r(ping, true)}, 1, false, false},
		{"a ping before the message counts as the first", 1, []step{w(request), w(requestEnd), r(settings, false), r(ping, false), r(headers, false), r(data(one), false), r(ping, true)}, 1, false, false},
		{"a ping before the request's end does not count", 1, []step{w(request), r(settings, false), r(ping, false), w(requestEnd), r(headers, false), r(data(one), false), r(ping, false), r(ping, true)}, 1, false, false},
		{"a request that ends with its headers", 1, []step{w(headersEnd), r(settings, false), r(headers, false), r(data(one), false), r(ping, false), r(ping, true)}, 1, false, false},
		{"acknowledgements are no pings", 1, []step{w(request), w(requestEnd), r(headers, false), r(data(one), false), r(ack, false), r(ack, false), r(ack, false)}, 1, false, false},
		{"pings before every recorded message came", 2, []step{w(request), w(requestEnd), r(headers, false), r(data(one), false), r(ping, false), r(ping, false), r(data(two), false), r(ping, true)}, 2, false, false},
		{"a message not yet whole", 2, []step{w(request), w(requestEnd), r(headers, false), r(data(both[:len(both)-1]), false), r(ping, false), r(ping, false), r(data(both[len(both)-1:]), false), r(ping, true)}, 2, false, false},
		{"padded frames, a message across them", 2, []step{w(request), w(requestEnd), r(headers, false), r(padded(both[:9]), false), r(padded(both[9:]), false), r(ping, false), r(ping, true)}, 2, false, false},
		{"empty messages", 2, []step{w(request), w(requestEnd), r(headers, false), r(data(append(grpcMessage(nil), grpcMessage(nil)...)), false), r(ping, false), r(ping, true)}, 2, false, false},
		{"none recorded", 0, []step{w(request), w(requestEnd), r(headers, false), r(ping, false), r(ping, true)}, 0, true, false},
		{"the app ended the stream", 1, []step{w(request), w(requestEnd), r(headers, false), r(data(one), false), r(trailers, false), r(ping, false), r(ping, false)}, 1, false, false},
		{"the app reset the stream", 1, []step{w(request), w(requestEnd), r(headers, false), r(data(one), false), r(reset, false), r(ping, false), r(ping, false)}, 1, false, false},
		{"a message beyond the recording", 1, []step{w(request), w(requestEnd), r(headers, false), r(data(both), false), r(ping, false), r(ping, true)}, 2, false, false},
		{"stalled short of the recording", 2, []step{w(request), w(requestEnd), r(headers, false), r(data(one), false), r(ping, false), r(ping, false), r(ping, true)}, 1, false, true},
		{"a message between two keepalives is no stall", 3, []step{w(request), w(requestEnd), r(headers, false), r(data(one), false), r(ping, false), r(ping, false), r(data(two), false), r(ping, false), r(ping, true)}, 2, false, true},
		{"none recorded and no headers yet", 0, []step{w(request), w(requestEnd), r(ping, false), r(ping, false), r(ping, true)}, 0, true, true},
		{"none recorded, then the end", 0, []step{w(request), w(requestEnd), r(ping, false), r(ping, false), r(trailers, false), r(ping, false), r(ping, false)}, 0, false, false},
		{"what the app sends after the keepalive", 1, []step{w(request), w(requestEnd), r(headers, false), r(data(one), false), r(ping, false), r(ping, true), r(data(two), true), r(ping, true)}, 1, false, false},
		{"the drain had every message first", 1, []step{w(request), w(requestEnd), r(headers, false), r(data(one), false), d(1), r(ping, false), r(ping, true)}, 1, true, false},
	} {
		for _, split := range []bool{false, true} {
			name := tc.name
			if split {
				name += ", a byte at a time"
			}
			t.Run(name, func(t *testing.T) {
				var cuts atomic.Int32
				cut := make(chan struct{}, 1)
				pw := newPingWatch(nil)
				pw.arm(tc.complete, func() { cuts.Add(1); cut <- struct{}{} })
				for i, st := range tc.steps {
					if st.drain > 0 {
						if pw.gotMessage(st.drain) {
							t.Fatalf("step %d: the drain is done before the app held the stream open", i)
						}
						continue
					}
					chunks := [][]byte{st.b}
					if split {
						chunks = nil
						for j := range st.b {
							chunks = append(chunks, st.b[j:j+1])
						}
					}
					for _, c := range chunks {
						if st.write {
							pw.scanWrite(c)
						} else {
							pw.scanRead(c)
						}
					}
					if got, _, _ := pw.heldOpen(); got != st.held {
						t.Fatalf("step %d: held open %v, want %v", i, got, st.held)
					}
				}
				held, at, stalled := pw.heldOpen()
				if !held {
					return
				}
				if at != tc.heldAt {
					t.Fatalf("held open after %d messages, want %d: the ones that came before the keepalive", at, tc.heldAt)
				}
				if stalled != tc.stalled {
					t.Fatalf("stalled %v, want %v", stalled, tc.stalled)
				}
				if tc.cutByKeepalive {
					// The drain had them all: the keepalive cuts the
					// stream it waits on.
					<-cut
					if n := cuts.Load(); n != 1 {
						t.Fatalf("cut %d times, want once", n)
					}
					return
				}
				// The drain had none of them yet: it is done once it has
				// them all.
				for n := 1; n <= at; n++ {
					if done := pw.gotMessage(n); done != (n == at) {
						t.Fatalf("the drain is done after %d of %d messages: %v", n, at, done)
					}
				}
			})
		}
	}
}
