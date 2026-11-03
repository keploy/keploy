//go:build linux

package http

import (
	"net"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/fakeconn"
	"go.keploy.io/server/v3/pkg/agent/proxy/supervisor"

	"go.keploy.io/server/v3/pkg/models"
	yamlLib "gopkg.in/yaml.v3"
)

// Through the relay, as the proxy records: the app reads part of an answer and
// closes. What the upstream does after that close is not something the app
// saw, so it must not shape the mock: an upstream that stays silent, one that
// sends the rest after the app's FIN, one whose rest was already at the
// proxy's socket, unread, when the FIN came, and one that resets the
// connection after the FIN all record the same mock, the answer as far as the
// app read it with a stopped_reading fault. Under a busy host the order of the
// app's FIN and the upstream's last bytes is the scheduler's choice; the
// recording must not be.
func TestRecordV2_ThroughTheRelayTheMockOfAnAnswerTheAppStoppedReadingIsTheSameWhateverTheUpstreamDoesAfter(t *testing.T) {
	for _, shape := range []struct {
		name string
		// read is what the app reads before it closes; rest is the rest of
		// the answer the upstream has to send; body is the body read.
		read, rest, body string
		// sent is how the upstream framed the body.
		sent sentAs
	}{
		{name: "a length", read: "HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nContent-Length: 20\r\n\r\n01234",
			rest: "56789abcdefghij", body: "01234", sent: sentAs{length: 20}},
		{name: "chunks", read: "HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nTransfer-Encoding: chunked\r\n\r\n14\r\n01234",
			rest: "56789abcdefghij\r\n0\r\n\r\n", body: "01234", sent: sentChunked},
		{name: "the upstream's close", read: "HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nConnection: close\r\n\r\n01234",
			rest: "56789abcdefghij", body: "01234", sent: sentByClose},
	} {
		for _, proven := range []string{"", "1"} {
			t.Run(shape.name+"/KEPLOY_PROVEN="+proven, func(t *testing.T) {
				t.Setenv("KEPLOY_PROVEN", proven)
				var first string
				for _, order := range []string{"silent", "the rest after the FIN", "the rest before the FIN", "a reset after the FIN"} {
					c := newRelayedConn(t)
					var held *readsHeldAfter
					if order == "the rest before the FIN" {
						held = &readsHeldAfter{n: len(shape.read), release: make(chan struct{})}
						c.wrapDst = func(u net.Conn) net.Conn { held.Conn = u; return held }
					}
					c.start(t)
					if held != nil {
						t.Cleanup(held.let)
					}
					write(t, c.app, c.src, jdkRequest(7))
					relayed(t, c.dest, jdkRequest(7))
					if _, err := c.dest.Write([]byte(shape.read)); err != nil {
						t.Fatal(err)
					}
					relayed(t, c.app, shape.read)
					if held != nil {
						// At the proxy's socket, unread, until the FIN is in.
						write(t, c.dest, c.dst, shape.rest)
					}
					if err := c.app.Close(); err != nil {
						t.Fatal(err)
					}
					awaitFIN(t, c.dest)
					switch order {
					case "the rest before the FIN":
						held.let()
					case "the rest after the FIN":
						if _, err := c.dest.Write([]byte(shape.rest)); err != nil {
							t.Fatal(err)
						}
					case "a reset after the FIN":
						resetConn(t, c.dest)
					}
					mocks := c.end(t)
					if len(mocks) != 1 {
						t.Fatalf("%s: recorded %d mocks, want the one exchange, as far as the app read it", order, len(mocks))
					}
					wantStoppedReading(t, mocks[0], shape.body, shape.sent)
					got := mockShape(t, mocks[0])
					if first == "" {
						first = got
						continue
					}
					if got != first {
						t.Fatalf("%s: recorded\n%s\nwant the mock recorded when the upstream was silent:\n%s", order, got, first)
					}
				}
			})
		}
	}
}

