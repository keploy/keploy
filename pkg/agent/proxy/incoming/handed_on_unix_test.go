//go:build linux || darwin

package proxy

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"go.uber.org/zap"

	"go.keploy.io/server/v3/config"
	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
)

// A request the app served and the ingress was recording either reaches the
// recording's queue as a test case or is counted as one that did not
// (syncMock NoteNotHandedOn): never a silent loss. The context a capture runs
// on ends as its ingress stops (a forwarder's StopIngress, the agent's stop),
// and the capture's waits for a slot and for the queue gave up on it without
// a count: a recording that lost test cases so ended short with nothing said
// (tcs_sent_to_cli and tcs_suppressed_total both 0 in a run that lost every
// one). Each round serves one Connection: close exchange and ends the
// context the moment the exchange is served, as an ingress stopped then
// would; the capture is then racing that end.
func TestEveryRecordedRequestIsHandedOnOrCountedAsItsIngressStops(t *testing.T) {
	appLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	serveApp(t, appLn, func(w http.ResponseWriter, _ *http.Request) { _, _ = fmt.Fprint(w, "pong") })
	cfg := &config.Config{}
	cfg.Agent.MockMode = true
	cfg.Agent.RecordRequests = true
	pm := New(zap.NewNop(), nil, cfg)
	mgr := syncMock.New(zap.NewNop())
	const rounds = 80 // under the channel's 100, so no round waits on a reader
	for i := 0; i < rounds; i++ {
		client, proxied := tcpPairForWatch(t)
		ctx, cancel := context.WithCancel(syncMock.NewContext(context.Background(), mgr))
		done := make(chan struct{})
		go func() {
			defer close(done)
			pm.ServeLive(ctx, proxied, appLn.Addr().String(), 8083)
			cancel() // the ingress stops as soon as the exchange is served
			_ = proxied.Close()
		}()
		if _, err := fmt.Fprintf(client, "GET /ping/%d HTTP/1.1\r\nHost: app\r\nConnection: close\r\n\r\n", i); err != nil {
			t.Fatal(err)
		}
		resp, err := http.ReadResponse(bufio.NewReader(client), nil)
		if err != nil {
			t.Fatal(err)
		}
		// The whole answer is read before the client goes: one that goes
		// first is told as not recorded for that (tellClientLeft).
		if _, err := io.ReadAll(resp.Body); err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		_ = client.Close()
		<-done
	}
	pm.captures.Wait()
	delivered, counted := uint64(len(pm.tcChan)), mgr.NotHandedOn()
	if delivered+counted != rounds {
		t.Fatalf("of %d served requests %d reached the test-case channel and %d were counted as not handed on: %d were lost without a count",
			rounds, delivered, counted, rounds-delivered-counted)
	}
	t.Logf("%d handed on, %d counted as not handed on", delivered, counted)
}

