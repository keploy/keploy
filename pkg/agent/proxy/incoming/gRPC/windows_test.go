package proxy

import (
	"context"
	"fmt"
	"io"
	"net"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg"
	"go.keploy.io/server/v3/pkg/agent/memoryguard"
	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
)

var reqFields = []hpack.HeaderField{
	{Name: ":method", Value: "POST"},
	{Name: ":path", Value: "/svc.Echo/Ping"},
	{Name: "content-type", Value: "application/grpc"},
}

// The scanner opens a call at each HEADERS frame on a stream above every one
// opened before, wherever the reads cut the bytes: not at a request's
// trailers, nor at a PRIORITY frame for a stream that never opens.
func TestStartScannerOpensEachCallAtItsHeadersWhereverTheReadsCut(t *testing.T) {
	var b []byte
	b = append(b, http2.ClientPreface...)
	b = append(b, headersFrameBytes(t, 1, reqFields, false)...)
	b = append(b, dataFrameBytes(t, 1, false, grpcPayloadBytes([]byte("x")))...)
	b = append(b, headersFrameBytes(t, 1, []hpack.HeaderField{{Name: "x-trailer", Value: "1"}}, true)...) // trailers
	b = append(b, frameBytes(t, func(fr *http2.Framer) error {
		return fr.WritePriority(7, http2.PriorityParam{Weight: 1})
	})...) // an idle stream's priority: it opens nothing
	b = append(b, headersFrameBytes(t, 3, reqFields, true)...)
	b = append(b, frameBytes(t, func(fr *http2.Framer) error { return fr.WritePing(false, [8]byte{}) })...) // not a stream's
	b = append(b, headersFrameBytes(t, 5, reqFields, false)...)
	for _, cut := range []int{1, 2, 5, 9, 10, 13, len(b)} {
		var s startScanner
		var got []uint32
		for i := 0; i < len(b); i += cut {
			got = append(got, s.scan(b[i:min(i+cut, len(b))])...)
		}
		if len(got) != 3 || got[0] != 1 || got[1] != 3 || got[2] != 5 {
			t.Fatalf("reads of %d bytes: opened %v, want [1 3 5]", cut, got)
		}
	}
}

// stubShiftedClock stamps the forwarder's chunks shift behind the system
// clock: a call stamped so is as old, to another request's stale cutoff, as
// one in flight for shift.
func stubShiftedClock(t *testing.T, shift time.Duration) {
	t.Helper()
	stubbedClock.Store(&forwardClock{now: func() time.Time { return time.Now().Add(-shift) }, sleep: time.Sleep})
	t.Cleanup(func() { stubbedClock.Store(nil) })
}

