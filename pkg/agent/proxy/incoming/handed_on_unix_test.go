//go:build linux || darwin

package proxy

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
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
	pm, mgr, done, _ = serveOneToAStoppingIngress(t, synchronous)
	return pm, mgr, done
}

// serveOneToAStoppingIngress is serveOneToAStoppingTest whose caller also
// stops the ingress (stop ends the context the handler serves on).
func serveOneToAStoppingIngress(t *testing.T, synchronous bool) (pm *IngressProxyManager, mgr *syncMock.SyncMockManager, done chan struct{}, stop context.CancelFunc) {
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
	return pm, mgr, done, cancel
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

// A client that goes before its answer's end (a stream it stops reading, a
// request it gives up on) leaves an exchange that is not recorded: the app is
// told of its going (watchClient), and what it sends after may answer that
// rather than the request. The answer's end can still be read from the app
// after the client went, and from then the test case is noted as being built
// (buildingNote), before the handler finds its client gone. The handler's way
// out ends that note. Left, every later stop of the recording waited on a test
// case that never came, until its wait gave up and failed a good recording.
// With --sync and without, the client closing its connection or only its
// sending side.
func TestAClientThatLeftBeforeTheAnswersEndLeavesNoNoteForAStop(t *testing.T) {
	for _, tc := range []struct{ synchronous, halfClose bool }{{false, false}, {true, false}, {false, true}, {true, true}} {
		t.Run(fmt.Sprintf("synchronous %v, half-closed %v", tc.synchronous, tc.halfClose), func(t *testing.T) {
			gone := make(chan struct{})
			srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/x-ndjson")
				w.WriteHeader(http.StatusOK)
				_, _ = fmt.Fprintln(w, `{"status":"RUNNING"}`)
				if f, ok := w.(http.Flusher); ok {
					f.Flush()
				}
				<-r.Context().Done()
				close(gone)
				_, _ = fmt.Fprintln(w, `{"status":"CLIENT GONE"}`)
			})}
			appLn, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			go func() { _ = srv.Serve(appLn) }()
			t.Cleanup(func() { _ = srv.Close() })
			cfg := &config.Config{}
			cfg.Agent.MockMode = true
			cfg.Agent.RecordRequests = true
			cfg.Agent.Synchronous = tc.synchronous
			pm := New(zap.NewNop(), nil, cfg)
			mgr := syncMock.New(zap.NewNop())
			mgr.SetFirstRequestSignaled()
			t.Cleanup(mgr.AddWatermark(quietCapture{}))
			ctx, cancel := context.WithCancel(syncMock.NewContext(context.Background(), mgr))

			client, proxied := tcpPairForWatch(t)
			served := make(chan struct{})
			go func() {
				defer close(served)
				defer func() { _ = proxied.Close() }()
				pm.ServeLive(ctx, proxied, appLn.Addr().String(), 8083)
			}()
			// The handler is gone before the test ends, whatever failed.
			t.Cleanup(func() {
				cancel()
				_ = client.Close()
				<-served
			})
			if _, err := client.Write([]byte("GET /client/v1/jobs/j/events HTTP/1.1\r\nHost: api\r\n\r\n")); err != nil {
				t.Fatal(err)
			}
			br := bufio.NewReader(client)
			for {
				line, err := br.ReadString('\n')
				if err != nil {
					t.Fatalf("the answer's first event: %v", err)
				}
				if strings.Contains(line, "RUNNING") {
					break
				}
			}
			if tc.halfClose {
				tcp, ok := client.(*net.TCPConn)
				if !ok {
					t.Fatalf("the client is a %T, not a TCP connection to half-close", client)
				}
				_ = tcp.CloseWrite()
				_, _ = io.ReadAll(br)
			} else {
				_ = client.Close()
			}
			for _, c := range []chan struct{}{gone, served} {
				select {
				case <-c:
				case <-never(t):
					t.Fatal("the exchange did not end once its client went")
				}
			}
			pm.captures.Wait()
			if n := len(pm.tcChan); n != 0 {
				t.Fatalf("%d test cases reached the queue from an exchange whose client went before its answer's end", n)
			}
			if st := mgr.BacklogBefore(time.Now()); st.Pending {
				t.Errorf("the handler has returned and its exchange is not recorded, and a stop still waits on %q: it waits until its wait gives up and fails the recording", st.WaitingOn)
			}
		})
	}
}

