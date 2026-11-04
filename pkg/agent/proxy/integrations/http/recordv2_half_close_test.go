//go:build linux

package http

import (
	"errors"
	"io"
	"net"
	"syscall"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/supervisor"
	"go.keploy.io/server/v3/pkg/models"
)

// Through the relay, as the proxy records on Linux: an app that half-closes
// (shutdown(SHUT_WR): it sends its request, then its FIN, as Node's
// socket.end(data) or Python's sock.shutdown(SHUT_WR) do) ends its side before
// the upstream's end, and is judged by that end, whatever it does after. Its
// FIN is the last thing its socket says: a later close() of a socket whose FIN
// is out sends nothing, and the hooks keep no record of that release (they
// record only the release of a socket that had not shut down). So an app that
// read on to the upstream's end and one that took part of the answer and then
// gave up, exited or was stopped look the same to keploy whenever the upstream
// sends nothing after that close. Both are recorded as far as they read
// (stopped_reading), which replay serves without its end and holds: the app
// that gave up sees what it saw, and the app that read on waits on the held
// answer until its own timeout, so its test fails rather than passing on an
// end it may never have had. Only bytes the upstream sends after the app's
// close tell the two apart (its closed socket resets on them).

// halfClosedAppTakes runs the start of one call through c: the app sends a
// request and shuts its sending side down; once the relay has read its FIN,
// the upstream sends sent, and the app reads it. It returns a channel closed
// once the FIN is marked on the session, where appFirst (orderEndsMarked).
func halfClosedAppTakes(t *testing.T, c *relayedConn, appFirst bool, sent string) <-chan struct{} {
	t.Helper()
	appRead, appMarked := orderEndsMarked(c, appFirst)
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
	return appMarked
}

// upstreamEndsAfter ends the upstream's side, a reset where reset is set, else
// its close.
func upstreamEndsAfter(t *testing.T, c *relayedConn, reset bool) {
	t.Helper()
	if reset {
		resetConn(t, c.dest)
	} else if err := c.dest.Close(); err != nil {
		t.Fatal(err)
	}
}

// wantHeldOnce checks a half-closed app's call recorded the same mock in both
// read orders: the one call, as far as the app read it.
func wantHeldOnce(t *testing.T, mocks []*models.Mock, shapeBody string, sent sentAs) {
	t.Helper()
	if len(mocks) != 1 {
		t.Fatalf("recorded %d mocks, want the one call, as far as the half-closed app read it", len(mocks))
	}
	wantStoppedReading(t, mocks[0], shapeBody, sent)
}

func TestRecordV2_ThroughTheRelayAHalfClosedAppThatReadToTheUpstreamsEndIsHeld(t *testing.T) {
	for _, shape := range partAnswers {
		for _, up := range upstreamEnds {
			for _, proven := range []string{"", "1"} {
				for _, order := range ownEndOrders {
					t.Run(shape.name+"/"+up.name+"/"+order.name+"/KEPLOY_PROVEN="+proven, func(t *testing.T) {
						t.Setenv("KEPLOY_PROVEN", proven)
						c := newRelayedConn(t)
						halfClosedAppTakes(t, c, order.appFirst, shape.read)
						upstreamEndsAfter(t, c, up.reset)
						_ = c.app.SetReadDeadline(time.Now().Add(10 * time.Second))
						n, err := c.app.Read(make([]byte, 1))
						if n != 0 || !up.reset && !errors.Is(err, io.EOF) || up.reset && !errors.Is(err, syscall.ECONNRESET) {
							t.Fatalf("the half-closed app read %d byte(s), %v, after the answer; want the upstream's end (io.EOF for a close, ECONNRESET for a reset)", n, err)
						}
						mocks := c.end(t)
						if c.sess.EndedWhereClientStopped() {
							t.Fatal("the relay said the app stopped taking the stream, though it read all the upstream sent")
						}
						wantHeldOnce(t, mocks, shape.body, shape.sent)
					})
				}
			}
		}
	}
}

