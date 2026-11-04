//go:build linux

package http

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"sync"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// At replay a half-closed app half-closes again, as it did at record: its
// request, then its FIN, in before the serve writes the answer. That FIN is
// not its close: the hold reads the end of the stream and holds on, until a
// reset or the run's end, so the app sees what it read at record and then
// nothing until its own timeout. A FIN after the answer is the app's close,
// and lets the connection go, as before.

// idleMarks is a request holder (integrations.WithRequestHolder) that keeps
// the idle marks a serve takes (integrations.MarkIdle), as the proxy's kept
// connection does: marked closes once one is taken, released once it is
// let go.
type idleMarks struct {
	marked, released chan struct{}
	mark, release    sync.Once
}

func newIdleMarks() *idleMarks {
	return &idleMarks{marked: make(chan struct{}), released: make(chan struct{})}
}

func (m *idleMarks) Hold() func() { return func() {} }

func (m *idleMarks) MarkIdle() func() {
	m.mark.Do(func() { close(m.marked) })
	return func() { m.release.Do(func() { close(m.released) }) }
}

// heldReplay is a serve of one mock to an app on a real socket.
type heldReplay struct {
	app   net.Conn
	marks *idleMarks
	done  <-chan error
	stop  context.CancelFunc
}

// replayTo serves m to an app that sends request and, where halfClose, then
// its FIN: the proxy reads the request, and the FIN once it is in (a read
// returns the end of the stream), before the serve begins, as the proxy's
// socket holds it by the time a matched answer is written. proxyOf wraps the
// proxy's end where it is not nil. The proxy closes the app's connection once
// decodeHTTP returns.
func replayTo(t *testing.T, m *models.Mock, request string, halfClose bool, proxyOf func(net.Conn) net.Conn) heldReplay {
	t.Helper()
	app, proxy := tcpPair(t)
	t.Cleanup(func() { _ = app.Close() })
	if _, err := io.WriteString(app, request); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(proxy, make([]byte, len(request))); err != nil {
		t.Fatal(err)
	}
	if halfClose {
		if err := app.(*net.TCPConn).CloseWrite(); err != nil {
			t.Fatal(err)
		}
		_ = proxy.SetReadDeadline(time.Now().Add(10 * time.Second)) // turns a hang into a failure
		if n, err := proxy.Read(make([]byte, 1)); n != 0 || !errors.Is(err, io.EOF) {
			t.Fatalf("the proxy read %d byte(s), %v, after the request; want the app's FIN", n, err)
		}
		_ = proxy.SetReadDeadline(time.Time{})
	}
	served := proxy
	if proxyOf != nil {
		served = proxyOf(proxy)
	}
	marks := newIdleMarks()
	ctx, cancel := context.WithCancel(integrations.WithRequestHolder(context.Background(), marks))
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() {
		err := (&HTTP{Logger: zap.NewNop()}).decodeHTTP(ctx, []byte(request), served, nil,
			&mockMemDb{mocks: []*models.Mock{m}, updateUnFilteredReturn: true}, models.OutgoingOptions{})
		_ = proxy.Close()
		done <- err
	}()
	return heldReplay{app: app, marks: marks, done: done, stop: cancel}
}

// held waits until the serve holds the half-closed app's connection past its
// FIN (the idle mark), failing the row where decodeHTTP returned first.
func (r heldReplay) held(t *testing.T) {
	t.Helper()
	select {
	case <-r.marks.marked:
	case err := <-r.done:
		t.Fatalf("decodeHTTP returned %v at the app's FIN, sent before the answer: the app is let go with an end it never had", err)
	case <-time.After(time.Minute): // turns a hang into a failure
		t.Fatal("the serve neither held the half-closed app's connection nor returned")
	}
}

// stillHeld fails the row where the serve has let the connection go.
func (r heldReplay) stillHeld(t *testing.T, after string) {
	t.Helper()
	select {
	case err := <-r.done:
		t.Fatalf("decodeHTTP returned %v %s; want the connection held", err, after)
	case <-r.marks.released:
		t.Fatalf("the idle mark was released %s; want the connection held", after)
	default:
	}
}

