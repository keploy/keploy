//go:build linux

package http

import (
	"errors"
	"io"
	"net"
	"sync"
	"syscall"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/fakeconn"
	"go.keploy.io/server/v3/pkg/agent/proxy/supervisor"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// Through the relay on real sockets, as the proxy records: an upstream that
// fails the call. The application must see the fault the upstream gave, and
// the call must be recorded with it, so replay can give the same fault.

const faultReq = "GET /charge HTTP/1.1\r\nHost: pay.example\r\n\r\n"

// finish closes both peers and returns what recordV2 recorded and returned.
func (c *relayedConn) finish(t *testing.T) ([]*models.Mock, error) {
	t.Helper()
	_ = c.app.Close()
	_ = c.dest.Close()
	var err error
	select {
	case err = <-c.done:
	case <-time.After(10 * time.Second):
		t.Fatal("recordV2 did not return within 10s of the connection's end")
	}
	var mocks []*models.Mock
	for {
		select {
		case m := <-c.mocks:
			mocks = append(mocks, m)
		default:
			return mocks, err
		}
	}
}

// appRead reads at the application until its connection ends, and returns
// what it read and how it ended.
func appRead(t *testing.T, app net.Conn) (string, error) {
	t.Helper()
	_ = app.SetReadDeadline(time.Now().Add(10 * time.Second))
	var got []byte
	buf := make([]byte, 4096)
	for {
		n, err := app.Read(buf)
		got = append(got, buf[:n]...)
		if err != nil {
			return string(got), err
		}
	}
}

// awaitPeerEnd waits until the relay has read dir's end of the connection
// (Session.PeerEnded), so a close of the other side that follows is the
// second end, as an application's reaction to an end the relay forwarded it
// is: the relay marks an end before it forwards it.
func awaitPeerEnd(t *testing.T, c *relayedConn, dir fakeconn.Direction) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for c.sess.PeerEnded(dir) == supervisor.PeerOpen {
		if time.Now().After(deadline) {
			t.Fatalf("the relay did not read the end of side %d within 10s", dir)
		}
		time.Sleep(time.Millisecond)
	}
}

func resetConn(t *testing.T, c net.Conn) {
	t.Helper()
	if err := c.(*net.TCPConn).SetLinger(0); err != nil {
		t.Fatal(err)
	}
	_ = c.Close()
}

func oneMock(t *testing.T, mocks []*models.Mock, err error) *models.Mock {
	t.Helper()
	if err != nil {
		t.Fatalf("recordV2 = %v, want nil: the call ended in a fault the upstream gave, which is recorded, not a decode error", err)
	}
	if len(mocks) != 1 {
		t.Fatalf("recorded %d mocks, want 1: the faulted call is recorded", len(mocks))
	}
	return mocks[0]
}

func TestRecordV2_FaultUpstreamResetBeforeAnswer(t *testing.T) {
	c := newRelayedConn(t)
	c.start(t)
	write(t, c.app, c.src, faultReq)
	relayed(t, c.dest, faultReq)
	resetConn(t, c.dest)
	// The proxy closes the application's connection once the relay has
	// ended (proxy.go, after recordViaSupervisor).
	select {
	case <-c.relayDone:
	case <-time.After(10 * time.Second):
		t.Fatal("the relay did not end within 10s of the upstream's reset")
	}
	_ = c.src.Close()
	got, rerr := appRead(t, c.app)
	if got != "" || !errors.Is(rerr, syscall.ECONNRESET) {
		t.Errorf("the application read %q, %v; want nothing, then a reset, as the upstream gave", got, rerr)
	}
	mocks, err := c.finish(t)
	m := oneMock(t, mocks, err)
	wantFault(t, m, models.Fault{Kind: models.FaultReset})
}

