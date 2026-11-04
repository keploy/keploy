//go:build linux

package proxy

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"strconv"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/connseq"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	httpint "go.keploy.io/server/v3/pkg/agent/proxy/integrations/http"
	"go.keploy.io/server/v3/pkg/agent/proxy/supervisor"
	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.keploy.io/server/v3/pkg/agent/proxy/util"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
	"golang.org/x/sync/errgroup"
)

// Through real sockets, recordViaSupervisor and the HTTP parser, as a proxied
// connection is recorded: an app reads a chunked response's head and its JSON
// chunk and closes the connection (a Go client with keep-alives off closes on
// Body.Close once it decoded the JSON), and the server writes the body's last
// chunk only once the app's FIN, which the relay forwards, has reached it, as
// a server whose handler returns late does. The app refuses that last chunk
// with a reset, and the destination's stream ends where the app stopped
// taking it. The exchange is recorded as far as the app took it, with a
// stopped_reading fault and a whole answer's head (the Content-Length of what
// it read, no Transfer-Encoding), so replay serves it without its end; nothing
// is left out. An app that read the whole response before it closed has it
// recorded whole, with no fault. A server that closes after the app's FIN
// without the last chunk gives the same mock as one that sends it: what the
// server does once the app closed is nothing the app saw. An app that closed
// before the response's head came whole had no response: the exchange is left
// out, and said so, as before. KEPLOY_PROVEN does not change any of it.
func TestRecordViaSupervisor_AResponseTheAppStoppedReadingIsRecordedAsFarAsItRead(t *testing.T) {
	const (
		request = "GET /v1/charges/ch_1 HTTP/1.1\r\nHost: pay\r\n\r\n"
		head    = "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nTransfer-Encoding: chunked\r\n\r\n"
		json    = `{"id":"ch_1"}`
		data    = "d\r\n" + json + "\r\n"
		end     = "0\r\n\r\n"
	)
	for _, row := range []struct {
		name string
		// beforeClose is what the server sends, and the app reads, before
		// the app closes; afterFIN what the server sends once the app's FIN
		// reached it, before it closes.
		beforeClose, afterFIN string
		stopped               bool
		// leftOut: no mock is recorded, and the exchange is counted and
		// named as left out.
		leftOut bool
	}{
		{name: "the last chunk after the app's FIN", beforeClose: head + data, afterFIN: end, stopped: true},
		{name: "the app read the whole response", beforeClose: head + data + end},
		{name: "the server closes after the app's FIN, without the last chunk", beforeClose: head + data, stopped: true},
		{name: "the app closes before the head came whole", beforeClose: head[:20], leftOut: true},
	} {
		for _, proven := range []bool{false, true} {
			name := row.name
			if proven {
				name += ", KEPLOY_PROVEN on"
			}
			t.Run(name, func(t *testing.T) {
				v := ""
				if proven {
					v = "1"
				}
				t.Setenv(provenswitch.Env, v)
				supervisor.ResetLeftOutWarningsForTest()
				core, logs := observer.New(zapcore.DebugLevel)
				logger := zap.New(core)
				mgr := syncMock.New(nil)
				out := make(chan *models.Mock, 8)
				mgr.SetOutputChannel(out)
				ctx, cancel := context.WithTimeout(syncMock.NewContext(context.Background(), mgr), 30*time.Second)
				defer cancel()

				clientApp, rawSrc, cleanupSrc := tcpConnPair(t)
				defer cleanupSrc()
				dstConn, destSvc, cleanupDst := tcpConnPair(t)
				defer cleanupDst()
				// Exactly what handleConnection builds before dispatch.
				srcConn := &util.Conn{Conn: rawSrc, Reader: rawSrc, Logger: zap.NewNop()}
				p := &Proxy{
					logger:                 logger,
					recordBufferCap:        1 << 20,
					recordBufferQueueSize:  64,
					recordBufferStallGrace: 2 * time.Second,
					// Hooks that say how the app releases its connections,
					// with no record of this one's release, as Linux's: the
					// app's close is its own (peerEndMarker).
					DestInfo: &fakeConnEnds{},
				}
				done := make(chan error, 1)
				go func() {
					done <- p.recordViaSupervisor(ctx, srcConn, connseq.NewUpstream(dstConn), httpint.New(logger), integrations.HTTP,
						make(chan *models.Mock, 8), &errgroup.Group{}, logger, 1, 2, models.OutgoingOptions{})
				}()

				if _, err := clientApp.Write([]byte(request)); err != nil {
					t.Fatal(err)
				}
				_ = destSvc.SetReadDeadline(time.Now().Add(10 * time.Second))
				if _, err := http.ReadRequest(bufio.NewReader(destSvc)); err != nil {
					t.Fatalf("the server did not get the request: %v", err)
				}
				if _, err := destSvc.Write([]byte(row.beforeClose)); err != nil {
					t.Fatal(err)
				}
				_ = clientApp.SetReadDeadline(time.Now().Add(10 * time.Second))
				if _, err := io.ReadFull(clientApp, make([]byte, len(row.beforeClose))); err != nil {
					t.Fatalf("the app did not read what the server sent: %v", err)
				}
				if err := clientApp.Close(); err != nil {
					t.Fatal(err)
				}
				// The relay forwarded the app's FIN: the server reads its end.
				if rest, err := io.ReadAll(destSvc); err != nil || len(rest) != 0 {
					t.Fatalf("the server read %d more bytes, %v, before the FIN", len(rest), err)
				}
				if row.afterFIN != "" {
					if _, err := destSvc.Write([]byte(row.afterFIN)); err != nil {
						t.Fatal(err)
					}
				}
				_ = destSvc.Close()

				select {
				case err := <-done:
					if err != nil && !row.leftOut {
						t.Fatalf("recordViaSupervisor returned %v", err)
					}
				case <-time.After(20 * time.Second):
					t.Fatal("recordViaSupervisor did not return after both ends closed")
				}
				warns := logs.FilterLevelExact(zapcore.WarnLevel).FilterMessage(supervisor.LeftOutWarnMsg).All()
				if row.leftOut {
					if n := mgr.MocksLeftOut(); n != 1 || len(warns) != 1 || len(out) != 0 {
						t.Fatalf("%d mocks recorded, %d counted left out, %d left-out WARNs; want none recorded, one left out and named", len(out), n, len(warns))
					}
					return
				}
				var mock *models.Mock
				select {
				case mock = <-out:
				default:
					t.Fatal("no mock recorded")
				}
				resp := mock.Spec.HTTPResp
				stopped := mock.Spec.Fault != nil && mock.Spec.Fault.Kind == models.FaultStoppedReading
				if resp == nil || resp.Body != json || stopped != row.stopped || row.stopped && mock.Spec.Fault.BodyBytes != int64(len(json)) {
					t.Fatalf("recorded %+v with the fault %s; want the body %q, stopped_reading %v", resp, mock.Spec.Fault, json, row.stopped)
				}
				// A whole answer's head, either way: the fault alone frames
				// one the app stopped reading at replay.
				if cl, te := resp.Header["Content-Length"], resp.Header["Transfer-Encoding"]; cl != strconv.Itoa(len(json)) || te != "" {
					t.Fatalf("recorded with Content-Length %q, Transfer-Encoding %q; want a whole answer's head: %d, none", cl, te, len(json))
				}
				if n := mgr.MocksLeftOut(); n != 0 || len(warns) != 0 {
					t.Errorf("the manager counts %d mocks left out, %d left-out WARNs; want none", n, len(warns))
				}
			})
		}
	}
}