// The recording's stop closed the app partway through an answer (keploy
// stops the application once it has told the agent: Session.EndFollowedStop),
// and the upstream sent the rest after the app's FIN, which the relay refused
// for it (Session.EndedWhereClientStopped). The app did not stop reading of
// its own: no fault is its, and what it read is no whole answer. The exchange
// is left out, and said so with the stop's reason (LeftOutPartwayAtStop: the
// client's end came first and records no fault, partAnswerLeftOut), with no
// mock, and recordV2 returns nil.
func TestRecordV2_ThroughTheRelayAnAnswerTheRecordingsStopCutIsLeftOut(t *testing.T) {
	const read = "HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nContent-Length: 20\r\n\r\n01234"
	for _, proven := range []string{"", "1"} {
		t.Run("KEPLOY_PROVEN="+proven, func(t *testing.T) {
			t.Setenv("KEPLOY_PROVEN", proven)
			c := newRelayedConn(t)
			c.start(t)
			write(t, c.app, c.src, jdkRequest(7))
			relayed(t, c.dest, jdkRequest(7))
			if _, err := c.dest.Write([]byte(read)); err != nil {
				t.Fatal(err)
			}
			relayed(t, c.app, read)
			c.stopBegun.Store(true)
			if err := c.app.Close(); err != nil {
				t.Fatal(err)
			}
			awaitFIN(t, c.dest)
			if _, err := c.dest.Write([]byte("56789abcdefghij")); err != nil {
				t.Fatal(err)
			}
			awaitStopped := time.Now().Add(10 * time.Second)
			for !c.sess.EndedWhereClientStopped() && time.Now().Before(awaitStopped) {
				time.Sleep(time.Millisecond)
			}
			if !c.sess.EndedWhereClientStopped() {
				t.Fatal("the relay did not end the stream where the client stopped taking it")
			}
			if mocks := c.end(t); len(mocks) != 0 {
				t.Fatalf("recorded %d mocks, want none: the recording's stop, not the app, cut the answer (fault %v)", len(mocks), mocks[0].Spec.Fault)
			}
			if got := c.leftOut(supervisor.LeftOutPartwayAtStop); len(got) != 1 {
				t.Fatalf("said %d times that the exchange was left out under %q, want once", len(got), supervisor.LeftOutPartwayAtStop)
			}
		})
	}
}

// partAnswerFault: the relay's word that the stream ended where the client
// stopped taking it (Session.EndedWhereClientStopped) makes the fault the
// app's stop, whichever end the session has marked first, or none yet: the
// upstream's close or reset is marked before the client's where it came on
// its own before the relay read the client's end (a server with keep-alives
// off closes once it has sent its last chunk). An upstream's end in answer to
// the client's close comes only after the relay forwarded that close, which
// it marks first.
func TestPartAnswerFault_TheClientsStopWhicheverEndIsMarkedFirst(t *testing.T) {
	for _, tc := range []struct {
		name  string
		marks func(s *supervisor.Session)
	}{
		{"no end marked", func(*supervisor.Session) {}},
		{"the upstream's close marked first", func(s *supervisor.Session) {
			s.MarkPeerEnded(fakeconn.FromDest, false)
			s.MarkPeerEnded(fakeconn.FromClient, false)
		}},
		{"the upstream's reset marked first", func(s *supervisor.Session) {
			s.MarkPeerEnded(fakeconn.FromDest, true)
			s.MarkPeerEnded(fakeconn.FromClient, true)
		}},
		{"the client's end marked first", func(s *supervisor.Session) {
			s.MarkPeerEnded(fakeconn.FromClient, false)
			s.MarkPeerEnded(fakeconn.FromDest, true)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &supervisor.Session{}
			tc.marks(s)
			s.MarkEndedWhereClientStopped()
			if f := partAnswerFault(s); f == nil || f.Kind != models.FaultStoppedReading || !f.AfterHead {
				t.Fatalf("partAnswerFault = %+v, want the app's stop (stopped_reading after the head)", f)
			}
		})
	}
}

// partAnswerFault: an end read once the recording's stop had begun
// (Session.EndFollowedStop) records no fault, though the relay said the
// stream ended where the client stopped taking it: the stop, not the app, cut
// the answer. recordV2 leaves a client's first end at the stop out before it
// asks (partAnswerLeftOut), so through recordV2 only the upstream's end marked
// first reaches this (one that came on its own before the relay read the
// app's end, as a server with keep-alives off closes after its last chunk);
// asked directly, either order records none.
func TestPartAnswerFault_TheRecordingsStopRecordsNoFaultWhicheverEndIsMarkedFirst(t *testing.T) {
	for _, tc := range []struct {
		name  string
		first fakeconn.Direction
	}{
		{"the upstream's close marked first", fakeconn.FromDest},
		{"the client's end marked first", fakeconn.FromClient},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &supervisor.Session{}
			s.MarkPeerEndedBy(tc.first, false, supervisor.EndAtStop)
			s.MarkPeerEndedBy(1-tc.first, false, supervisor.EndAtStop)
			s.MarkEndedWhereClientStopped()
			if f := partAnswerFault(s); f != nil {
				t.Fatalf("partAnswerFault = %+v, want none: the recording's stop, not the app, cut the answer", f)
			}
		})
	}
}

// mockShape is m's spec as the mock file writes it, its times left out: what
// replay serves from.
func mockShape(t *testing.T, m *models.Mock) string {
	t.Helper()
	req, res := *m.Spec.HTTPReq, *m.Spec.HTTPResp
	req.Timestamp, res.Timestamp = time.Time{}, time.Time{}
	b, err := yamlLib.Marshal(models.HTTPSchema{Metadata: m.Spec.Metadata, Request: req, Response: res, Fault: m.Spec.Fault})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
