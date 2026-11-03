//go:build linux

package http

import (
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/fakeconn"
	"go.keploy.io/server/v3/pkg/agent/proxy/supervisor"
	"go.keploy.io/server/v3/pkg/models"
)

// Through the relay, as the proxy records on Linux: the app closes partway
// through a call, and the upstream, with keep-alives off, sends the rest after
// the app's FIN and closes once it has (a close of its own, not an answer to
// the app's). The relay refuses the rest for the app
// (Session.EndedWhereClientStopped). Its two forwarders read the app's FIN
// and the upstream's close in the scheduler's order: on a busy host the
// upstream's close can be read first, though the app's FIN came first. The
// call is the app's to judge, by its own end (Session.ClientEndNoFault),
// either order: an app whose process exited on its own, or that the
// recording's stop closed, has its exchange left out with that end's reason;
// one that gave up (its own close, which may be a fault) has the answer
// recorded as far as it read it (stopped_reading), or, with no answer read, a
// timeout. The two orders record the same.

// ownEndOrders are the two orders the relay can read the ends in.
var ownEndOrders = []struct {
	name     string
	appFirst bool
}{
	{"the app's end read first", true},
	{"the upstream's close read first", false},
}

// ownEnds are the app's ends: how its process ended its side, as the hooks
// judge it on Linux, or the recording's stop.
var ownEnds = []struct {
	name   string
	client supervisor.NoFaultEnd
	atStop bool
	// leftOut is the reason the exchange is left out for; "" where it is
	// recorded.
	leftOut string
}{
	{name: "its process exited", client: supervisor.EndAtExit, leftOut: supervisor.LeftOutPartwayAtExit},
	{name: "the recording's stop", atStop: true, leftOut: supervisor.LeftOutPartwayAtStop},
	{name: "it gave up", client: supervisor.EndMayBeFault},
}

// orderEnds makes the relay mark c's two ends in the order asked (endHook),
// and returns a channel closed once the relay has read the app's end, its
// FIN, so the upstream's bytes after it come after the FIN. A hook that waits
// for the other end longer than 10s marks its own, and the order the test
// asserts afterwards fails.
func orderEnds(c *relayedConn, appFirst bool) <-chan struct{} {
	appRead, appMarked, destMarked := make(chan struct{}), make(chan struct{}), make(chan struct{})
	await := func(ch <-chan struct{}) {
		select {
		case <-ch:
		case <-time.After(10 * time.Second):
		}
	}
	c.endHook = func(dir fakeconn.Direction, mark func()) {
		switch {
		case dir == fakeconn.FromClient && appFirst:
			close(appRead)
			mark()
			close(appMarked)
		case dir == fakeconn.FromClient:
			close(appRead)
			await(destMarked)
			mark()
		case appFirst:
			await(appMarked)
			mark()
		default:
			mark()
			close(destMarked)
		}
	}
	return appRead
}

// endsAfterTheAppsFIN runs one call through c: the app sends a request, reads
// read (its first bytes of the answer; none for ""), and closes; once the
// relay has read its FIN, the upstream sends rest and closes. It returns what
// recordV2 recorded, after checking the relay refused the rest for the app and
// marked the ends in the order asked.
func endsAfterTheAppsFIN(t *testing.T, c *relayedConn, appFirst, atStop bool, read, rest string) []*models.Mock {
	t.Helper()
	appRead := orderEnds(c, appFirst)
	c.start(t)
	write(t, c.app, c.src, jdkRequest(7))
	relayed(t, c.dest, jdkRequest(7))
	if read != "" {
		if _, err := c.dest.Write([]byte(read)); err != nil {
			t.Fatal(err)
		}
		relayed(t, c.app, read)
	}
	c.stopBegun.Store(atStop)
	if err := c.app.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-appRead:
	case <-time.After(10 * time.Second):
		t.Fatal("the relay did not read the app's FIN within 10s")
	}
	if _, err := c.dest.Write([]byte(rest)); err != nil {
		t.Fatal(err)
	}
	if err := c.dest.Close(); err != nil {
		t.Fatal(err)
	}
	mocks := c.end(t)
	if !c.sess.EndedWhereClientStopped() {
		t.Fatal("the relay did not end the stream where the app stopped taking it")
	}
	want := fakeconn.FromDest
	if appFirst {
		want = fakeconn.FromClient
	}
	if first, ok := c.sess.FirstPeerEnded(); !ok || first != want {
		t.Fatalf("the relay marked side %d's end first (%v), want side %d's", first, ok, want)
	}
	return mocks
}