func TestRecordV2_FaultUpstreamClosesWithNoAnswer(t *testing.T) {
	c := newRelayedConn(t)
	c.start(t)
	write(t, c.app, c.src, faultReq)
	relayed(t, c.dest, faultReq)
	_ = c.dest.Close()
	got, rerr := appRead(t, c.app)
	if got != "" || rerr != io.EOF {
		t.Errorf("the application read %q, %v; want nothing, then EOF, as the upstream gave", got, rerr)
	}
	mocks, err := c.finish(t)
	wantFault(t, oneMock(t, mocks, err), models.Fault{Kind: models.FaultEOF})
}

func TestRecordV2_FaultUpstreamNeverAnswersAndTheAppGivesUp(t *testing.T) {
	c := newRelayedConn(t)
	c.start(t)
	write(t, c.app, c.src, faultReq)
	relayed(t, c.dest, faultReq)
	// The application's own client timeout: it closes, with no answer.
	_ = c.app.Close()
	buf := make([]byte, 1)
	_ = c.dest.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, err := c.dest.Read(buf); err != io.EOF {
		t.Fatalf("the upstream read %v; want the application's close forwarded (EOF)", err)
	}
	mocks, err := c.finish(t)
	wantFault(t, oneMock(t, mocks, err), models.Fault{Kind: models.FaultTimeout})
}

func TestRecordV2_FaultUpstreamTruncatesItsBody(t *testing.T) {
	const head = "HTTP/1.1 200 OK\r\nContent-Length: 20\r\n\r\n"
	c := newRelayedConn(t)
	c.start(t)
	write(t, c.app, c.src, faultReq)
	relayed(t, c.dest, faultReq)
	write(t, c.dest, c.dst, head+"0123456789")
	_ = c.dest.Close()
	got, rerr := appRead(t, c.app)
	if got != head+"0123456789" || rerr != io.EOF {
		t.Errorf("the application read %q, %v; want the head and 10 of 20 body bytes, then EOF", got, rerr)
	}
	mocks, err := c.finish(t)
	m := oneMock(t, mocks, err)
	if m.Spec.HTTPResp.Body != "0123456789" {
		t.Errorf("recorded body %q, want the 10 bytes sent", m.Spec.HTTPResp.Body)
	}
	wantFault(t, m, models.Fault{Kind: models.FaultTruncate, BodyBytes: 10})
	if te, ok := m.Spec.HTTPResp.Header["Transfer-Encoding"]; ok {
		t.Errorf("recorded Transfer-Encoding %q, want none: the upstream sent none", te)
	}
	if got := m.Spec.HTTPResp.Header["Content-Length"]; got != "20" {
		t.Errorf("recorded Content-Length %q, want 20, as the upstream sent it", got)
	}
}

func TestRecordV2_FaultCustomReasonPhraseIsRecorded(t *testing.T) {
	const resp = "HTTP/1.1 402 Card Declined\r\nContent-Length: 2\r\n\r\n{}"
	c := newRelayedConn(t)
	c.start(t)
	write(t, c.app, c.src, faultReq)
	relayed(t, c.dest, faultReq)
	write(t, c.dest, c.dst, resp)
	relayed(t, c.app, resp)
	mocks, err := c.finish(t)
	m := oneMock(t, mocks, err)
	if m.Spec.Fault != nil {
		t.Errorf("a whole answer is recorded with fault %v, want none", m.Spec.Fault)
	}
	if m.Spec.HTTPResp.StatusMessage != "Card Declined" {
		t.Errorf("recorded reason phrase %q, want %q", m.Spec.HTTPResp.StatusMessage, "Card Declined")
	}
	wire, berr := (&HTTP{}).encodeRecordedAnswer(m)
	if berr != nil {
		t.Fatal(berr)
	}
	if want := "HTTP/1.1 402 Card Declined\r\n"; len(wire) < len(want) || string(wire[:len(want)]) != want {
		t.Errorf("replay serves status line %q, want %q", firstLine(wire), want)
	}
}

func firstLine(b []byte) string {
	for i := range b {
		if b[i] == '\n' {
			return string(b[:i+1])
		}
	}
	return string(b)
}