// A call's window opens before the app can read its request, and holds every
// mock the app makes for it while it is in flight: another request's verdict
// past the stale horizon drops none of them. With --sync (the call signals
// the recording's first request, so its mocks are buffered) the call's test
// case gets its mock, mapped to its name; before, the cutoff dropped it and
// the test case was left out. Without --sync nothing is buffered; the window ends
// once the capture has decided it. A mock made after it, while no call is in
// flight, is outside the requests; one made during the call is not.
func TestRecordIncoming_ACallsWindowHoldsItsMocksPastTheStaleHorizon(t *testing.T) {
	for _, sync := range []bool{true, false} {
		t.Run(map[bool]string{true: "sync", false: "async"}[sync], func(t *testing.T) {
			shift := syncMock.StaleHorizon + 3*time.Second
			stubShiftedClock(t, shift)
			m := syncMock.New(zap.NewNop())
			out := make(chan *models.Mock, 64)
			m.SetOutputChannel(out)
			maps := make(chan models.TestMockMapping, 16)
			mctx, mcancel := context.WithCancel(context.Background())
			defer mcancel()
			m.SetMappingChannel(mctx, maps)
			old := time.Now().Add(-time.Hour)
			for i := 0; i < models.StartupMockTestCaseWindow; i++ {
				m.ResolveRange(old, old, "", true, false)
			}
			remove := m.AddWatermark(caughtUp{})
			defer remove()
			tcCh := make(chan *models.TestCase, 4)
			ctx, cancel := context.WithCancel(syncMock.NewContext(context.Background(), m))
			defer cancel()
			cliApp, cliProxy := net.Pipe()
			appProxy, appApp := net.Pipe()
			// The forwarder's first write to the app returns only once the
			// test has looked at the windows: the app has the request, and
			// the forwarder has done nothing after the write.
			looked := make(chan struct{})
			done := make(chan struct{})
			go func() {
				defer close(done)
				_ = RecordIncoming(ctx, zap.NewNop(), cliProxy, &heldWriteConn{Conn: appProxy, held: looked}, tcCh, 50051, "", sync, true)
			}()
			defer func() {
				cancel()
				_ = cliApp.Close()
				_ = appApp.Close()
				<-done
			}()
			req, resp := pingCall(t)
			go func() { _, _ = cliApp.Write(req) }()
			if _, err := io.ReadFull(appApp, make([]byte, len(req))); err != nil {
				t.Fatal(err)
			}
			n := m.OpenWindows()
			close(looked)
			if n != 1 {
				t.Fatalf("%d windows open as the app read the call's request, want its own", n)
			}
			during := perTestMockAt(clockNow())
			m.AddMock(during)
			now := time.Now()
			m.ResolveRange(now.Add(-10*time.Millisecond), now, "test-other", true, true)
			go func() { _, _ = appApp.Write(resp) }()
			if _, err := io.ReadFull(cliApp, make([]byte, len(resp))); err != nil {
				t.Fatal(err)
			}
			var tc *models.TestCase
			select {
			case tc = <-tcCh:
			case <-time.After(10 * time.Second):
				t.Fatal("the call's test case was not handed on")
			}
			deadline := time.Now().Add(5 * time.Second)
			for m.OpenWindows() != 0 {
				if time.Now().After(deadline) {
					t.Fatal("the call's window was not ended once its capture decided it")
				}
				time.Sleep(time.Millisecond)
			}
			after := perTestMockAt(time.Now().Add(time.Millisecond))
			m.AddMock(after)
			if during.OutsideRequests || !after.OutsideRequests {
				t.Fatalf("outside the requests: the mock made during the call %v, the one made after it %v", during.OutsideRequests, after.OutsideRequests)
			}
			emitted := false
			for len(out) > 0 {
				if <-out == during {
					emitted = true
				}
			}
			if !emitted {
				t.Fatal("the call's mock was dropped while the call was in flight")
			}
			if !sync {
				return
			}
			start, end := models.TestCaseWindow(tc)
			if !m.SendingTestCase(tc.Name, start, end) {
				t.Fatalf("%s is left out", tc.Name)
			}
			var mapped []string
			for len(maps) > 0 {
				if mp := <-maps; mp.TestName == tc.Name {
					mapped = append(mapped, mp.MockIDs...)
				}
			}
			if len(mapped) != 1 {
				t.Fatalf("mapped %v to %s, want the call's mock", mapped, tc.Name)
			}
		})
	}
}

// heldWriteConn's writes return once held is closed.
type heldWriteConn struct {
	net.Conn
	held chan struct{}
}

func (c *heldWriteConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	<-c.held
	return n, err
}

func perTestMockAt(at time.Time) *models.Mock {
	return &models.Mock{Kind: models.HTTP, ConnectionID: "egress-1", Spec: models.MockSpec{ReqTimestampMock: at, ResTimestampMock: at.Add(time.Millisecond)},
		TestModeInfo: models.TestModeInfo{Lifetime: models.LifetimePerTest, LifetimeDerived: true}}
}

// parse runs parseFramesFromChan over chunks, one direction, to their end.
func parse(sm *pkg.DefaultStreamManager, calls *callWindows, outgoing bool, chunks ...[]byte) {
	ch := make(chan frameChunk, len(chunks))
	for _, c := range chunks {
		ch <- frameChunk{data: c, timestamp: time.Now()}
	}
	close(ch)
	parseFramesFromChan(zap.NewNop(), ch, sm, outgoing, func([]func()) {}, calls)
}

