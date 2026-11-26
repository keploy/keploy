//go:build linux || darwin

package proxy

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"sync"
	"testing"
	"time"

	"go.keploy.io/server/v3/config"
	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.uber.org/zap"
)

// A recording's stop asks what may still come from before it once its test
// has the answers it waited for (syncMock BacklogBefore). An exchange over the
// capture budget is decided, and counted over the size limit, only once its
// handler has written the whole answer and goes on. It took no building note,
// so a stop that asked in between heard "clear", ended the stream and read
// the summary before the count was made: the request was lost to the summary
// with no count and no word, the loss its count is there to name. Its note is
// taken as its answer's end is found, as a recorded exchange's is, and ended
// once it is counted. The handler is held right after its write
// (answerWritten) until the stop has asked; with --sync and without.
func TestAStopWaitsForTheCountOfAnExchangeOverTheBudget(t *testing.T) {
	for _, synchronous := range []bool{false, true} {
		t.Run(fmt.Sprintf("synchronous=%v", synchronous), func(t *testing.T) {
			parked, goOn := make(chan struct{}), make(chan struct{})
			var once bool
			saved := answerWritten
			answerWritten = func() {
				if !once {
					once = true
					close(parked)
					<-goOn
				}
			}
			// Restored once the handler is gone: Cleanup runs last-in
			// first, and serveOneOverBudget's waits for it.
			t.Cleanup(func() { answerWritten = saved })
			release := sync.OnceFunc(func() { close(goOn) })
			pm, mgr, tally, done := serveOneOverBudget(t, synchronous, release)
			select {
			case <-parked:
			case <-done:
				t.Fatal("the handler returned without being held after its write")
			}
			stopFindsItPending(t, mgr, "the handler of an exchange over the capture budget waits after its write")

			release()
			overBudgetCountedAndNothingLeft(t, pm, mgr, tally, done)
		})
	}
}

// An exchange over the capture budget is counted, and only then ends its note
// (buildingNote): a recording's stop that asks in between still waits, and the
// summary it then reads holds the count. Counted after the note's end, a stop
// that asked in between heard nothing was to come, ended the stream, and read
// its summary without this request. The handler is held right after the
// count, before the note's end (lossCounted), until the stop has asked; with
// --sync and without.
func TestAStopStillWaitsAsAnExchangeOverTheBudgetIsCounted(t *testing.T) {
	for _, synchronous := range []bool{false, true} {
		t.Run(fmt.Sprintf("synchronous=%v", synchronous), func(t *testing.T) {
			parked, goOn := holdAtTheCount(t)
			pm, mgr, tally, done := serveOneOverBudget(t, synchronous, goOn)
			select {
			case <-parked:
			case <-done:
				t.Fatal("the handler returned without reaching the count")
			}
			stopFindsItPending(t, mgr, "the exchange over the capture budget is counted")
			if got := tally.Read().Total; got != 1 {
				t.Errorf("held right after the count, %d exchanges are counted over the size limit, want 1", got)
			}
			goOn()
			overBudgetCountedAndNothingLeft(t, pm, mgr, tally, done)
		})
	}
}

// serveOneOverBudget serves one Connection: close exchange whose answer is
// over the capture budget through the HTTP/1 ingress (with --sync or
// without), on a manager of its own with a quiet capture (a stop can tell
// what is still to come) and a session's tally open, and reads its whole
// answer. done closes once the handler returns; letGo lets a handler held in
// a test seam go on, and is called before the test waits for the handler.
func serveOneOverBudget(t *testing.T, synchronous bool, letGo func()) (pm *IngressProxyManager, mgr *syncMock.SyncMockManager, tally *syncMock.OverSizeTally, done chan struct{}) {
	t.Helper()
	big := maxHTTPBodyCaptureBytes + 64<<10
	appLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	serveApp(t, appLn, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(big))
		_, _ = w.Write(bytes.Repeat([]byte("y"), big))
	})
	cfg := &config.Config{}
	cfg.Agent.MockMode = true
	cfg.Agent.RecordRequests = true
	cfg.Agent.Synchronous = synchronous
	pm = New(zap.NewNop(), nil, cfg)
	mgr = syncMock.New(zap.NewNop())
	mgr.SetFirstRequestSignaled()
	t.Cleanup(mgr.AddWatermark(quietCapture{}))
	tally = mgr.OpenOverSizeTally()
	t.Cleanup(tally.Close)
	client, proxied := tcpPairForWatch(t)
	ctx, cancel := context.WithCancel(syncMock.NewContext(context.Background(), mgr))
	done = make(chan struct{})
	go func() {
		defer close(done)
		defer func() { _ = proxied.Close() }()
		pm.ServeLive(ctx, proxied, appLn.Addr().String(), 8083)
	}()
	// The handler is gone before the test ends, whatever failed.
	t.Cleanup(func() {
		letGo()
		cancel()
		_ = client.Close()
		<-done
	})
	if _, err := fmt.Fprintf(client, "GET /big HTTP/1.1\r\nHost: app\r\nConnection: close\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(client), nil)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := io.Copy(io.Discard, resp.Body); err != nil || n != int64(big) {
		t.Fatalf("the client read %d bytes of the answer (%v), want %d", n, err, big)
	}
	_ = resp.Body.Close()
	return pm, mgr, tally, done
}

// overBudgetCountedAndNothingLeft checks, once the handler is gone, that the
// exchange over the capture budget was counted once and reached no queue, and
// that a stop then has nothing to wait for.
func overBudgetCountedAndNothingLeft(t *testing.T, pm *IngressProxyManager, mgr *syncMock.SyncMockManager, tally *syncMock.OverSizeTally, done chan struct{}) {
	t.Helper()
	<-done
	pm.captures.Wait()
	if got := tally.Read(); got.Total != 1 {
		t.Fatalf("the exchange over the capture budget was counted %+v, want once", got)
	}
	if len(pm.tcChan) != 0 {
		t.Fatalf("an exchange over the capture budget reached the queue as %d test cases", len(pm.tcChan))
	}
	if st := mgr.BacklogBefore(time.Now()); st.Pending {
		t.Fatalf("the exchange is counted, and the stop still waits on %q", st.WaitingOn)
	}
}