// wantFault checks m was recorded with the fault want (Observed aside).
func wantFault(t *testing.T, m *models.Mock, want models.Fault) {
	t.Helper()
	if m.Spec.Fault == nil {
		t.Fatalf("recorded with no fault, want %s", &want)
	}
	got := *m.Spec.Fault
	if got.Observed == "" {
		t.Errorf("fault %s recorded with no Observed, want what the recorder saw", &got)
	}
	got.Observed = ""
	if got != want {
		t.Errorf("recorded fault %+v, want %+v", got, want)
	}
	if err := m.ValidateFault(); err != nil {
		t.Errorf("recorded fault does not validate: %v", err)
	}
}

func TestRecordV2_FaultUpstreamResetsAfterTheHead(t *testing.T) {
	const head = "HTTP/1.1 200 OK\r\nContent-Length: 20\r\n\r\n"
	c := newRelayedConn(t)
	c.start(t)
	write(t, c.app, c.src, faultReq)
	relayed(t, c.dest, faultReq)
	write(t, c.dest, c.dst, head+"01234")
	relayed(t, c.app, head+"01234")
	resetConn(t, c.dest)
	<-c.relayDone
	_ = c.src.Close()
	got, rerr := appRead(t, c.app)
	if got != "" || !errors.Is(rerr, syscall.ECONNRESET) {
		t.Errorf("the application read %q, %v after the head and 5 bytes; want a reset, as the upstream gave", got, rerr)
	}
	mocks, err := c.finish(t)
	m := oneMock(t, mocks, err)
	wantFault(t, m, models.Fault{Kind: models.FaultReset, AfterHead: true, BodyBytes: 5})
	if m.Spec.HTTPResp.StatusCode != 200 || m.Spec.HTTPResp.Body != "01234" {
		t.Errorf("recorded %d %q, want the head's 200 and the 5 bytes sent", m.Spec.HTTPResp.StatusCode, m.Spec.HTTPResp.Body)
	}
}

func TestRecordV2_FaultUpstreamResetsACloseDelimitedBody(t *testing.T) {
	const head = "HTTP/1.1 200 OK\r\nConnection: close\r\n\r\n"
	c := newRelayedConn(t)
	c.start(t)
	write(t, c.app, c.src, faultReq)
	relayed(t, c.dest, faultReq)
	write(t, c.dest, c.dst, head+"abc")
	relayed(t, c.app, head+"abc")
	resetConn(t, c.dest)
	awaitPeerEnd(t, c, fakeconn.FromDest)
	mocks, err := c.finish(t)
	wantFault(t, oneMock(t, mocks, err), models.Fault{Kind: models.FaultReset, AfterHead: true, BodyBytes: 3})
}

func TestRecordV2_FaultCloseDelimitedBodyClosedIsWhole(t *testing.T) {
	const resp = "HTTP/1.1 200 OK\r\nConnection: close\r\n\r\nabc"
	c := newRelayedConn(t)
	c.start(t)
	write(t, c.app, c.src, faultReq)
	relayed(t, c.dest, faultReq)
	write(t, c.dest, c.dst, resp)
	relayed(t, c.app, resp)
	_ = c.dest.Close()
	awaitPeerEnd(t, c, fakeconn.FromDest)
	mocks, err := c.finish(t)
	m := oneMock(t, mocks, err)
	if m.Spec.Fault != nil || m.Spec.HTTPResp.Body != "abc" {
		t.Errorf("recorded %q with fault %v; want the whole answer a close ends, no fault", m.Spec.HTTPResp.Body, m.Spec.Fault)
	}
}

func TestRecordV2_FaultUpstreamGoesSilentAfterTheHeadAndTheAppGivesUp(t *testing.T) {
	const head = "HTTP/1.1 200 OK\r\nContent-Length: 20\r\n\r\n"
	c := newRelayedConn(t)
	c.start(t)
	write(t, c.app, c.src, faultReq)
	relayed(t, c.dest, faultReq)
	write(t, c.dest, c.dst, head+"01234")
	relayed(t, c.app, head+"01234")
	_ = c.app.Close()
	buf := make([]byte, 1)
	_ = c.dest.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, err := c.dest.Read(buf); err != io.EOF {
		t.Fatalf("the upstream read %v; want the application's close forwarded (EOF)", err)
	}
	mocks, err := c.finish(t)
	wantFault(t, oneMock(t, mocks, err), models.Fault{Kind: models.FaultTimeout, AfterHead: true, BodyBytes: 5})
}