// A reset ends the window of a call that is not complete: one whose request
// the parser read, and one it has read nothing of yet (the app's reset can be
// handled before the client's frames are). A complete call's window is its
// capture's: a reset after it ends nothing.
func TestParseFramesFromChan_AResetEndsOnlyAnIncompleteCallsWindow(t *testing.T) {
	m := syncMock.New(zap.NewNop())
	calls := newCallWindows(m, false)
	req1, resp1 := pingCallOn(t, 1, true)
	req3 := headersFrameBytes(t, 3, reqFields, false)
	req5 := headersFrameBytes(t, 5, reqFields, false)
	calls.started(append(append(append([]byte{}, req1...), req3...), req5...), time.Now(), false)
	if n := m.OpenWindows(); n != 3 {
		t.Fatalf("%d windows open, want the three calls'", n)
	}
	sm := pkg.NewStreamManager(zap.NewNop())
	parse(sm, calls, false, req1, req3)
	rst := func(id uint32) []byte {
		return frameBytes(t, func(fr *http2.Framer) error { return fr.WriteRSTStream(id, http2.ErrCodeCancel) })
	}
	parse(sm, calls, true, resp1, rst(1), rst(3), rst(5))
	if n := m.OpenWindows(); n != 1 {
		t.Fatalf("%d windows open after the resets, want only the complete call's", n)
	}
	if calls.take(1) == nil {
		t.Fatal("a reset after the call was complete ended its window")
	}
	if got := sm.GetCompleteStreams(); len(got) != 1 || got[0].ID != 1 {
		t.Fatalf("complete calls after the resets: %d, want the one complete before them", len(got))
	}
}

// A parser that gives up on its connection, on either side, ends every window
// still open there and opens none from then on: no call on it completes any
// more. (Its give-up at a frame over pkg.MaxFrameSize cannot happen: a frame's
// length is 24 bits, and MaxFrameSize is the largest.)
func TestParseFramesFromChan_AParserThatGivesUpEndsTheConnectionsWindows(t *testing.T) {
	for _, c := range []struct {
		name     string
		outgoing bool
		bytes    []byte
	}{
		{"not HTTP/2", false, []byte("GET / HTTP/1.1\r\nHost: app\r\n\r\n")},
		{"a frame it cannot read", true, []byte{0, 0, 1, 0x4, 0x0, 0, 0, 0, 0, 0x0}}, // SETTINGS of one byte
	} {
		t.Run(c.name, func(t *testing.T) {
			m := syncMock.New(zap.NewNop())
			calls := newCallWindows(m, false)
			req, _ := pingCallOn(t, 1, true)
			calls.started(req, time.Now(), false)
			parse(pkg.NewStreamManager(zap.NewNop()), calls, c.outgoing, c.bytes)
			if n := m.OpenWindows(); n != 0 {
				t.Fatalf("%d windows open after the parser gave up", n)
			}
			calls.started(headersFrameBytes(t, 3, reqFields, false), time.Now(), false)
			if n := m.OpenWindows(); n != 0 {
				t.Fatalf("a call opened a window after the parser gave up (%d open)", n)
			}
		})
	}
}

// A recording of gRPC calls alone with --sync maps each call's mocks to its
// own test case: the call signals the recording's first request as its window
// opens, before the app reads it, so the mocks the app makes for it are held
// for its window. Before, nothing signalled it: every mock went out at once,
// owned by no test, and no test case had a mapping.
func TestRecordIncoming_AGRPCOnlySyncRecordingMapsEachCallsMocksToItsTestCase(t *testing.T) {
	m := syncMock.New(zap.NewNop())
	out := make(chan *models.Mock, 64)
	m.SetOutputChannel(out)
	maps := make(chan models.TestMockMapping, 16)
	mctx, mcancel := context.WithCancel(context.Background())
	defer mcancel()
	m.SetMappingChannel(mctx, maps)
	old := time.Now().Add(-time.Hour)
	for i := 0; i < models.StartupMockTestCaseWindow; i++ {
		m.ResolveRange(old, old, "", true, false)
	}
	remove := m.AddWatermark(caughtUp{})
	defer remove()
	tcCh := make(chan *models.TestCase, 4)
	ctx, cancel := context.WithCancel(syncMock.NewContext(context.Background(), m))
	defer cancel()
	cliApp, cliProxy := net.Pipe()
	appProxy, appApp := net.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = RecordIncoming(ctx, zap.NewNop(), cliProxy, appProxy, tcCh, 50051, "", true, true)
	}()
	defer func() {
		cancel()
		_ = cliApp.Close()
		_ = appApp.Close()
		<-done
	}()
	want := map[string]string{}
	for i, id := range []uint32{1, 3} {
		req, resp := pingCallOn(t, id, i == 0)
		go func() { _, _ = cliApp.Write(req) }()
		if _, err := io.ReadFull(appApp, make([]byte, len(req))); err != nil {
			t.Fatal(err)
		}
		mk := perTestMockAt(time.Now())
		mk.Name = fmt.Sprint("mock-", id)
		m.AddMock(mk)
		go func() { _, _ = appApp.Write(resp) }()
		if _, err := io.ReadFull(cliApp, make([]byte, len(resp))); err != nil {
			t.Fatal(err)
		}
		var tc *models.TestCase
		select {
		case tc = <-tcCh:
		case <-time.After(10 * time.Second):
			t.Fatalf("call %d's test case was not handed on", id)
		}
		start, end := models.TestCaseWindow(tc)
		if !m.SendingTestCase(tc.Name, start, end) {
			t.Fatalf("%s is left out", tc.Name)
		}
		want[tc.Name] = mk.Name
	}
	got := map[string][]string{}
	deadline := time.After(10 * time.Second)
	for len(got) < len(want) {
		select {
		case mp := <-maps:
			if _, ok := want[mp.TestName]; ok {
				got[mp.TestName] = append(got[mp.TestName], mp.MockIDs...)
			}
		case <-deadline:
			t.Fatalf("mapped %v, want %v", got, want)
		}
	}
	for name, mock := range want {
		if ids := got[name]; len(ids) != 1 || ids[0] != mock {
			t.Errorf("mapped %v to %s, want [%s]", ids, name, mock)
		}
	}
}