// After an exchange is served, the HTTP/1 capture waits for a capture slot
// (takeCaptureSlot) before it builds the test case. An ingress that stops
// while every slot is held ends that wait: the app served the request and its
// test case is never made, so it is counted as one not handed on (syncMock
// NoteNotHandedOn), never lost without a word. Every slot is held here for
// the whole test, so each exchange can leave its capture only by its
// context's end.
func TestAnExchangeWaitingForACaptureSlotAsItsIngressStopsIsCounted(t *testing.T) {
	// No capture of another test reads the semaphore while it is swapped:
	// each test that starts captures waits for them before it returns.
	saved := captureHookSem
	t.Cleanup(func() { captureHookSem = saved })
	held := make(chan struct{}, 1)
	held <- struct{}{} // every slot is held, and none is given back
	captureHookSem = held

	appLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	serveApp(t, appLn, func(w http.ResponseWriter, _ *http.Request) { _, _ = fmt.Fprint(w, "pong") })
	cfg := &config.Config{}
	cfg.Agent.MockMode = true
	cfg.Agent.RecordRequests = true
	pm := New(zap.NewNop(), nil, cfg)
	mgr := syncMock.New(zap.NewNop())
	t.Cleanup(mgr.AddWatermark(quietCapture{}))
	const rounds = 8
	for i := 0; i < rounds; i++ {
		client, proxied := tcpPairForWatch(t)
		ctx, cancel := context.WithCancel(syncMock.NewContext(context.Background(), mgr))
		done := make(chan struct{})
		go func() {
			defer close(done)
			pm.ServeLive(ctx, proxied, appLn.Addr().String(), 8083)
			_ = proxied.Close()
		}()
		if _, err := fmt.Fprintf(client, "GET /ping/%d HTTP/1.1\r\nHost: app\r\nConnection: close\r\n\r\n", i); err != nil {
			cancel()
			t.Fatal(err)
		}
		resp, err := http.ReadResponse(bufio.NewReader(client), nil)
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		if _, err := io.ReadAll(resp.Body); err != nil {
			cancel()
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		// Served. With no slot to take, the exchange's capture cannot have
		// ended: a handler that returned here never waited for one.
		select {
		case <-done:
			cancel()
			t.Fatalf("round %d: the ingress finished with every capture slot held: the exchange did not wait for a slot", i)
		default:
		}
		cancel() // the ingress stops while the exchange waits for a slot
		<-done
		_ = client.Close()
	}
	pm.captures.Wait()
	if n := len(pm.tcChan); n != 0 {
		t.Fatalf("%d test cases reached the queue with every capture slot held", n)
	}
	if got := mgr.NotHandedOn(); got != rounds {
		t.Fatalf("of %d exchanges whose ingress stopped while they waited for a capture slot, %d were counted as not handed on: %d were lost without a count",
			rounds, got, rounds-int(got))
	}
	// Each was noted as being built as its answer's end was found
	// (buildingNote), and its note ends with its count: a stop waits on
	// none of them.
	if st := mgr.BacklogBefore(time.Now()); st.Pending {
		t.Fatalf("every exchange is counted, and a stop still waits on %q", st.WaitingOn)
	}
}

// quietCapture is a capture whose readers and parsers are always through: a
// recording's stop can tell what is still to come (syncMock Backlog), and
// only the ingress's own backlog can hold it.
type quietCapture struct{}

func (quietCapture) Settled(_, _ string, _, _ time.Time) bool { return true }
func (quietCapture) PendingBefore(time.Time) bool             { return false }

// serveOneToAStoppingTest serves one Connection: close exchange through the
// HTTP/1 ingress (with --sync or without) and reads its whole answer, as a
// test that then ends and has its recording stopped would. done closes once
// the handler returns.
func serveOneToAStoppingTest(t *testing.T, synchronous bool) (pm *IngressProxyManager, mgr *syncMock.SyncMockManager, done chan struct{}) {
	t.Helper()
	appLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	serveApp(t, appLn, func(w http.ResponseWriter, _ *http.Request) { _, _ = fmt.Fprint(w, "pong") })
	cfg := &config.Config{}
	cfg.Agent.MockMode = true
	cfg.Agent.RecordRequests = true
	cfg.Agent.Synchronous = synchronous
	pm = New(zap.NewNop(), nil, cfg)
	mgr = syncMock.New(zap.NewNop())
	mgr.SetFirstRequestSignaled()
	t.Cleanup(mgr.AddWatermark(quietCapture{}))
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
		cancel()
		_ = client.Close()
		<-done
	})
	if _, err := fmt.Fprintf(client, "GET /ping HTTP/1.1\r\nHost: app\r\nConnection: close\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(client), nil)
	if err != nil {
		t.Fatal(err)
	}
	if body, err := io.ReadAll(resp.Body); err != nil || string(body) != "pong" {
		t.Fatalf("the client read %q (%v), not the whole answer", body, err)
	}
	_ = resp.Body.Close()
	return pm, mgr, done
}