// A half-closed app that took part of the answer and then closed (it gave up,
// or its process exited), the upstream then sending nothing more before it
// closes or resets, is judged by its own end, the FIN it sent first, in both
// orders the relay can read the two ends in. Its process's exit after that
// FIN reads as the FIN: the hooks keep no record of the release of a socket
// that had shut down (they write one only for a socket still established),
// so the proxy's peerEndMarker finds none at the FIN and marks an end that
// may be a fault, as for an app that gave up.
func TestRecordV2_ThroughTheRelayAHalfClosedAppThatTookPartThenClosedIsHeld(t *testing.T) {
	for _, shape := range partAnswers {
		for _, up := range upstreamEnds {
			for _, end := range []struct {
				name   string
				client supervisor.NoFaultEnd
			}{
				{"it gave up", supervisor.EndMayBeFault},
				{"its process exited", supervisor.EndMayBeFault},
			} {
				for _, proven := range []string{"", "1"} {
					t.Run(shape.name+"/"+up.name+"/"+end.name+"/KEPLOY_PROVEN="+proven, func(t *testing.T) {
						t.Setenv("KEPLOY_PROVEN", proven)
						for _, order := range ownEndOrders {
							c := newRelayedConn(t)
							halfClosedAppTakes(t, c, order.appFirst, shape.read)
							c.clientEnd.Store(uint32(end.client))
							if err := c.app.Close(); err != nil {
								t.Fatal(err)
							}
							upstreamEndsAfter(t, c, up.reset)
							mocks := c.end(t)
							if !c.sess.ClientEndedBeforeDest() {
								t.Fatalf("%s: the relay's look as it read the upstream's end did not find the app's FIN in", order.name)
							}
							wantHeldOnce(t, mocks, shape.body, shape.sent)
						}
					})
				}
			}
		}
	}
}

// A half-closed app the recording's stop closed once it had taken part of the
// answer, the upstream then closing, resetting, or the relay ending as the
// stop cancels it, is judged by its own end too: its FIN was read before the
// stop began, so it is not the stop's (EndAtStop), and the call is recorded
// as far as the app read it. The app's FIN is marked before the stop begins
// (the row waits for the mark): a half-closed app's FIN is read as it sends
// it, long before the stop.
func TestRecordV2_ThroughTheRelayAHalfClosedAppTheRecordingsStopClosedIsHeld(t *testing.T) {
	for _, shape := range partAnswers {
		for _, how := range []string{"the upstream closes", "the upstream resets", "the relay is stopped"} {
			for _, proven := range []string{"", "1"} {
				t.Run(shape.name+"/"+how+"/KEPLOY_PROVEN="+proven, func(t *testing.T) {
					t.Setenv("KEPLOY_PROVEN", proven)
					c := newRelayedConn(t)
					appMarked := halfClosedAppTakes(t, c, true, shape.read)
					select {
					case <-appMarked:
					case <-time.After(10 * time.Second):
						t.Fatal("the relay did not mark the app's FIN within 10s")
					}
					c.stopBegun.Store(true)
					if err := c.app.Close(); err != nil {
						t.Fatal(err)
					}
					switch how {
					case "the upstream closes":
						upstreamEndsAfter(t, c, false)
					case "the upstream resets":
						upstreamEndsAfter(t, c, true)
					default:
						c.stopRelay()
						select {
						case <-c.relayDone:
						case <-time.After(10 * time.Second):
							t.Fatal("the relay did not end within 10s of its stop")
						}
					}
					mocks := c.end(t)
					if got := c.sess.ClientEndNoFault(); got != supervisor.EndMayBeFault {
						t.Fatalf("the app's end was marked %d, want %d: its FIN was read before the stop began", got, supervisor.EndMayBeFault)
					}
					wantHeldOnce(t, mocks, shape.body, shape.sent)
				})
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
				wantHeldOnce(t, mocks, shape.body, shape.sent)
			})
		}
	}
}