// A client that takes an answer's headers and closes without reading its
// body (Go's http.Get and a Body.Close: a client that wanted only the status)
// went after the answer's end when the app wrote the answer whole at once:
// the end arrived with the headers, before the client could have them. The
// exchange is the app's answer to its request and is recorded. The client's
// close was judged against where the ingress had got to in forwarding the
// answer, and the ingress forwards the headers before it reads the body on:
// whenever the watch saw the close before the ingress read the body (often
// on a loaded host), the exchange was not recorded, "its client closed its
// side of the connection before the app's answer ended", and a test the user
// made went missing. The ingress is held here between the headers and the
// body until the app has been told of the close, as a loaded host holds it.
func TestAClientThatLeavesOnceTheAnswerArrivedWholeIsRecorded(t *testing.T) {
	for _, synchronous := range []bool{false, true} {
		t.Run(fmt.Sprintf("synchronous %v", synchronous), func(t *testing.T) {
			const body = `{"name":"whoami","served_at":1}`
			appLn, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = appLn.Close() })
			told := make(chan struct{})
			go func() {
				c, err := appLn.Accept()
				if err != nil {
					return
				}
				defer c.Close()
				br := bufio.NewReader(c)
				if _, err := http.ReadRequest(br); err != nil {
					return
				}
				// The whole answer in one write, as a Go server writes a
				// small one.
				_, _ = fmt.Fprintf(c, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s", len(body), body)
				// The app reads on until it is told its client went.
				_, _ = io.Copy(io.Discard, br)
				close(told)
			}()
			left := make(chan struct{})
			saved := bodyReading
			bodyReading = func() {
				for _, c := range []chan struct{}{left, told} {
					select {
					case <-c:
					case <-never(t):
						t.Error("the client did not go, or the app was not told of it")
						return
					}
				}
			}
			t.Cleanup(func() { bodyReading = saved })

			cfg := &config.Config{}
			cfg.Agent.MockMode = true
			cfg.Agent.RecordRequests = true
			cfg.Agent.Synchronous = synchronous
			pm := New(zap.NewNop(), nil, cfg)
			mgr := syncMock.New(zap.NewNop())
			mgr.SetFirstRequestSignaled()
			t.Cleanup(mgr.AddWatermark(quietCapture{}))
			ctx, cancel := context.WithCancel(syncMock.NewContext(context.Background(), mgr))

			client, proxied := tcpPairForWatch(t)
			served := make(chan struct{})
			go func() {
				defer close(served)
				defer func() { _ = proxied.Close() }()
				pm.ServeLive(ctx, proxied, appLn.Addr().String(), 8083)
			}()
			t.Cleanup(func() {
				cancel()
				_ = client.Close()
				<-served
			})
			if _, err := client.Write([]byte("GET /whoami HTTP/1.1\r\nHost: api\r\n\r\n")); err != nil {
				t.Fatal(err)
			}
			// The headers, and no more: the body is not yet forwarded.
			br := bufio.NewReader(client)
			for {
				line, err := br.ReadString('\n')
				if err != nil {
					t.Fatalf("the answer's headers: %v", err)
				}
				if line == "\r\n" {
					break
				}
			}
			_ = client.Close()
			close(left)
			select {
			case <-served:
			case <-never(t):
				t.Fatal("the exchange did not end once its client went")
			}
			pm.captures.Wait()
			if n := len(pm.tcChan); n != 1 {
				t.Fatalf("%d test cases reached the queue from an exchange whose app answered whole before its client went, want 1", n)
			}
			if tc := <-pm.tcChan; tc.HTTPResp.Body != body {
				t.Fatalf("the recorded answer's body is %q, want the app's %q", tc.HTTPResp.Body, body)
			}
		})
	}
}