func TestRecordV2_FaultChunkedBodyTruncatedKeepsItsFraming(t *testing.T) {
	const head = "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n"
	const sent = "5\r\nabcde\r\n5\r\nfg"
	c := newRelayedConn(t)
	c.start(t)
	write(t, c.app, c.src, faultReq)
	relayed(t, c.dest, faultReq)
	write(t, c.dest, c.dst, head+sent)
	_ = c.dest.Close()
	if got, rerr := appRead(t, c.app); got != head+sent || rerr != io.EOF {
		t.Errorf("the application read %q, %v; want the head and the bytes sent, then EOF", got, rerr)
	}
	mocks, err := c.finish(t)
	m := oneMock(t, mocks, err)
	wantFault(t, m, models.Fault{Kind: models.FaultTruncate, BodyBytes: int64(len(sent))})
	if m.Spec.HTTPResp.Body != sent || m.Spec.HTTPResp.Header["Transfer-Encoding"] != "chunked" {
		t.Errorf("recorded body %q, Transfer-Encoding %q; want the bytes as sent, framing and all, and chunked",
			m.Spec.HTTPResp.Body, m.Spec.HTTPResp.Header["Transfer-Encoding"])
	}
}

// A close or a reset from the upstream before any answer on a reused
// connection is the keep-alive idle-close race: the application's client
// retries on a new connection, where the call is recorded. Nothing is
// recorded for it here.
func TestRecordV2_FaultBeforeAnswerOnAReusedConnectionIsNotRecorded(t *testing.T) {
	const resp = "HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok"
	for _, reset := range []bool{false, true} {
		c := newRelayedConn(t)
		c.start(t)
		write(t, c.app, c.src, faultReq)
		relayed(t, c.dest, faultReq)
		write(t, c.dest, c.dst, resp)
		relayed(t, c.app, resp)
		write(t, c.app, c.src, faultReq)
		relayed(t, c.dest, faultReq)
		if reset {
			resetConn(t, c.dest)
		} else {
			_ = c.dest.Close()
		}
		// The upstream's end reaches the application before it closes.
		got, rerr := appRead(t, c.app)
		if want := error(io.EOF); reset && !errors.Is(rerr, syscall.ECONNRESET) || !reset && rerr != want || got != "" {
			t.Errorf("reset %v: the application read %q, %v; want nothing, then the upstream's end", reset, got, rerr)
		}
		mocks, err := c.finish(t)
		if m := oneMock(t, mocks, err); m.Spec.Fault != nil {
			t.Errorf("reset %v: the first call recorded with fault %v, want the answer it got", reset, m.Spec.Fault)
		}
	}
}

// The application gives up on a call on a reused connection (a pooled
// client's timeout): it is recorded as a timeout, as on a first request. The
// idle-close race is the upstream's end, not the application's.
func TestRecordV2_FaultTimeoutOnAReusedConnectionIsRecorded(t *testing.T) {
	const resp = "HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok"
	c := newRelayedConn(t)
	c.start(t)
	write(t, c.app, c.src, faultReq)
	relayed(t, c.dest, faultReq)
	write(t, c.dest, c.dst, resp)
	relayed(t, c.app, resp)
	write(t, c.app, c.src, faultReq)
	relayed(t, c.dest, faultReq)
	closeUnread(t, c)
	mocks, err := c.finish(t)
	if err != nil || len(mocks) != 2 {
		t.Fatalf("recorded %d mocks, %v; want 2: the answered call, then the one the application gave up on", len(mocks), err)
	}
	if mocks[0].Spec.Fault != nil {
		t.Errorf("the first call recorded with fault %v, want the answer it got", mocks[0].Spec.Fault)
	}
	wantFault(t, mocks[1], models.Fault{Kind: models.FaultTimeout})
}

