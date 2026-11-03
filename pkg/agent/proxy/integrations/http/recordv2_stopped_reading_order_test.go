//go:build linux

package http

import (
	"net"
	"testing"
	"time"

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
	}{
		{name: "a length", read: "HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nContent-Length: 20\r\n\r\n01234",
			rest: "56789abcdefghij", body: "01234"},
		{name: "chunks", read: "HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nTransfer-Encoding: chunked\r\n\r\n14\r\n01234",
			rest: "56789abcdefghij\r\n0\r\n\r\n", body: "01234"},
		{name: "the upstream's close", read: "HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nConnection: close\r\n\r\n01234",
			rest: "56789abcdefghij", body: "01234"},
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
					wantStoppedReading(t, mocks[0], shape.body)
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