// A way out of an exchange that does not record it counts what it lost, and
// only then ends the exchange's note (buildingNote): a recording's stop that
// asks in between still waits, and the summary it then reads holds the count.
// With the note ended first, a stop that asked in between heard nothing was to
// come, ended the stream, and read its summary without this loss: a silent 0.
// The handler is held right after the count (lossCounted) until the stop has
// asked.
func TestAStopStillWaitsAsAnExchangesLossIsCounted(t *testing.T) {
	// Without --sync: the ingress stops while the exchange's capture waits
	// for a slot, and the exchange is counted as not handed on.
	t.Run("without --sync, the ingress stops before a capture slot comes", func(t *testing.T) {
		// No capture of another test reads the semaphore while it is
		// swapped: each test that starts captures waits for them before it
		// returns.
		saved := captureHookSem
		t.Cleanup(func() { captureHookSem = saved })
		held := make(chan struct{}, 1)
		held <- struct{}{} // every slot is held, and none is given back
		captureHookSem = held

		parked, goOn := holdAtTheCount(t)
		pm, mgr, done, stop := serveOneToAStoppingIngress(t, false)
		t.Cleanup(goOn)
		select {
		case <-done:
			t.Fatal("the ingress finished with every capture slot held: the exchange did not wait for a slot")
		default:
		}
		stop()
		atTheCount(t, parked)
		stopFindsItPending(t, mgr, "the exchange's loss is counted")
		if got := mgr.NotHandedOn(); got != 1 {
			t.Errorf("held right after the count, %d exchanges are counted as not handed on, want 1", got)
		}
		goOn()
		nothingRecordedAndNothingLeft(t, pm, mgr, done)
	})
	// With --sync: the memory guard's pause begins once the answer is
	// written, and the exchange is counted as not recorded under pressure.
	// It leaves by the way out the handler's return also takes (leave), so
	// this holds that one's order too: no way out by return holds a note
	// and a count of its own (a body cut by the pause is never noted, and
	// every check of the pause before the answer's end keeps it from being
	// noted).
	t.Run("with --sync, the memory guard pauses recording once the answer is written", func(t *testing.T) {
		var paused atomic.Bool
		stubIngressPaused(t, paused.Load)
		savedWritten := answerWritten
		answerWritten = func() { paused.Store(true) }
		// Restored once the handler is gone: Cleanup runs last-in first,
		// and serveOneToAStoppingTest's waits for it.
		t.Cleanup(func() { answerWritten = savedWritten })

		parked, goOn := holdAtTheCount(t)
		pm, mgr, done := serveOneToAStoppingTest(t, true)
		t.Cleanup(goOn)
		atTheCount(t, parked)
		stopFindsItPending(t, mgr, "the exchange's loss is counted")
		if got := mgr.NotRecordedUnderPressure().Total; got != 1 {
			t.Errorf("held right after the count, %d exchanges are counted as not recorded under pressure, want 1", got)
		}
		goOn()
		nothingRecordedAndNothingLeft(t, pm, mgr, done)
	})
}

// holdAtTheCount holds the first handler that counts its exchange's loss
// right after the count (lossCounted): parked closes there, and the handler
// goes on once goOn is called (any number of times). The seam is restored
// once the handler is gone: the caller registers goOn's Cleanup after the
// handler's, so it runs first.
func holdAtTheCount(t *testing.T) (parked chan struct{}, goOn func()) {
	t.Helper()
	parked = make(chan struct{})
	release := make(chan struct{})
	var first, released sync.Once
	saved := lossCounted
	lossCounted = func() {
		first.Do(func() {
			close(parked)
			<-release
		})
	}
	t.Cleanup(func() { lossCounted = saved })
	return parked, func() { released.Do(func() { close(release) }) }
}

// atTheCount waits until the handler is held right after its count.
func atTheCount(t *testing.T, parked chan struct{}) {
	t.Helper()
	select {
	case <-parked:
	case <-never(t):
		t.Fatal("the handler did not count its exchange's loss")
	}
}

// nothingRecordedAndNothingLeft checks the exchange whose loss was counted
// reached no queue, and that a stop then has nothing to wait for.
func nothingRecordedAndNothingLeft(t *testing.T, pm *IngressProxyManager, mgr *syncMock.SyncMockManager, done chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-never(t):
		t.Fatal("the ingress did not finish its exchange")
	}
	pm.captures.Wait()
	if n := len(pm.tcChan); n != 0 {
		t.Fatalf("%d test cases reached the queue from an exchange counted as lost", n)
	}
	if st := mgr.BacklogBefore(time.Now()); st.Pending {
		t.Fatalf("the exchange is counted and its handler gone, and a stop still waits on %q", st.WaitingOn)
	}
}