// stopFindsItPending is what a recording's stop that asks now hears of the
// exchange whose answer its test has (syncMock BacklogBefore): its test case
// is still to come, so the stop waits for it.
func stopFindsItPending(t *testing.T, mgr *syncMock.SyncMockManager, where string) {
	t.Helper()
	st := mgr.BacklogBefore(time.Now())
	if !st.Known {
		t.Fatal("the stop cannot tell what is still to come: no capture watermark")
	}
	if !st.Pending {
		t.Errorf("%s: the client has the whole answer, and a stop that asks now hears nothing is to come: it ends the recording's stream and the test case is left out, with no count and no word", where)
		return
	}
	t.Logf("%s: the stop waits on %q", where, st.WaitingOn)
}

// builtAndHandedOn checks the exchange's test case reached the queue once its
// capture could go on, and that the stop then has nothing more to wait for.
func builtAndHandedOn(t *testing.T, pm *IngressProxyManager, mgr *syncMock.SyncMockManager, done chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-never(t):
		t.Fatal("the ingress did not finish its exchange")
	}
	pm.captures.Wait()
	if n := len(pm.tcChan); n != 1 {
		t.Fatalf("%d test cases reached the queue, want the exchange's 1", n)
	}
	if st := mgr.BacklogBefore(time.Now()); st.Pending {
		t.Fatalf("the test case is in the queue, and the stop still waits on %q", st.WaitingOn)
	}
}

// A recording's stop asks what may still come from before it (syncMock
// BacklogBefore) once its test has the answers it waited for. The capture of
// an exchange whose client has the whole answer may still wait for a capture
// slot (takeCaptureSlot): its test case is still to come. It was noted as
// being built (BuildingTestCase) only once a slot came, so a stop that asked
// while it waited heard "clear", ended the stream, and the test case was left
// out with no count and no word. Every slot is held here until the stop has
// asked.
func TestAStopWaitsForAServedExchangeWhoseCaptureWaitsForASlot(t *testing.T) {
	// No capture of another test reads the semaphore while it is swapped:
	// each test that starts captures waits for them before it returns.
	saved := captureHookSem
	t.Cleanup(func() { captureHookSem = saved })
	held := make(chan struct{}, 1)
	held <- struct{}{} // every slot is held until the stop has asked
	captureHookSem = held

	pm, mgr, done := serveOneToAStoppingTest(t, false)
	select {
	case <-done:
		t.Fatal("the ingress finished with every capture slot held: the exchange did not wait for a slot")
	default:
	}
	stopFindsItPending(t, mgr, "its capture waits for a slot")
	<-held // a slot frees
	builtAndHandedOn(t, pm, mgr, done)
}

// The handler goes on once its client has the whole answer: with --sync it
// noted the test case as being built only after its write had returned, and
// without it only once a capture slot came. On a loaded host the handler can
// wait for the CPU right after its write while its test ends and the
// recording's stop asks what is still to come: the stop heard "clear" and the
// test case was left out with no count and no word. It is noted before the
// answer's end is forwarded. The handler is held here right after its write
// (answerWritten) until the stop has asked.
func TestAStopWaitsForAnExchangeWhoseHandlerWaitsAfterItsWrite(t *testing.T) {
	for _, synchronous := range []bool{false, true} {
		name := "without --sync"
		if synchronous {
			name = "with --sync"
		}
		t.Run(name, func(t *testing.T) {
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
			// first, and serveOneToAStoppingTest's waits for it.
			t.Cleanup(func() { answerWritten = saved })
			pm, mgr, done := serveOneToAStoppingTest(t, synchronous)
			var goneOn bool
			t.Cleanup(func() {
				if !goneOn {
					close(goOn)
				}
			})
			select {
			case <-parked:
			case <-never(t):
				t.Fatal("the handler did not go on past its write")
			}
			stopFindsItPending(t, mgr, "the handler waits after its write")
			goneOn = true
			close(goOn)
			builtAndHandedOn(t, pm, mgr, done)
		})
	}
}
