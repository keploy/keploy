//go:build linux

package http

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"io"
	"math/big"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/fakeconn"
	"go.keploy.io/server/v3/pkg/agent/proxy/supervisor"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// A response whose body ends where the upstream closes the connection (no
// Content-Length, not chunked) is whole only where that close was read. Read up
// to any other end, it is not known to be whole, and no fault is known to
// record it with: it is left out, and said so (LeftOutCloseNotRead), never
// recorded as a whole response shorter than the upstream sent it, which replay
// would serve with a clean end.

const (
	reportReq  = "GET /report HTTP/1.1\r\nHost: reports.example\r\n\r\n"
	reportHead = "HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nConnection: close\r\n\r\n"
	firstHalf  = "the first half of the report, "
)

// leftOutReasons is the reason of each exchange the session reported left out
// (Session.ReportLeftOut), at WARN or, past its cause's limit, at DEBUG.
func leftOutReasons(logs *observer.ObservedLogs) []string {
	var reasons []string
	for _, e := range logs.All() {
		if e.Message == supervisor.LeftOutWarnMsg || e.Message == supervisor.LeftOutDebugMsg {
			r, _ := e.ContextMap()["reason"].(string)
			reasons = append(reasons, r)
		}
	}
	return reasons
}

// wantLeftOutUnread checks that the exchange was left out for want, with
// nothing recorded, nothing logged at ERROR, and recordV2 returning nil.
func wantLeftOutUnread(t *testing.T, name string, logs *observer.ObservedLogs, mocks []*models.Mock, err error, want string) {
	t.Helper()
	if err != nil {
		t.Errorf("%s: recordV2 = %v, want nil: nothing failed to decode", name, err)
	}
	for _, m := range mocks {
		body := ""
		if m.Spec.HTTPResp != nil {
			body = m.Spec.HTTPResp.Body
		}
		t.Errorf("%s: recorded a mock with body %q and fault %v; want none: the body ends at the upstream's close, which was not read",
			name, body, m.Spec.Fault)
	}
	if got := leftOutReasons(logs); len(got) != 1 || got[0] != want {
		t.Errorf("%s: left out for %q; want once for %q", name, got, want)
	}
	for _, e := range logs.FilterLevelExact(zapcore.ErrorLevel).All() {
		t.Errorf("%s: logged at ERROR: %s", name, e.Message)
	}
}

// awaitParserWaits waits until recordV2 has taken n bytes of s and waits for
// more, with nothing buffered for it.
func awaitParserWaits(t *testing.T, s *fakeconn.FakeConn, n int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if waiting, _ := s.Waiting(); waiting && s.Consumed() >= int64(n) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the parser did not take %d bytes and wait for more within 10s (took %d)", n, s.Consumed())
		}
		time.Sleep(time.Millisecond)
	}
}

// cutConn is an upstream's socket under its TLS layer: once cut is set, the
// next write puts only the first half of its bytes on the wire, then closes the
// socket, so the record it carries reaches the proxy only in part.
type cutConn struct {
	net.Conn
	cut atomic.Bool
}

func (c *cutConn) Write(p []byte) (int, error) {
	if !c.cut.Load() {
		return c.Conn.Write(p)
	}
	n, _ := c.Conn.Write(p[:len(p)/2])
	_ = c.Close()
	return n, io.ErrClosedPipe
}

