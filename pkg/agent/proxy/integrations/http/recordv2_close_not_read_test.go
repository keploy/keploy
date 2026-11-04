//go:build linux

package http

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
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

// wantLeftOutUnread checks that the exchange, whose response's end was not
// read, was left out for want, with its span recorded (spanRecorded: the test
// cases over it are left out too), nothing recorded, nothing logged at ERROR,
// and recordV2 returning nil.
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
		t.Errorf("%s: recorded a mock with body %q and fault %v; want none: the response's end was not read",
			name, body, m.Spec.Fault)
	}
	if got := leftOutReasons(logs); len(got) != 1 || got[0] != want {
		t.Errorf("%s: left out for %q; want once for %q", name, got, want)
	}
	for _, e := range logs.All() {
		if e.Message != supervisor.LeftOutWarnMsg && e.Message != supervisor.LeftOutDebugMsg {
			continue
		}
		if spanned, _ := e.ContextMap()["spanRecorded"].(bool); !spanned {
			t.Errorf("%s: left out with spanRecorded %v; want its exchange's span recorded", name, e.ContextMap()["spanRecorded"])
		}
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

// An observe-only capture that ended the upstream's stream at a hole has not
// read the body's end, even when it marks its connection's end too: what the
// hole lost may be the body's tail.
func TestRecordV2_ObserveOnly_ACloseDelimitedBodyEndedAtAHoleIsLeftOut(t *testing.T) {
	mocks, logs, err := recordUnit(t, reportReq, reportHead+firstHalf, func(s *supervisor.Session) {
		s.Opts.SkipTLSMITM = true
		s.MarkEndedAtHole(fakeconn.FromDest, "test hole")
		s.MarkEndedWithConnection(fakeconn.FromDest)
	})
	wantLeftOutUnread(t, "a hole, then the connection's end", logs, mocks, err, supervisor.LeftOutCloseNotRead)
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

// recordUnit records one exchange of req and resp on a session whose streams
// end with no side's end marked, after prep sets the session up, and returns
// what it recorded, what was left out and what recordV2 returned.
func recordUnit(t *testing.T, req, resp string, prep func(*supervisor.Session)) ([]*models.Mock, *observer.ObservedLogs, error) {
	t.Helper()
	supervisor.ResetLeftOutWarningsForTest()
	core, logs := observer.New(zapcore.DebugLevel)
	sess, sendReq, closeReq, sendResp, closeResp, mocks := newTestSession(t)
	sess.Logger = zap.New(core)
	at := time.Unix(1_700_007_000, 0)
	sendReq([]byte(req), at, at)
	sendResp([]byte(resp), at, at)
	if prep != nil {
		prep(sess)
	}
	closeReq()
	closeResp()
	err := (&HTTP{Logger: zap.New(core)}).recordV2(t.Context(), sess)
	return drainMocks(mocks), logs, err
}

// wantRecordedWhole checks that one mock was recorded, of status and body,
// with nothing left out, nothing logged at ERROR, and recordV2 returning nil.
func wantRecordedWhole(t *testing.T, name string, logs *observer.ObservedLogs, mocks []*models.Mock, err error, status int, body string) {
	t.Helper()
	if err != nil || len(mocks) != 1 || mocks[0].Spec.HTTPResp == nil ||
		mocks[0].Spec.HTTPResp.StatusCode != status || mocks[0].Spec.HTTPResp.Body != body || mocks[0].Spec.Fault != nil {
		t.Errorf("%s: recorded %d mocks (%+v), recordV2 = %v; want one of status %d, body %q, no fault", name, len(mocks), mocks, err, status, body)
	}
	if got := leftOutReasons(logs); len(got) != 0 {
		t.Errorf("%s: left out for %q; want nothing left out", name, got)
	}
	for _, e := range logs.FilterLevelExact(zapcore.ErrorLevel).All() {
		t.Errorf("%s: logged at ERROR: %s", name, e.Message)
	}
}

const (
	upgradeReq  = "GET /ws HTTP/1.1\r\nHost: ws.example\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Version: 13\r\n\r\n"
	upgradeHead = "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: s3pPLMBiTxaQ9kYGzzhZRbK+xOo=\r\n\r\n"
	wsFrame     = "\x81\x05hello"
)

// An HTTP/2 cleartext upgrade asked on a POST (RFC 7540 3.2: an Upgrade may
// come on any method), and the server's first HTTP/2 frame after its 101.
const (
	h2cUpgradeReq  = "POST /rpc HTTP/1.1\r\nHost: h2.example\r\nContent-Length: 4\r\nConnection: Upgrade, HTTP2-Settings\r\nUpgrade: h2c\r\nHTTP2-Settings: AAMAAABkAAQAAP__\r\n\r\nping"
	h2cUpgradeHead = "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: h2c\r\n\r\n"
	h2Settings     = "\x00\x00\x00\x04\x00\x00\x00\x00\x00"
)

// A 101 Switching Protocols ends at its head (RFC 9112 6.3), whatever method
// the Upgrade came on, and what follows it is another protocol, not its body:
// its mock does not depend on how the upgraded connection ends. Each way the
// stream ends short of the upstream's close records it, as a whole
// close-delimited body is not.
func TestRecordV2_ASwitchingProtocolsHeadIsWholeHoweverTheUpgradedStreamEnds(t *testing.T) {
	for upgrade, ex := range map[string]struct{ req, resp string }{
		"a WebSocket upgrade on a GET": {upgradeReq, upgradeHead + wsFrame},
		"an h2c upgrade on a POST":     {h2cUpgradeReq, h2cUpgradeHead + h2Settings},
	} {
		for name, prep := range map[string]func(*supervisor.Session){
			"no side's end":           nil,
			"a capture hole":          func(s *supervisor.Session) { s.MarkEndedAtHole(fakeconn.FromDest, "test hole") },
			"observe-only, cut":       func(s *supervisor.Session) { s.Opts.SkipTLSMITM = true },
			"the upstream's reset":    func(s *supervisor.Session) { s.MarkPeerEndedBy(fakeconn.FromDest, true, supervisor.EndAtStop) },
			"observe-only, conn ends": func(s *supervisor.Session) { s.Opts.SkipTLSMITM = true; s.MarkEndedWithConnection(fakeconn.FromDest) },
		} {
			mocks, logs, err := recordUnit(t, ex.req, ex.resp, prep)
			wantRecordedWhole(t, upgrade+", "+name, logs, mocks, err, 101, "")
		}
	}
}

// The same through the relay on real sockets: the recording stops while the
// upgraded connection is open, and the relay ends the streams with neither
// side's end read.
func TestRecordV2_ASwitchingProtocolsHeadIsWholeAtTheRelaysStop(t *testing.T) {
	supervisor.ResetLeftOutWarningsForTest()
	c := newRelayedConn(t)
	c.start(t)
	write(t, c.app, c.src, upgradeReq)
	relayed(t, c.dest, upgradeReq)
	write(t, c.dest, c.dst, upgradeHead+wsFrame)
	relayed(t, c.app, upgradeHead+wsFrame)
	c.stopBegun.Store(true)
	c.stopRelay()
	<-c.relayDone
	mocks, err := c.finish(t)
	wantRecordedWhole(t, "the relay's stop", c.logs, mocks, err, 101, "")
}

// A close-delimited body the recording's stop cut through the relay, with the
// connection open: the relay ends the stream with no side's end, which is not
// the upstream's close.
func TestRecordV2_ACloseDelimitedBodyAtTheRelaysStopIsLeftOut(t *testing.T) {
	supervisor.ResetLeftOutWarningsForTest()
	c := newRelayedConn(t)
	c.start(t)
	write(t, c.app, c.src, reportReq)
	relayed(t, c.dest, reportReq)
	write(t, c.dest, c.dst, reportHead+firstHalf)
	relayed(t, c.app, reportHead+firstHalf)
	awaitParserWaits(t, c.sess.DestStream, len(reportHead+firstHalf))
	c.stopBegun.Store(true)
	c.stopRelay()
	<-c.relayDone
	mocks, err := c.finish(t)
	if p := c.sess.PeerEnded(fakeconn.FromDest); p != supervisor.PeerOpen {
		t.Fatalf("the upstream's end is %v; want none: the relay stopped with the connection open", p)
	}
	wantLeftOutUnread(t, "the relay's stop", c.logs, mocks, err, supervisor.LeftOutCloseNotRead)
}

// The upstream's close is asked of the upstream's side: its reset once the
// stop had begun, then the client's FIN, ends no close-delimited body.
func TestRecordV2_ACloseDelimitedBodyEndsOnlyAtTheUpstreamsOwnClose(t *testing.T) {
	mocks, logs, err := recordUnit(t, reportReq, reportHead+firstHalf, func(s *supervisor.Session) {
		s.MarkPeerEndedBy(fakeconn.FromDest, true, supervisor.EndAtStop)
		s.MarkPeerEnded(fakeconn.FromClient, false)
	})
	wantLeftOutUnread(t, "reset, then the client's FIN", logs, mocks, err, supervisor.LeftOutCloseNotRead)
}

// The answer to a HEAD has no body whatever its head says (RFC 9112 6.3), so
// one with neither a Content-Length nor a chunked Transfer-Encoding is whole at
// its head, whatever its line endings, however its stream ends.
func TestRecordV2_TheAnswerToAHEADWithNoFramingIsWholeAtItsHead(t *testing.T) {
	for name, resp := range map[string]string{
		"CRLF lines":    "HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nConnection: close\r\n\r\n",
		"bare-LF lines": "HTTP/1.1 200 OK\nContent-Type: text/plain\nConnection: close\n\n",
	} {
		mocks, logs, err := recordUnit(t, "HEAD /report HTTP/1.1\r\nHost: reports.example\r\n\r\n", resp, nil)
		wantRecordedWhole(t, name, logs, mocks, err, 200, "")
	}
}

const connectReq = "CONNECT api.example:443 HTTP/1.1\r\nHost: api.example:443\r\n\r\n"

// A CONNECT's 2xx whose head's lines end in a bare LF, which readResponseV2
// finds no end of and reads to the stream's end, tunnel bytes and all, is
// framed as buildHTTPMock parses it: at its head, with no body and no
// Content-Length, however the stream ends; not as a body that ends at a close
// never read.
func TestRecordV2_ABareLFConnectHeadIsWholeAtItsHead(t *testing.T) {
	mocks, logs, err := recordUnit(t, connectReq, "HTTP/1.1 200 Connection Established\nProxy-Agent: corp-proxy\n\n\x16\x03\x01tunnel", nil)
	wantRecordedWhole(t, "bare-LF CONNECT head", logs, mocks, err, 200, "")
	if len(mocks) == 1 && mocks[0].Spec.HTTPResp != nil {
		if cl, ok := mocks[0].Spec.HTTPResp.Header["Content-Length"]; ok {
			t.Errorf("the tunnel's head was recorded with Content-Length %q, which it was not sent and must not carry", cl)
		}
	}
	wantTunnelWarned(t, "bare-LF CONNECT head", logs, "api.example:443")
}

// A CONNECT's 2xx, any 2xx, opens a tunnel after its head (RFC 9110 9.3.6): it
// has no body (RFC 9112 6.3), and what follows is the tunnel's bytes, no
// HTTP/1 exchange. Its mock is its head, recorded as the head comes, and the
// connection's recording ends there, while the tunnel carries on, which is said
// once at WARN, naming the tunnel's host (Session.ReportTunnel). A CONNECT
// refused (its 407) is an answer like any other, with its body, and the client
// may ask again on the connection. The 202's connection then ends with no
// side's end: its head is whole however its stream ends, and so is not a body
// cut short of a close.
func TestRecordV2_AConnectsTunnelIsNoBodyAndNoExchange(t *testing.T) {
	for _, tc := range []struct {
		name         string
		refusedFirst bool
		status       int
		head         string
		ended        bool
	}{
		{name: "tunnel", status: 200, head: "HTTP/1.1 200 Connection Established\r\n\r\n"},
		{name: "refused, then a tunnel", refusedFirst: true, status: 200, head: "HTTP/1.1 200 Connection Established\r\n\r\n"},
		{name: "a tunnel opened by a 2xx other than 200", status: 202, head: "HTTP/1.1 202 Accepted\r\n\r\n", ended: true},
	} {
		name, refusedFirst := tc.name, tc.refusedFirst
		supervisor.ResetLeftOutWarningsForTest()
		core, logs := observer.New(zapcore.DebugLevel)
		sess, sendReq, closeReq, sendResp, closeResp, mocks := newTestSession(t)
		t.Cleanup(func() { closeReq(); closeResp() })
		sess.Logger = zap.New(core)
		at := time.Unix(1_700_007_300, 0)
		if refusedFirst {
			sendReq([]byte(connectReq), at, at)
			sendResp([]byte("HTTP/1.1 407 Proxy Authentication Required\r\nContent-Length: 4\r\n\r\nauth"), at, at)
		}
		sendReq([]byte(connectReq), at, at)
		// The tunnel's first bytes come with the head; inside it, a plain
		// HTTP/1 call, which is the tunnel's, not this connection's.
		sendResp([]byte(tc.head+"\x16\x03\x01tunnel"), at, at)
		sendReq([]byte("GET /inside HTTP/1.1\r\nHost: api.example\r\n\r\n"), at, at)
		sendResp([]byte("HTTP/1.1 200 OK\r\nContent-Length: 6\r\n\r\ninside"), at, at)
		if tc.ended {
			closeReq()
			closeResp()
		}
		done := make(chan error, 1)
		go func() { done <- (&HTTP{Logger: zap.New(core)}).recordV2(t.Context(), sess) }()
		var err error
		select {
		case err = <-done:
		case <-time.After(10 * time.Second):
			t.Fatalf("%s: recordV2 did not return within 10s of the tunnel's head with the connection open", name)
		}
		got := drainMocks(mocks)
		want := []int{tc.status}
		if refusedFirst {
			want = []int{407, tc.status}
		}
		if err != nil || len(got) != len(want) {
			t.Fatalf("%s: recorded %d mocks, recordV2 = %v; want %d and nil", name, len(got), err, len(want))
		}
		for i, m := range got {
			if m.Spec.HTTPResp == nil || m.Spec.HTTPResp.StatusCode != want[i] {
				t.Fatalf("%s: mock %d is %+v; want status %d", name, i, m.Spec.HTTPResp, want[i])
			}
		}
		if refusedFirst && got[0].Spec.HTTPResp.Body != "auth" {
			t.Errorf("%s: the 407's body is %q; want %q", name, got[0].Spec.HTTPResp.Body, "auth")
		}
		tunnel := got[len(got)-1].Spec.HTTPResp
		if tunnel.Body != "" {
			t.Errorf("%s: the tunnel's head was recorded with body %q; want none: those are the tunnel's bytes", name, tunnel.Body)
		}
		if cl, ok := tunnel.Header["Content-Length"]; ok {
			t.Errorf("%s: the tunnel's head was recorded with Content-Length %q, which it was not sent and must not carry", name, cl)
		}
		if r := leftOutReasons(logs); len(r) != 0 {
			t.Errorf("%s: left out for %q; want nothing left out", name, r)
		}
		for _, e := range logs.FilterLevelExact(zapcore.ErrorLevel).All() {
			t.Errorf("%s: logged at ERROR: %s", name, e.Message)
		}
		wantTunnelWarned(t, name, logs, "api.example:443")
	}
}

// wantTunnelWarned checks that the connection said once, at WARN, that its
// recording ended at a tunnel to host (Session.ReportTunnel).
func wantTunnelWarned(t *testing.T, name string, logs *observer.ObservedLogs, host string) {
	t.Helper()
	var hosts []any
	for _, e := range logs.FilterMessage(supervisor.TunnelWarnMsg).All() {
		if e.Level != zapcore.WarnLevel {
			t.Errorf("%s: the tunnel was said at %v; want WARN", name, e.Level)
		}
		hosts = append(hosts, e.ContextMap()["host"])
	}
	if len(hosts) != 1 || hosts[0] != host {
		t.Errorf("%s: the tunnel was said for hosts %v; want once, for %q", name, hosts, host)
	}
}

// The parser's context ends, then the supervisor's abort closes its streams
// under it, with no side's end and the recording not stopping, partway
// through a response framed by its Content-Length, its chunks or its head:
// what was read is not the response, and the cut is keploy's own, not a decode
// error. The exchange is left out, and said so (LeftOutReadAborted), with
// nothing logged at ERROR. What was read whole by then is recorded: a 101's
// head, and a bare-LF head's response, which readResponseV2 reads to the
// stream's end, once whole.
func TestRecordV2_AResponseReadTheAbortClosedIsLeftOutUnlessWhole(t *testing.T) {
	for _, tc := range []struct {
		name, req, resp string
		marked          bool
		whole           int // the status recorded, 0 for none
		body            string
	}{
		{name: "Content-Length body", req: reportReq, resp: "HTTP/1.1 200 OK\r\nContent-Length: 40\r\n\r\n" + firstHalf},
		{name: "chunked, between chunks", req: reportReq, resp: "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n5\r\nhello\r\n"},
		{name: "chunked, in a chunk", req: reportReq, resp: "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n20\r\nhello"},
		{name: "head", req: reportReq, resp: "HTTP/1.1 200 OK\r\nContent-Le"},
		{name: "head, marked", req: reportReq, resp: "HTTP/1.1 200 OK\r\nContent-Le", marked: true},
		{name: "101", req: upgradeReq, resp: upgradeHead + wsFrame, whole: 101},
		{name: "bare-LF head, whole", req: reportReq, resp: "HTTP/1.1 200 OK\nContent-Length: 5\n\nhello", whole: 200, body: "hello"},
	} {
		supervisor.ResetLeftOutWarningsForTest()
		core, logs := observer.New(zapcore.DebugLevel)
		sess, sendReq, _, sendResp, _, mocks := newTestSession(t)
		sess.Logger = zap.New(core)
		at := time.Unix(1_700_007_400, 0)
		sendReq([]byte(tc.req), at, at)
		sendResp([]byte(tc.resp), at, at)
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() { done <- (&HTTP{Logger: zap.New(core)}).recordV2(ctx, sess) }()
		awaitParserWaits(t, sess.DestStream, len(tc.resp))
		if tc.marked {
			sess.MarkMockIncomplete("test mark")
		}
		cancel()
		_ = sess.DestStream.Close()
		_ = sess.ClientStream.Close()
		var err error
		select {
		case err = <-done:
		case <-time.After(10 * time.Second):
			t.Fatalf("%s: recordV2 did not return within 10s of its streams' close", tc.name)
		}
		if tc.whole != 0 {
			// Recorded, the parser goes on to the next request and returns
			// at its context's end.
			if errors.Is(err, context.Canceled) {
				err = nil
			}
			wantRecordedWhole(t, tc.name, logs, drainMocks(mocks), err, tc.whole, tc.body)
			continue
		}
		want := supervisor.LeftOutReadAborted
		if tc.marked {
			want = "test mark: " + want
		}
		wantLeftOutUnread(t, tc.name, logs, drainMocks(mocks), err, want)
	}
}

// A response read to its stream's end, not to a close of keploy's own, that
// does not parse is the upstream's malformed answer: a decode error, said at
// ERROR, as before.
func TestRecordV2_AMalformedResponseReadToItsStreamsEndIsADecodeError(t *testing.T) {
	mocks, logs, err := recordUnit(t, reportReq, "HTTP/1.1 200 OK\nContent-Type text/plain\n\nbody", nil)
	if err == nil || len(mocks) != 0 {
		t.Errorf("recorded %d mocks, recordV2 = %v; want none and the decode error", len(mocks), err)
	}
	if got := logs.FilterLevelExact(zapcore.ErrorLevel).FilterMessage("V2 HTTP record: failed to build mock").Len(); got != 1 {
		t.Errorf("logged the decode error at ERROR %d times; want once", got)
	}
	for _, r := range leftOutReasons(logs) {
		if r == supervisor.LeftOutReadAborted {
			t.Errorf("left out for %q; want the decode error: keploy did not close the read", r)
		}
	}
}