// Server bytes no request asked for before a request (its 408 on a keep-alive
// connection that idled out) mark that request only: the application's close
// after them is the idle-close race, not a timeout, and a later request on the
// connection the application gives up on is a timeout again.
func TestRecordV2_FaultNotRecordedAfterServerBytesNoRequestAskedFor(t *testing.T) {
	const resp = "HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok"
	t.Run("the request after them", func(t *testing.T) {
		c := newRelayedConn(t)
		c.start(t)
		write(t, c.app, c.src, faultReq)
		relayed(t, c.dest, faultReq)
		write(t, c.dest, c.dst, resp)
		relayed(t, c.app, resp)
		write(t, c.dest, c.dst, idleTimeout)
		relayed(t, c.app, idleTimeout)
		write(t, c.app, c.src, faultReq)
		relayed(t, c.dest, faultReq)
		closeUnread(t, c)
		mocks, err := c.finish(t)
		if m := oneMock(t, mocks, err); m.Spec.Fault != nil {
			t.Errorf("the first call recorded with fault %v, want the answer it got, and nothing for the second", m.Spec.Fault)
		}
	})
	t.Run("a later request", func(t *testing.T) {
		c := newRelayedConn(t)
		c.start(t)
		write(t, c.dest, c.dst, idleTimeout)
		relayed(t, c.app, idleTimeout)
		write(t, c.app, c.src, faultReq)
		relayed(t, c.dest, faultReq)
		write(t, c.dest, c.dst, resp)
		relayed(t, c.app, resp)
		write(t, c.app, c.src, faultReq)
		relayed(t, c.dest, faultReq)
		closeUnread(t, c)
		mocks, err := c.finish(t)
		if err != nil || len(mocks) != 2 {
			t.Fatalf("recorded %d mocks, %v; want 2: the answered call, then the one the application gave up on", len(mocks), err)
		}
		wantFault(t, mocks[1], models.Fault{Kind: models.FaultTimeout})
	})
}

// The answer in flight when the capture joined a connection partway answers a
// request the capture lacks (it is left out), not no request: the
// application's giving up on the next call is still a timeout.
func TestRecordV2_FaultTimeoutAfterTheAnswerInFlightWhenTheCaptureJoined(t *testing.T) {
	const resp = "HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok"
	c := newRelayedConn(t)
	c.startRelay(t)
	c.sess.JoinedMidConnection = true
	c.startParser(t)
	write(t, c.dest, c.dst, resp)
	relayed(t, c.app, resp)
	write(t, c.app, c.src, faultReq)
	relayed(t, c.dest, faultReq)
	closeUnread(t, c)
	mocks, err := c.finish(t)
	wantFault(t, oneMock(t, mocks, err), models.Fault{Kind: models.FaultTimeout})
	if n := c.mgr.MocksLeftOut(); n != 1 {
		t.Errorf("%d mocks left out, want 1: the answer in flight when the capture joined", n)
	}
}

// Neither side ending it: the recording stopped with the call unanswered.
func TestRecordV2_FaultNotRecordedWhenTheRecordingStops(t *testing.T) {
	c := newRelayedConn(t)
	c.start(t)
	write(t, c.app, c.src, faultReq)
	relayed(t, c.dest, faultReq)
	c.stopRelay()
	<-c.relayDone
	// The proxy's close of the client stays a FIN: no side reset.
	_ = c.src.Close()
	if got, rerr := appRead(t, c.app); got != "" || rerr != io.EOF {
		t.Errorf("the application read %q, %v; want nothing, then EOF", got, rerr)
	}
	mocks, err := c.finish(t)
	if err != nil || len(mocks) != 0 {
		t.Errorf("recorded %d mocks, %v; want none: the call was cut by the recording's stop, not by a side of it", len(mocks), err)
	}
}