func TestRecordV2_ThroughTheRelayTheAppsOwnEndJudgesAnAnswerItStoppedTakingWhicheverEndIsReadFirst(t *testing.T) {
	for _, shape := range []struct {
		name, read, rest, body string
		sent                   sentAs
	}{
		{name: "a length", read: "HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nContent-Length: 20\r\n\r\n01234",
			rest: "56789abcdefghij", body: "01234", sent: sentAs{length: 20}},
		{name: "chunks", read: "HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nTransfer-Encoding: chunked\r\n\r\n14\r\n01234",
			rest: "56789abcdefghij\r\n0\r\n\r\n", body: "01234", sent: sentChunked},
		{name: "the upstream's close", read: "HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nConnection: close\r\n\r\n01234",
			rest: "56789abcdefghij", body: "01234", sent: sentByClose},
	} {
		for _, end := range ownEnds {
			for _, proven := range []string{"", "1"} {
				t.Run(shape.name+"/"+end.name+"/KEPLOY_PROVEN="+proven, func(t *testing.T) {
					t.Setenv("KEPLOY_PROVEN", proven)
					var first string
					for _, order := range ownEndOrders {
						c := newRelayedConn(t)
						c.clientEnd.Store(uint32(end.client))
						mocks := endsAfterTheAppsFIN(t, c, order.appFirst, end.atStop, shape.read, shape.rest)
						if end.leftOut != "" {
							if len(mocks) != 0 {
								t.Fatalf("%s: recorded %d mocks (fault %+v), want none: the app's own end records no fault, and what it read is no whole answer",
									order.name, len(mocks), mocks[0].Spec.Fault)
							}
							if got := c.leftOut(end.leftOut); len(got) != 1 {
								t.Fatalf("%s: said %d times that the exchange was left out under %q, want once", order.name, len(got), end.leftOut)
							}
							continue
						}
						if len(mocks) != 1 {
							t.Fatalf("%s: recorded %d mocks, want the one exchange, as far as the app read it", order.name, len(mocks))
						}
						wantStoppedReading(t, mocks[0], shape.body, shape.sent)
						got := mockShape(t, mocks[0])
						if first == "" {
							first = got
							continue
						}
						if got != first {
							t.Fatalf("%s: recorded\n%s\nwant the mock recorded with the app's end read first:\n%s", order.name, got, first)
						}
					}
				})
			}
		}
	}
}

// The same with no byte of the answer read: the app closed before the
// upstream answered, and the upstream sent its whole answer after the FIN,
// which the relay refused for the app, and closed. An app that gave up has
// the call recorded as a timeout, either order, never as the upstream's close
// with no answer; one whose process exited, or that the stop closed, records
// nothing.
func TestRecordV2_ThroughTheRelayTheAppsOwnEndJudgesACallItClosedUnansweredWhicheverEndIsReadFirst(t *testing.T) {
	for _, end := range ownEnds {
		for _, proven := range []string{"", "1"} {
			t.Run(end.name+"/KEPLOY_PROVEN="+proven, func(t *testing.T) {
				t.Setenv("KEPLOY_PROVEN", proven)
				for _, order := range ownEndOrders {
					c := newRelayedConn(t)
					c.clientEnd.Store(uint32(end.client))
					mocks := endsAfterTheAppsFIN(t, c, order.appFirst, end.atStop, "",
						"HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nContent-Length: 5\r\n\r\nhello")
					if end.leftOut != "" {
						if len(mocks) != 0 {
							t.Fatalf("%s: recorded %d mocks (fault %+v), want none: the app's own end records no fault",
								order.name, len(mocks), mocks[0].Spec.Fault)
						}
						continue
					}
					if len(mocks) != 1 {
						t.Fatalf("%s: recorded %d mocks, want the call the app gave up on", order.name, len(mocks))
					}
					wantFault(t, mocks[0], models.Fault{Kind: models.FaultTimeout})
				}
			})
		}
	}
}