// selfSignedUpstream is a certificate for a test's TLS upstream.
func selfSignedUpstream(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "reports.example"},
		DNSNames:     []string{"reports.example"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// The upstream's TLS stream breaks partway through a record of the body: its
// socket closes with the record half sent. The relay's read of the upstream
// fails (io.ErrUnexpectedEOF), which is neither a close nor a reset, and the
// relay ends the streams with neither side's end read. The body's end, the
// upstream's close at a record's boundary, never came. And for contrast, the
// same body whose upstream closes its socket at a record's boundary, with or
// without its close_notify: that close ends the body, which is recorded whole.
func TestRecordV2_ACloseDelimitedBodyCutByABrokenTLSStreamIsLeftOut(t *testing.T) {
	for _, end := range []string{"a record cut short", "a close at a record's boundary", "a close_notify"} {
		supervisor.ResetLeftOutWarningsForTest()
		c := newRelayedConn(t)
		c.wrapDst = func(up net.Conn) net.Conn {
			return tls.Client(up, &tls.Config{ServerName: "reports.example", InsecureSkipVerify: true}) // #nosec G402 -- a test's self-signed upstream
		}
		raw := &cutConn{Conn: c.dest}
		upstream := tls.Server(raw, &tls.Config{Certificates: []tls.Certificate{selfSignedUpstream(t)}})
		c.startRelay(t)
		if _, err := c.app.Write([]byte(reportReq)); err != nil {
			t.Fatal(err)
		}
		_ = upstream.SetDeadline(time.Now().Add(10 * time.Second))
		got := make([]byte, len(reportReq))
		if _, err := io.ReadFull(upstream, got); err != nil || string(got) != reportReq {
			t.Fatalf("%s: the upstream read %q, %v; want the request", end, got, err)
		}
		if _, err := upstream.Write([]byte(reportHead + firstHalf)); err != nil {
			t.Fatal(err)
		}
		relayed(t, c.app, reportHead+firstHalf)
		switch end {
		case "a record cut short":
			raw.cut.Store(true)
			_, _ = upstream.Write([]byte("and the second half"))
		case "a close at a record's boundary":
			_ = c.dest.Close()
		default:
			_ = upstream.Close()
		}
		if end != "a record cut short" {
			// The application closes once it has read the body to the
			// close the relay forwarded it.
			awaitPeerEnd(t, c, fakeconn.FromDest)
			_ = c.app.Close()
		}
		select {
		case <-c.relayDone:
		case <-time.After(10 * time.Second):
			t.Fatalf("%s: the relay did not end within 10s of the upstream's end", end)
		}
		// The parser reads behind its capture, once the relay has ended.
		c.startParser(t)
		mocks, err := c.finish(t)
		if end != "a record cut short" {
			m := oneMock(t, mocks, err)
			if m.Spec.Fault != nil || m.Spec.HTTPResp == nil || m.Spec.HTTPResp.Body != firstHalf {
				t.Errorf("%s: recorded body %v, fault %v; want %q whole, with no fault: the upstream's close ends it",
					end, m.Spec.HTTPResp, m.Spec.Fault, firstHalf)
			}
			continue
		}
		if p := c.sess.PeerEnded(fakeconn.FromDest); p != supervisor.PeerOpen {
			t.Fatalf("%s: the relay read the upstream's end as %v; want none: its read failed mid-record", end, p)
		}
		wantLeftOutUnread(t, end, c.logs, mocks, err, supervisor.LeftOutCloseNotRead)
	}
}

// The session closes the response's stream partway through the body (the
// supervisor's abort), with the recording not stopping: with no side's end
// read, and after the upstream's close was read, but before the parser read
// the stream's end. The read ends at its reader's close, not at the
// upstream's.
func TestRecordV2_ACloseDelimitedBodyTheSessionClosedIsLeftOut(t *testing.T) {
	for _, closedFirst := range []bool{false, true} {
		name := "no side's end"
		if closedFirst {
			name = "after the upstream's close"
		}
		supervisor.ResetLeftOutWarningsForTest()
		c := newRelayedConn(t)
		c.start(t)
		write(t, c.app, c.src, reportReq)
		relayed(t, c.dest, reportReq)
		write(t, c.dest, c.dst, reportHead+firstHalf)
		relayed(t, c.app, reportHead+firstHalf)
		awaitParserWaits(t, c.sess.DestStream, len(reportHead+firstHalf))
		if closedFirst {
			c.sess.MarkPeerEnded(fakeconn.FromDest, false)
		}
		_ = c.sess.DestStream.Close()
		mocks, err := c.finish(t)
		wantLeftOutUnread(t, name, c.logs, mocks, err, supervisor.LeftOutCloseNotRead)
	}
}

// The upstream resets the connection partway through the body once the
// recording's stop has begun. Its end records no fault (Session.EndFollowedStop),
// and a reset is not the close that ends such a body.
func TestRecordV2_ACloseDelimitedBodyResetAtTheStopIsLeftOut(t *testing.T) {
	supervisor.ResetLeftOutWarningsForTest()
	c := newRelayedConn(t)
	c.startRelay(t)
	write(t, c.app, c.src, reportReq)
	relayed(t, c.dest, reportReq)
	write(t, c.dest, c.dst, reportHead+firstHalf)
	relayed(t, c.app, reportHead+firstHalf)
	c.stopBegun.Store(true)
	resetConn(t, c.dest)
	select {
	case <-c.relayDone:
	case <-time.After(10 * time.Second):
		t.Fatal("the relay did not end within 10s of the upstream's reset")
	}
	c.startParser(t)
	mocks, err := c.finish(t)
	if p := c.sess.PeerEnded(fakeconn.FromDest); p != supervisor.PeerReset || !c.sess.EndFollowedStop() {
		t.Fatalf("the upstream's end is %v, after the stop %v; want its reset, read once the stop had begun", p, c.sess.EndFollowedStop())
	}
	wantLeftOutUnread(t, "reset at the stop", c.logs, mocks, err, supervisor.LeftOutCloseNotRead)
}

// The capture lost part of the destination's stream and ended it at the hole,
// which the upstream's close followed: the stream did not run out at that
// close. A mark of the incomplete-mock flag goes before the reason.
func TestRecordV2_ACloseDelimitedBodyEndedAtAHoleIsLeftOut(t *testing.T) {
	for _, marked := range []bool{false, true} {
		supervisor.ResetLeftOutWarningsForTest()
		core, logs := observer.New(zapcore.DebugLevel)
		sess, sendReq, closeReq, sendResp, closeResp, mocks := newTestSession(t)
		sess.Logger = zap.New(core)
		at := time.Unix(1_700_006_000, 0)
		sendReq([]byte(reportReq), at, at)
		sendResp([]byte(reportHead+firstHalf), at, at)
		sess.MarkEndedAtHole(fakeconn.FromDest, "test hole")
		sess.MarkPeerEnded(fakeconn.FromDest, false)
		want := supervisor.LeftOutCloseNotRead
		if marked {
			sess.MarkMockIncomplete("test mark")
			want = "test mark: " + want
		}
		closeReq()
		closeResp()
		err := (&HTTP{Logger: zap.New(core)}).recordV2(t.Context(), sess)
		wantLeftOutUnread(t, "hole", logs, drainMocks(mocks), err, want)
	}
}

// An observe-only capture (Opts.SkipTLSMITM) has no relay to read the
// upstream's close: it says the stream ended because its connection did
// (Session.EndedWithConnection), and then the body is recorded as the client
// read it, to that end. A stream that ended otherwise (the recording stopped
// while the connection carried on) did not reach the body's end.
func TestRecordV2_ObserveOnly_ACloseDelimitedBodyIsWholeOnlyAtTheConnectionsEnd(t *testing.T) {
	for name, resp := range map[string]string{
		"close-delimited":                       "HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\n\r\nhello until close",
		"close-delimited, bare-LF header lines": "HTTP/1.1 200 OK\r\nContent-Type: text/plain\n\nhello until close",
	} {
		for _, end := range []streamEnd{clientClosed, captureCut} {
			supervisor.ResetLeftOutWarningsForTest()
			core, logs := observer.New(zapcore.DebugLevel)
			sess, sendReq, closeReq, sendResp, closeResp, mocks := newTestSession(t)
			sess.Logger = zap.New(core)
			sess.Opts.SkipTLSMITM = true
			if end.withConn {
				sess.MarkEndedWithConnection(fakeconn.FromDest)
			}
			at := time.Unix(1_700_006_100, 0)
			sendReq(versionRequest, at, at)
			sendResp([]byte(resp), at, at)
			closeReq()
			closeResp()
			err := (&HTTP{Logger: zap.New(core)}).recordV2(t.Context(), sess)
			got := drainMocks(mocks)
			if !end.withConn {
				wantLeftOutUnread(t, name+", the capture cut", logs, got, err, supervisor.LeftOutCloseNotRead)
				continue
			}
			if err != nil || len(got) != 1 || got[0].Spec.HTTPResp == nil || got[0].Spec.HTTPResp.Body != "hello until close" {
				t.Errorf("%s, the connection's end: recorded %d mocks, %v; want the body whole", name, len(got), err)
			}
			if r := leftOutReasons(logs); len(r) != 0 {
				t.Errorf("%s, the connection's end: left out for %q; want nothing left out", name, r)
			}
		}
	}
}

// drainMocks returns the mocks on mocks, without waiting.
func drainMocks(mocks chan *models.Mock) []*models.Mock {
	var got []*models.Mock
	for {
		select {
		case m := <-mocks:
			got = append(got, m)
		default:
			return got
		}
	}
}