// The capture lost part of the destination's stream: its end is not known
// to be the upstream's.
func TestRecordV2_FaultNotRecordedAtACaptureHole(t *testing.T) {
	c := newRelayedConn(t)
	c.start(t)
	write(t, c.app, c.src, faultReq)
	relayed(t, c.dest, faultReq)
	c.sess.MarkEndedAtHole(fakeconn.FromDest, "test hole")
	_ = c.dest.Close()
	mocks, err := c.finish(t)
	if err != nil || len(mocks) != 0 {
		t.Errorf("recorded %d mocks, %v; want none at a capture hole", len(mocks), err)
	}
}

// A capture that joined the connection partway does not know its first
// request, so a close before an answer may be the idle-close race.
func TestRecordV2_FaultNotRecordedWhenTheCaptureJoinedPartway(t *testing.T) {
	c := newRelayedConn(t)
	c.startRelay(t)
	c.sess.JoinedMidConnection = true
	c.startParser(t)
	write(t, c.app, c.src, faultReq)
	relayed(t, c.dest, faultReq)
	_ = c.dest.Close()
	awaitPeerEnd(t, c, fakeconn.FromDest)
	mocks, err := c.finish(t)
	if err != nil || len(mocks) != 0 {
		t.Errorf("recorded %d mocks, %v; want none for a capture that joined the connection partway", len(mocks), err)
	}
}

// The session closed the stream (the supervisor's abort): the call is left
// out, whatever side has ended since.
func TestRecordV2_FaultNotRecordedWhenTheSessionClosesTheStream(t *testing.T) {
	c := newRelayedConn(t)
	c.start(t)
	write(t, c.app, c.src, faultReq)
	relayed(t, c.dest, faultReq)
	c.sess.MarkPeerEnded(fakeconn.FromDest, false)
	_ = c.sess.DestStream.Close()
	mocks, err := c.finish(t)
	if err != nil || len(mocks) != 0 {
		t.Errorf("recorded %d mocks, %v; want none when the session closed the stream", len(mocks), err)
	}
}

// Partway through the answer: neither a hole nor the recording's stop is a
// fault, and a stream that ends there records nothing, as before.
func TestRecordV2_FaultAfterTheHeadNotRecordedAtAHoleOrAStop(t *testing.T) {
	const head = "HTTP/1.1 200 OK\r\nContent-Length: 20\r\n\r\n"
	for _, hole := range []bool{true, false} {
		c := newRelayedConn(t)
		c.start(t)
		write(t, c.app, c.src, faultReq)
		relayed(t, c.dest, faultReq)
		write(t, c.dest, c.dst, head+"01234")
		relayed(t, c.app, head+"01234")
		if hole {
			c.sess.MarkEndedAtHole(fakeconn.FromDest, "test hole")
			_ = c.dest.Close()
		} else {
			c.stopRelay()
		}
		<-c.relayDone
		mocks, _ := c.finish(t)
		for _, m := range mocks {
			if m.Spec.Fault != nil {
				t.Errorf("hole %v: recorded fault %v, want none", hole, m.Spec.Fault)
			}
		}
	}
}

func TestRecordV2_FaultNotedOnceAsRecorded(t *testing.T) {
	const head = "HTTP/1.1 200 OK\r\nContent-Length: 20\r\n\r\n"
	for _, partway := range []bool{false, true} {
		faultNoted = sync.Once{}
		c := newRelayedConn(t)
		c.start(t)
		write(t, c.app, c.src, faultReq)
		relayed(t, c.dest, faultReq)
		if partway {
			write(t, c.dest, c.dst, head+"01234")
			relayed(t, c.app, head+"01234")
		}
		_ = c.dest.Close()
		mocks, err := c.finish(t)
		oneMock(t, mocks, err)
		if n := c.logs.FilterMessageSnippet("keploy now records a call that ended in a fault").Len(); n != 1 {
			t.Errorf("partway %v: the note was logged %d times, want once", partway, n)
		}
	}
}

