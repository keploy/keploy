//go:build linux

package http

import (
	"errors"
	"io"
	"net"
	"syscall"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// Through the relay, as the proxy records on Linux: an app that half-closes
// (shutdown(SHUT_WR): it sends its request, then its FIN, and goes on
// reading, as Node's socket.end(data) or Python's sock.shutdown(SHUT_WR) do)
// ends its side before the upstream's end, but it has not stopped reading.
// Its stack takes what the upstream sends after its FIN (the relay sets each
// such chunk aside until the app's stack answers for it, client_took.go), and
// the app reads the answer to the upstream's end. The call is the upstream's
// to judge, as for an app that kept its side open: an answer the upstream's
// close frames is whole, one it cut short of its framing is its truncate, and
// its reset is its reset. Before, the app's FIN came before the upstream's
// end, so the app's end judged the call: its whole answer was recorded as one
// it stopped reading (stopped_reading), which replay serves without its end
// and holds, and the app, reading to the end it had at record, waits on it
// until its own timeout.

// halfClosedAppReadsTheAnswer runs one call through c: the app sends a
// request and shuts its sending side down; once the relay has read its FIN,
// the upstream sends sent and ends its side (a reset where reset is set, else
// its close), and the app reads everything to that end. It returns what
// recordV2 recorded, after checking the relay gave no word that the app
// stopped taking the stream.
func halfClosedAppReadsTheAnswer(t *testing.T, c *relayedConn, appFirst bool, sent string, reset bool) []*models.Mock {
	t.Helper()
	appRead := orderEnds(c, appFirst)
	c.start(t)
	write(t, c.app, c.src, jdkRequest(7))
	relayed(t, c.dest, jdkRequest(7))
	if err := c.app.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-appRead:
	case <-time.After(10 * time.Second):
		t.Fatal("the relay did not read the app's FIN within 10s")
	}
	if _, err := c.dest.Write([]byte(sent)); err != nil {
		t.Fatal(err)
	}
	relayed(t, c.app, sent)
	if reset {
		resetConn(t, c.dest)
	} else if err := c.dest.Close(); err != nil {
		t.Fatal(err)
	}
	_ = c.app.SetReadDeadline(time.Now().Add(10 * time.Second))
	n, err := c.app.Read(make([]byte, 1))
	if n != 0 || !reset && !errors.Is(err, io.EOF) || reset && !errors.Is(err, syscall.ECONNRESET) {
		t.Fatalf("the half-closed app read %d byte(s), %v, after the answer; want the upstream's end (io.EOF for a close, ECONNRESET for a reset)", n, err)
	}
	mocks := c.end(t)
	if c.sess.EndedWhereClientStopped() {
		t.Fatal("the relay said the app stopped taking the stream, though it read all the upstream sent")
	}
	return mocks
}

func TestRecordV2_ThroughTheRelayAHalfClosedAppThatReadToTheUpstreamsEndIsJudgedByTheUpstream(t *testing.T) {
	for _, shape := range partAnswers {
		for _, up := range upstreamEnds {
			for _, proven := range []string{"", "1"} {
				for _, order := range ownEndOrders {
					t.Run(shape.name+"/"+up.name+"/"+order.name+"/KEPLOY_PROVEN="+proven, func(t *testing.T) {
						t.Setenv("KEPLOY_PROVEN", proven)
						c := newRelayedConn(t)
						mocks := halfClosedAppReadsTheAnswer(t, c, order.appFirst, shape.read, up.reset)
						if len(mocks) != 1 {
							t.Fatalf("recorded %d mocks, want the one call", len(mocks))
						}
						f := mocks[0].Spec.Fault
						switch {
						case up.reset:
							if f == nil || f.Kind != models.FaultReset {
								t.Fatalf("recorded fault %+v, want the upstream's reset", f)
							}
						case shape.sent == sentByClose:
							if f != nil || mocks[0].Spec.HTTPResp.Body != shape.body {
								t.Fatalf("recorded fault %+v and body %q, want the whole answer the upstream's close framed, %q, with no fault: the half-closed app read it to that close", f, mocks[0].Spec.HTTPResp.Body, shape.body)
							}
						default:
							if f == nil || f.Kind != models.FaultTruncate {
								t.Fatalf("recorded fault %+v, want the upstream's truncate: the half-closed app read to the upstream's close", f)
							}
						}
					})
				}
			}
		}
	}
}

// A half-closed app that read part of the answer and then closed its socket
// did stop reading: its stack resets on what the upstream sends after, the
// relay ends the stream there (Session.EndedWhereClientStopped), and the call
// is recorded as far as the app read it (stopped_reading), though the app's
// stack took a chunk after its FIN.
func TestRecordV2_ThroughTheRelayAHalfClosedAppThatClosedPartwayStoppedReading(t *testing.T) {
	for _, shape := range partAnswers {
		for _, proven := range []string{"", "1"} {
			t.Run(shape.name+"/KEPLOY_PROVEN="+proven, func(t *testing.T) {
				t.Setenv("KEPLOY_PROVEN", proven)
				c := newRelayedConn(t)
				c.start(t)
				write(t, c.app, c.src, jdkRequest(7))
				relayed(t, c.dest, jdkRequest(7))
				if err := c.app.(*net.TCPConn).CloseWrite(); err != nil {
					t.Fatal(err)
				}
				if _, err := c.dest.Write([]byte(shape.read)); err != nil {
					t.Fatal(err)
				}
				relayed(t, c.app, shape.read)
				if err := c.app.Close(); err != nil {
					t.Fatal(err)
				}
				// The rest, which the closed app's stack resets on.
				if _, err := c.dest.Write([]byte("56789")); err != nil {
					t.Fatal(err)
				}
				mocks := c.end(t)
				if !c.sess.EndedWhereClientStopped() {
					t.Fatal("the relay did not say the app stopped taking the stream, though its closed socket refused the rest")
				}
				if len(mocks) != 1 {
					t.Fatalf("recorded %d mocks, want the one call, as far as the app read it", len(mocks))
				}
				wantStoppedReading(t, mocks[0], shape.body, shape.sent)
			})
		}
	}
}