// ended waits for decodeHTTP to return, and checks it let the idle mark go.
func (r heldReplay) ended(t *testing.T, by string) {
	t.Helper()
	if err := awaitReturn(t, r.done); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("decodeHTTP = %v once %s", err, by)
	}
	select {
	case <-r.marks.released:
	default:
		if markedAt(r.marks) {
			t.Fatalf("decodeHTTP returned once %s with its idle mark still taken", by)
		}
	}
}

func markedAt(m *idleMarks) bool {
	select {
	case <-m.marked:
		return true
	default:
		return false
	}
}

// readsUntilItsTimeout is the app's own client at replay: it parses the
// answer, and reads its body until its own deadline. It checks the app got
// body, and then nothing, no byte and no end, until that deadline.
func readsUntilItsTimeout(t *testing.T, app net.Conn, body string) {
	t.Helper()
	_ = app.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	resp, err := http.ReadResponse(bufio.NewReader(app), nil)
	if err != nil {
		t.Fatalf("no answer parses: %v", err)
	}
	got, err := io.ReadAll(resp.Body)
	if string(got) != body || !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("the app read %q, then %v; want %q, then nothing until its own timeout, as at record", got, err, body)
	}
}

// halfClosedAtRecord records, through the relay, a half-closed app's call:
// it takes shape's head and part of its body, then gives up and closes, the
// upstream closing after (readOn false); or it reads on to the upstream's
// end (readOn true). Either is one stopped_reading mock.
func halfClosedAtRecord(t *testing.T, shape string, readOn bool) *models.Mock {
	t.Helper()
	c := newRelayedConn(t)
	halfClosedAppTakes(t, c, true, shape)
	if !readOn {
		if err := c.app.Close(); err != nil {
			t.Fatal(err)
		}
	}
	upstreamEndsAfter(t, c, false)
	if readOn {
		_ = c.app.SetReadDeadline(time.Now().Add(10 * time.Second))
		if n, err := c.app.Read(make([]byte, 1)); n != 0 || !errors.Is(err, io.EOF) {
			t.Fatalf("the app read %d byte(s), %v, after the answer; want the upstream's close", n, err)
		}
	}
	mocks := c.end(t)
	if len(mocks) != 1 || mocks[0].Spec.Fault == nil || mocks[0].Spec.Fault.Kind != models.FaultStoppedReading {
		t.Fatalf("recorded %d mocks (%+v), want one stopped_reading", len(mocks), mocks)
	}
	return mocks[0]
}

// Recorded, then replayed: an app that half-closed and gave up after part of
// the answer sees at replay what it saw at record, the part, then nothing
// until its own timeout. One that read on to the upstream's end sees the
// same at replay, the stated limit: its test fails by its own timeout, and
// never passes on an end it may not have had. The serve holds on past the
// app's FIN, marked idle, until a reset, here the app's (it closes with the
// rest unread), or the run's end.
func TestReplay_AHalfClosedAppsHeldAnswerIsHeldPastItsFIN(t *testing.T) {
	for _, shape := range partAnswers {
		for _, readOn := range []bool{false, true} {
			atRecord := "it gave up"
			if readOn {
				atRecord = "it read on to the upstream's end"
			}
			for _, end := range []string{"the app resets", "the run ends"} {
				for _, proven := range []string{"", "1"} {
					t.Run(shape.name+"/"+atRecord+"/"+end+"/KEPLOY_PROVEN="+proven, func(t *testing.T) {
						t.Setenv("KEPLOY_PROVEN", proven)
						m := throughDisk(t, halfClosedAtRecord(t, shape.read, readOn))
						r := replayTo(t, m, jdkRequest(7), true, nil)
						r.held(t)
						readsUntilItsTimeout(t, r.app, shape.body)
						r.stillHeld(t, "before the app's own timeout")
						if end == "the app resets" {
							resetConn(t, r.app)
						} else {
							r.stop()
						}
						r.ended(t, end)
					})
				}
			}
		}
	}
}