// A call answered whole is recorded with no fault, and records no note of
// faults: the note is for a recording that holds one.
func TestRecordV2_NoNoteForACallAnsweredWhole(t *testing.T) {
	const answer = "HTTP/1.1 200 OK\r\nContent-Length: 5\r\n\r\n01234"
	faultNoted = sync.Once{}
	c := newRelayedConn(t)
	c.start(t)
	write(t, c.app, c.src, faultReq)
	relayed(t, c.dest, faultReq)
	write(t, c.dest, c.dst, answer)
	relayed(t, c.app, answer)
	_ = c.dest.Close()
	mocks, err := c.finish(t)
	if m := oneMock(t, mocks, err); m.Spec.Fault != nil {
		t.Fatalf("recorded fault %v on a call answered whole, want none", m.Spec.Fault)
	}
	if n := c.logs.FilterMessageSnippet("keploy now records a call that ended in a fault").Len(); n != 0 {
		t.Errorf("the note was logged %d times for a call answered whole, want never", n)
	}
}

// A passthrough call records nothing, faulted or not.
func TestRecordV2_FaultOnAPassthroughCallIsNotRecorded(t *testing.T) {
	const head = "HTTP/1.1 200 OK\r\nContent-Length: 20\r\n\r\n"
	for _, partway := range []bool{false, true} {
		c := newRelayedConn(t)
		c.startRelay(t)
		c.sess.Opts.Rules = []models.BypassRule{{Host: "pay.example"}}
		c.startParser(t)
		write(t, c.app, c.src, faultReq)
		relayed(t, c.dest, faultReq)
		if partway {
			write(t, c.dest, c.dst, head+"01234")
			relayed(t, c.app, head+"01234")
		}
		_ = c.dest.Close()
		mocks, err := c.finish(t)
		if err != nil || len(mocks) != 0 {
			t.Errorf("partway %v: recorded %d mocks, %v; want none for a passthrough call", partway, len(mocks), err)
		}
	}
}

// A faulted call whose request does not decode is reported, as any is.
func TestRecordV2_FaultOnARequestThatDoesNotDecodeIsAnError(t *testing.T) {
	const req = "POST /charge HTTP/1.1\r\nHost: pay.example\r\nContent-Encoding: gzip\r\nContent-Length: 3\r\n\r\nabc"
	c := newRelayedConn(t)
	c.start(t)
	write(t, c.app, c.src, req)
	relayed(t, c.dest, req)
	_ = c.dest.Close()
	mocks, err := c.finish(t)
	if err == nil || len(mocks) != 0 {
		t.Errorf("recorded %d mocks, %v; want none and the decode error", len(mocks), err)
	}
}

// buildHTTPMock records a fault after the head only on an answer cut short.
func TestBuildHTTPMock_FaultOnlyOnAnUnfinishedAnswer(t *testing.T) {
	h := &HTTP{Logger: zap.NewNop()}
	whole := "HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok"
	m, err := h.buildHTTPMock(&FinalHTTP{Req: []byte(faultReq), Resp: []byte(whole),
		Fault: &models.Fault{Kind: models.FaultReset, AfterHead: true}}, 80, "c", models.OutgoingOptions{})
	if err != nil || m == nil || m.Spec.Fault != nil || m.Spec.HTTPResp.Body != "ok" {
		t.Errorf("a whole answer with a reset after it: %v, %v; want the answer, no fault", m, err)
	}
	empty := "HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n"
	m, err = h.buildHTTPMock(&FinalHTTP{Req: []byte(faultReq), Resp: []byte(empty),
		Fault: &models.Fault{Kind: models.FaultReset, AfterHead: true}}, 80, "c", models.OutgoingOptions{})
	if err != nil || m == nil || m.Spec.Fault != nil || m.Spec.HTTPResp.StatusCode != 200 {
		t.Errorf("a whole answer with no body (Content-Length: 0) and a reset after it: %v, %v; want the answer, no fault", m, err)
	}
	bareLF := "HTTP/1.1 200 OK\nContent-Length: 20\n\n0123456789"
	if m, err := h.buildHTTPMock(&FinalHTTP{Req: []byte(faultReq), Resp: []byte(bareLF),
		Fault: &models.Fault{Kind: models.FaultTruncate}}, 80, "c", models.OutgoingOptions{}); err == nil {
		t.Errorf("a cut-short answer whose head has bare LFs recorded as %+v; want an error: its body's bytes are not known", m.Spec)
	}
}

