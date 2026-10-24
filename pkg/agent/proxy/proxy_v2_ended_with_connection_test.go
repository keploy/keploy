package proxy

import (
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/agent/proxy/fakeconn"
)

// On the proxy path, a direction whose peer closed it ends with the session
// saying so (Session.EndedWithConnection), when the relay teed every byte of
// it: a parser that reaches the end can tell the end of what the peer sent
// from a stop. A direction the relay stopped while its peer carried on (the
// recording stopped, or the other direction ended the relay), or one that lost
// a chunk, ends unmarked: the parser cannot know what such a stream is
// missing.
//
// Before, nothing on the proxy path marked a stream ended with its
// connection, so every end looked like a stop. A Couchbase recorder, which
// records a quiet request (answered only when it fails) only once the server's
// stream ended with its connection with no error reply for it, reported every
// quiet request still waiting at a normal close as left out: capture loss
// that did not happen.
func TestRecordViaSupervisorSaysAStreamEndedWithItsConnection(t *testing.T) {
	ends := func(t *testing.T, p *holeEndProbe) {
		t.Helper()
		<-p.endedC[fakeconn.FromClient]
		<-p.endedC[fakeconn.FromDest]
	}
	withConn := func(p *holeEndProbe) [2]bool {
		p.mu.Lock()
		defer p.mu.Unlock()
		return p.withConn
	}

	t.Run("the destination closes the connection", func(t *testing.T) {
		probe := newHoleEndProbe()
		h := newHoleEndHarness(t, probe, 1<<20)
		close(probe.gate)
		h.writeApp(t, "ping")
		// The destination closes only once it has the request: a close
		// while the relay still writes it fails that write, and the relay
		// stops the destination's direction before it reads the close.
		for h.destSeen() != "ping" {
			<-h.destFed
		}
		h.writeDest(t, "pong")
		h.closeDest()
		ends(t, probe)

		if got := strings.Join(probe.delivered(fakeconn.FromDest), ""); got != "pong" {
			t.Fatalf("the destination's stream delivered %q, want %q", got, "pong")
		}
		if got := strings.Join(probe.delivered(fakeconn.FromClient), ""); got != "ping" {
			t.Fatalf("the client's stream delivered %q, want %q", got, "ping")
		}
		got := withConn(probe)
		if !got[fakeconn.FromDest] {
			probe.mu.Lock()
			endErr := probe.endErr
			probe.mu.Unlock()
			t.Fatalf("the destination closed the connection, and its stream carried all it sent, "+
				"but at the stream's end (%v; the client's: %v) the session did not say it ended with its connection",
				endErr[fakeconn.FromDest], endErr[fakeconn.FromClient])
		}
		// The client never closed: the relay stopped its direction when the
		// destination's ended (a pipe has no half-close to forward).
		if got[fakeconn.FromClient] {
			t.Fatal("the session said the client's stream ended with its connection, " +
				"but the relay stopped it while the client carried on")
		}
	})

	t.Run("the recording stops while the connection lives", func(t *testing.T) {
		probe := newHoleEndProbe()
		h := newHoleEndHarness(t, probe, 1<<20)
		close(probe.gate)
		h.writeApp(t, "ping")
		h.writeDest(t, "pong")
		h.stop()
		ends(t, probe)

		if got := withConn(probe); got[fakeconn.FromClient] || got[fakeconn.FromDest] {
			t.Fatalf("the recording stopped while both peers carried on, but the session said "+
				"a stream ended with its connection (client %v, destination %v)",
				got[fakeconn.FromClient], got[fakeconn.FromDest])
		}
	})

	t.Run("the destination's stream lost a chunk before it closed", func(t *testing.T) {
		probe := newHoleEndProbe()
		// The 8-byte cap refuses the 10-byte chunk: a hole in the
		// destination's direction.
		h := newHoleEndHarness(t, probe, 8)
		close(probe.gate)
		h.writeDest(t, "0123456789")
		h.closeDest()
		ends(t, probe)

		if got := probe.delivered(fakeconn.FromDest); len(got) != 0 {
			t.Fatalf("the destination's stream delivered %q, want nothing (the cap refused it)", got)
		}
		if withConn(probe)[fakeconn.FromDest] {
			t.Fatal("the destination's stream lost a chunk, but the session said it ended with " +
				"its connection: a parser would take it to carry all the destination sent")
		}
	})
}