// A call signals the recording's first request only with --sync, and only
// when it may be recorded: not one read while the memory guard had paused
// recording, nor one on a connection whose calls are all handled (no window
// opens).
// RecordIncoming hands each call's window the pause it read the call's bytes
// under: a --sync call read while the memory guard has paused recording may not
// be recorded, so it does not signal the recording's first request, and the
// next one read once recording runs again does.
func TestRecordIncoming_ASyncCallReadDuringAPauseSignalsNoFirstRequest(t *testing.T) {
	m := syncMock.New(zap.NewNop())
	restore := memoryguard.SetRecordingPausedForTest(true)
	defer restore()
	tcCh := make(chan *models.TestCase, 4)
	ctx, cancel := context.WithCancel(syncMock.NewContext(context.Background(), m))
	defer cancel()
	cliApp, cliProxy := net.Pipe()
	appProxy, appApp := net.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = RecordIncoming(ctx, zap.NewNop(), cliProxy, appProxy, tcCh, 50051, "", true, true)
	}()
	defer func() {
		cancel()
		_ = cliApp.Close()
		_ = appApp.Close()
		<-done
	}()
	// The forwarder opens a call's window before it writes the call's bytes
	// on, so once the app has read them the signal is decided.
	send := func(req []byte) {
		go func() { _, _ = cliApp.Write(req) }()
		if _, err := io.ReadFull(appApp, make([]byte, len(req))); err != nil {
			t.Fatal(err)
		}
	}
	req, _ := pingCallOn(t, 1, true)
	send(req)
	if m.GetFirstReqSeen() {
		t.Fatal("a --sync call read while recording was paused signalled the first request")
	}
	restore()
	req, _ = pingCallOn(t, 3, false)
	send(req)
	if !m.GetFirstReqSeen() {
		t.Fatal("a --sync call read while recording runs did not signal the first request")
	}
}

func TestCallWindowsSignalTheFirstRequestOnlyForARecordableSyncCall(t *testing.T) {
	req, _ := pingCallOn(t, 1, true)
	later := headersFrameBytes(t, 3, reqFields, false)
	t.Run("sync", func(t *testing.T) {
		m := syncMock.New(zap.NewNop())
		c := newCallWindows(m, true)
		defer c.stop()
		c.started(req, time.Now(), true)
		if m.GetFirstReqSeen() {
			t.Fatal("a call read during a pause signalled the first request")
		}
		c.started(later, time.Now(), false)
		if !m.GetFirstReqSeen() {
			t.Fatal("a call that may be recorded did not signal the first request")
		}
	})
	t.Run("async", func(t *testing.T) {
		m := syncMock.New(zap.NewNop())
		c := newCallWindows(m, false)
		defer c.stop()
		c.started(req, time.Now(), false)
		if m.GetFirstReqSeen() {
			t.Fatal("a call without --sync signalled the first request")
		}
	})
	t.Run("stopped", func(t *testing.T) {
		m := syncMock.New(zap.NewNop())
		c := newCallWindows(m, true)
		c.stop()
		c.started(req, time.Now(), false)
		if m.GetFirstReqSeen() || m.OpenWindows() != 0 {
			t.Fatal("a call on a connection whose calls are all handled opened a window or signalled the first request")
		}
	})
}