// keploy record tells the agent it is stopping before it stops the
// application (Session.MarkPeerEndedStopping): a call the application had in
// flight, closed by that stop, ended in no fault of the upstream's or of the
// application's own client, and records none. Before the answer and after its
// head alike.
func TestRecordV2_FaultNotRecordedWhenTheAppIsStoppedMidCall(t *testing.T) {
	const head = "HTTP/1.1 200 OK\r\nContent-Length: 20\r\n\r\n"
	for _, afterHead := range []bool{false, true} {
		c := newRelayedConn(t)
		c.start(t)
		write(t, c.app, c.src, faultReq)
		relayed(t, c.dest, faultReq)
		if afterHead {
			write(t, c.dest, c.dst, head+"01234")
			relayed(t, c.app, head+"01234")
		}
		c.stopBegun.Store(true)
		appClosed(t, c)
		mocks, _ := c.finish(t)
		for _, m := range mocks {
			if m.Spec.Fault != nil {
				t.Errorf("after the head %v: recorded fault %v, want none: the recording's stop closed the application, which did not give up on the call", afterHead, m.Spec.Fault)
			}
		}
	}
}

// The reverse order: the application gave up on the call (its client's
// timeout) before the recording's stop began. Its timeout is recorded,
// however soon the stop follows.
func TestRecordV2_FaultTimeoutRecordedWhenTheAppGaveUpBeforeTheStop(t *testing.T) {
	const head = "HTTP/1.1 200 OK\r\nContent-Length: 20\r\n\r\n"
	for _, afterHead := range []bool{false, true} {
		c := newRelayedConn(t)
		c.start(t)
		write(t, c.app, c.src, faultReq)
		relayed(t, c.dest, faultReq)
		want := models.Fault{Kind: models.FaultTimeout}
		if afterHead {
			write(t, c.dest, c.dst, head+"01234")
			relayed(t, c.app, head+"01234")
			want = models.Fault{Kind: models.FaultTimeout, AfterHead: true, BodyBytes: 5}
		}
		appClosed(t, c)
		awaitPeerEnd(t, c, fakeconn.FromClient)
		c.stopBegun.Store(true)
		mocks, err := c.finish(t)
		wantFault(t, oneMock(t, mocks, err), want)
	}
}

// An upstream end the relay reads once the recording's stop has begun is the
// stop's too (Session.EndFollowedStop): keploy stops the application and its
// dependencies' fixtures alike, and an upstream that closes or resets then
// gave no fault the application met. No fault is recorded, by a close or a
// reset, before the answer or after its head.
func TestRecordV2_FaultNotRecordedWhenTheUpstreamEndsAfterTheStopBegan(t *testing.T) {
	const head = "HTTP/1.1 200 OK\r\nContent-Length: 20\r\n\r\n"
	for _, tc := range []struct {
		name             string
		afterHead, reset bool
	}{
		{"closed before the answer", false, false},
		{"reset before the answer", false, true},
		{"closed after the head", true, false},
		{"reset after the head", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newRelayedConn(t)
			c.start(t)
			write(t, c.app, c.src, faultReq)
			relayed(t, c.dest, faultReq)
			if tc.afterHead {
				write(t, c.dest, c.dst, head+"01234")
				relayed(t, c.app, head+"01234")
			}
			c.stopBegun.Store(true)
			if tc.reset {
				resetConn(t, c.dest)
			} else {
				_ = c.dest.Close()
			}
			awaitPeerEnd(t, c, fakeconn.FromDest)
			mocks, _ := c.finish(t)
			for _, m := range mocks {
				if m.Spec.Fault != nil {
					t.Errorf("recorded fault %v, want none: the upstream's end came after the recording's stop began", m.Spec.Fault)
				}
			}
		})
	}
}