// A timeout (the upstream silent, before its head or after part of its body)
// served to an app that half-closed, as it did at record, holds past its FIN
// too: the app waits until its own timeout, as at record.
func TestReplay_ATimeoutServedToAHalfClosedAppIsHeldPastItsFIN(t *testing.T) {
	for _, row := range []struct {
		name  string
		fault *models.Fault
		resp  *models.HTTPResp
		want  string
	}{
		{"before the head", &models.Fault{Kind: models.FaultTimeout}, nil, ""},
		{"after part of the body", &models.Fault{Kind: models.FaultTimeout, AfterHead: true, BodyBytes: 5},
			okHead("0123456789", map[string]string{"Content-Length": "10"}),
			"HTTP/1.1 200 OK\r\nContent-Length: 10\r\n\r\n01234"},
	} {
		for _, proven := range []string{"", "1"} {
			t.Run(row.name+"/KEPLOY_PROVEN="+proven, func(t *testing.T) {
				t.Setenv("KEPLOY_PROVEN", proven)
				r := replayTo(t, throughDisk(t, faultMock(row.fault, row.resp)), faultReq, true, nil)
				r.held(t)
				if s := appGivesUp(t, r.app); s != (seen{row.want, "its own timeout"}) {
					t.Fatalf("the half-closed app saw %+v; want %q, then nothing until its own timeout", s, row.want)
				}
				r.stillHeld(t, "before the app's own timeout")
				r.stop()
				r.ended(t, "the run ends")
			})
		}
	}
}

// An app that keeps its side open while it reads, and closes once it has
// what it wanted, ends the hold with that FIN, sent after the answer: the
// connection is let go at once, as before, and never marked idle.
func TestReplay_AFINAfterTheHeldAnswerEndsTheHold(t *testing.T) {
	for _, row := range []struct {
		name, request, body string
		mock                func(t *testing.T) *models.Mock
	}{
		{"stopped_reading", jdkRequest(7), "01234", func(t *testing.T) *models.Mock {
			return halfClosedAtRecord(t, partAnswers[0].read, false)
		}},
		{"timeout", faultReq, "01234", func(*testing.T) *models.Mock {
			return faultMock(&models.Fault{Kind: models.FaultTimeout, AfterHead: true, BodyBytes: 5},
				okHead("0123456789", map[string]string{"Content-Length": "10"}))
		}},
	} {
		for _, proven := range []string{"", "1"} {
			t.Run(row.name+"/KEPLOY_PROVEN="+proven, func(t *testing.T) {
				t.Setenv("KEPLOY_PROVEN", proven)
				r := replayTo(t, row.mock(t), row.request, false, nil)
				readsUntilItsTimeout(t, r.app, row.body)
				if err := r.app.(*net.TCPConn).CloseWrite(); err != nil {
					t.Fatal(err)
				}
				if err := awaitReturn(t, r.done); err != nil {
					t.Fatalf("decodeHTTP = %v once the app closed after the answer, want nil", err)
				}
				if markedAt(r.marks) {
					t.Fatal("the serve marked the connection idle at a FIN sent after the answer; want it let go")
				}
			})
		}
	}
}

// unaskedConn is the proxy's end of a connection through a wrapper that
// gives no socket (no NetConn, no SyscallConn): the serve cannot ask whether
// the app's FIN came before the answer.
type unaskedConn struct{ net.Conn }

// Where the socket cannot be asked, a FIN cannot be told a half-close or a
// close, and the hold holds on through it, until the run's end (fail closed:
// never an end the app may not have had). A reset still ends it at once.
func TestReplay_AHoldWhoseSocketCannotBeAskedHoldsPastAFIN(t *testing.T) {
	unasked := func(c net.Conn) net.Conn { return unaskedConn{c} }
	for _, end := range []string{"the app closes", "the app resets"} {
		t.Run(end, func(t *testing.T) {
			m := faultMock(&models.Fault{Kind: models.FaultTimeout, AfterHead: true, BodyBytes: 5},
				okHead("0123456789", map[string]string{"Content-Length": "10"}))
			r := replayTo(t, m, faultReq, false, unasked)
			readsUntilItsTimeout(t, r.app, "01234")
			if end == "the app resets" {
				resetConn(t, r.app)
				if err := awaitReturn(t, r.done); err != nil {
					t.Fatalf("decodeHTTP = %v once the app reset, want nil", err)
				}
				if markedAt(r.marks) {
					t.Fatal("the serve marked the connection idle at the app's reset; want it let go")
				}
				return
			}
			if err := r.app.(*net.TCPConn).CloseWrite(); err != nil {
				t.Fatal(err)
			}
			r.held(t)
			r.stillHeld(t, "at the app's FIN")
			r.stop()
			r.ended(t, "the run ends")
		})
	}
}
