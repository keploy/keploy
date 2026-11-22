package proxy

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"runtime"
	"strings"
	"testing"
	"time"
	"unsafe"
	"weak"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	httpint "go.keploy.io/server/v3/pkg/agent/proxy/integrations/http"
	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// liveHeap is the heap's live bytes now: what is left after a collection.
func liveHeap() uint64 {
	runtime.GC()
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return ms.HeapAlloc
}

// A set's HTTP recordings go with the trees that held them: the HTTP
// parser's indexes of a set's reads (of a staging, and of the shared part of
// a test's reads) are let go when the next set is staged, not at the
// parser's next call, which may come only well into the next set, or never.
// Ten sets of 128 MiB of recordings each (1.25 GiB over the run) are staged
// in turn on one manager, as an agent serves a run's sets; each is served a
// test (its own call and a pool call) and two calls of other pool
// recordings between tests, so both indexes are built. After each staging, before any call, the live heap
// holds one set: under 1.5 sets above where the run began, whatever the
// number of sets. It holds two sets where the indexes keep the last set's
// recordings: run it on a box.
func TestASetsHTTPRecordingsGoWhenTheNextSetIsStaged(t *testing.T) {
	const sets, pieces, piece = 10, 16, 8 << 20
	const setSize = pieces * piece
	t.Setenv(provenswitch.Env, "1")
	p := startedDrainProxy(t, 100)
	p.learnTracker = learn.NewTracker()
	h := httpint.New(zap.NewNop())
	mm := NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(mm.Close)
	p.setMockManager(mm)
	t.Cleanup(func() { p.setMockManager(nil) })
	send := func(target string) (int, string) {
		return r3SendVia(t, context.Background(), h, p, mm, models.OutgoingOptions{}, httpGet(target))
	}
	begin := liveHeap()
	var names []string
	stage := func(k int) {
		now := time.Now()
		// One backing array per set, each recording's answer a piece of it:
		// a recording of the set kept anywhere keeps the whole set's bytes.
		backing := strings.Repeat(string(rune('a'+k)), setSize)
		var session []*models.Mock
		names = names[:0]
		for i := 0; i < pieces; i++ {
			name := fmt.Sprintf("mock-big-%d", i)
			session = append(session, r3Mock(name, now.Add(time.Duration(i)*time.Microsecond), "GET", fmt.Sprintf("/big/%d", i), "", backing[i*piece:(i+1)*piece]))
			names = append(names, name)
		}
		session = append(session, r3Mock("mock-pool", now.Add(pieces*time.Microsecond), "GET", "/pool", "", fmt.Sprintf(`{"pool":%d}`, k)))
		names = append(names, "mock-pool")
		for i := 0; i < 2; i++ {
			name := fmt.Sprintf("mock-between-%d", i)
			session = append(session, r3Mock(name, now.Add(time.Duration(pieces+1+i)*time.Microsecond), "GET", fmt.Sprintf("/between/%d", i), "", fmt.Sprintf(`{"between":"%d-%d"}`, k, i)))
			names = append(names, name)
		}
		start := now.Add(time.Second)
		own := r3Mock("mock-own", start.Add(100*time.Millisecond), "GET", "/own", "", fmt.Sprintf(`{"own":%d}`, k))
		mm.ResetForReplaySession()
		mm.SeedRecordedWindows([]models.TestWindow{{Start: start, End: start.Add(900 * time.Millisecond)}})
		mm.SetMocksWithWindow([]*models.Mock{own}, session, models.BaseTime, now.Add(time.Hour))
		mm.ServeExactBackground(models.RecordedWithS12Routing, names, nil)
		mm.ServeExactPool(models.PoolKeyNone, names)
	}
	for k := 0; k < sets; k++ {
		stage(k)
		if held := liveHeap() - begin; held > setSize*3/2 {
			t.Fatalf("set %d staged: the live heap holds %d MiB over the run's start, %.1f sets of %d MiB: an earlier set's recordings are kept", k, held>>20, float64(held)/setSize, setSize>>20)
		}
		mm.ServeExactTest("test-0", []*models.Mock{roundMock(mm, "mock-own")})
		p.ContinueTestErrorCapture()
		if code, body := send("/own"); code != http.StatusOK || body != fmt.Sprintf(`{"own":%d}`, k) {
			t.Fatalf("set %d: the test's own call got %d %s", k, code, body)
		}
		if code, body := send("/pool"); code != http.StatusOK || body != fmt.Sprintf(`{"pool":%d}`, k) {
			t.Fatalf("set %d: the test's pool call got %d %s", k, code, body)
		}
		mm.ServeExactPool(models.PoolKeyNone, names)
		for i := 0; i < 2; i++ {
			if code, body := send(fmt.Sprintf("/between/%d", i)); code != http.StatusOK || body != fmt.Sprintf(`{"between":"%d-%d"}`, k, i) {
				t.Fatalf("set %d: pool call %d between tests got %d %s", k, i, code, body)
			}
		}
	}
	if misses, _ := p.GetMockErrors(context.Background()); len(misses) != 0 {
		t.Fatalf("misses %+v", misses)
	}
}

// sendSettled is r3SendVia, returning once the parser that answered the call
// has returned too: a parser that saw the app hang up retries its empty
// read for a while (util.ReadBytes), holding what it served until then.
func sendSettled(t *testing.T, h integrations.Integrations, p *Proxy, mm *MockManager, request string) (int, string) {
	t.Helper()
	app, agentEnd := net.Pipe()
	ctx := models.WithMockMismatchReporter(context.Background(), p.sendMockNotFoundError)
	ctx = learn.WithTracker(ctx, p.learnTracker)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = h.MockOutgoing(ctx, agentEnd, &models.ConditionalDstCfg{}, mm, models.OutgoingOptions{})
		_ = agentEnd.Close()
	}()
	if _, err := app.Write([]byte(request)); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(app), nil)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	_ = app.Close()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("the parser that answered the call never returned")
	}
	return resp.StatusCode, string(b)
}

// An ended test's recordings go with the learn tracker's end of the test,
// not at its next begin: keploy mock replay reads each test's own as the
// test opens, and between two tests nothing needs the ended test's view.
// Four tests of 64 MiB of their own recordings each are served in turn on one
// manager, with the learn tracker the agent's proxy steps at each test's
// begin and end. Each test makes one call, of its small recording; its big
// ones answer none (a test that failed early), so neither the tracker nor
// the staging's take log holds them. Then the pool's view is served and the
// tracker ends the test, as the agent ends one. The test's recordings are
// then unreachable: a weak pointer to their backing array is nil after a GC.
// They stayed reachable where the round kept the ended test's view bound to
// the tracker's test number until the tracker's next begin
// (roundState.bound). Reachability, not the live heap's size, which memory
// earlier rows of the package free meanwhile can hide. Run it on a box.
func TestAnEndedTestsRecordingsGoWithTheTrackersEnd(t *testing.T) {
	const tests, pieces, piece = 4, 4, 16 << 20
	const testSize = pieces * piece
	t.Setenv(provenswitch.Env, "1")
	p := startedDrainProxy(t, 100)
	p.learnTracker = learn.NewTracker()
	h := httpint.New(zap.NewNop())
	mm := NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(mm.Close)
	p.setMockManager(mm)
	t.Cleanup(func() { p.setMockManager(nil) })
	now := time.Now()
	start := func(k int) time.Time { return now.Add(time.Duration(k+1) * time.Minute) }
	// The pool: one call between each test and the next.
	var pool []*models.Mock
	var poolNames []string
	var windows []models.TestWindow
	for k := 0; k < tests; k++ {
		windows = append(windows, models.TestWindow{TestCase: fmt.Sprintf("test-%d", k), Start: start(k), End: start(k).Add(30 * time.Second)})
		pool = append(pool, r3Mock(fmt.Sprintf("mock-pool-%d", k), start(k).Add(45*time.Second), "GET", fmt.Sprintf("/pool/%d", k), "", fmt.Sprintf(`{"pool":%d}`, k)))
		poolNames = append(poolNames, fmt.Sprintf("mock-pool-%d", k))
	}
	mm.SeedRecordedWindows(windows)
	mm.SetMocksWithWindow(nil, pool, models.BaseTime, now.Add(time.Hour))
	mm.ServeExactPool(models.PoolKeyNone, poolNames)
	// open serves test k its own recordings, read as the test opens: its
	// small one, then its big ones, slices of one backing array, so a big
	// recording of the test kept anywhere keeps the whole test's bytes. It
	// returns a weak pointer to that array.
	open := func(k int) weak.Pointer[byte] {
		backing := strings.Repeat(string(rune('a'+k)), testSize)
		owned := []*models.Mock{r3Mock(fmt.Sprintf("mock-%d-own", k), start(k).Add(time.Second), "GET", fmt.Sprintf("/own/%d", k), "", fmt.Sprintf(`{"own":%d}`, k))}
		for i := 0; i < pieces; i++ {
			owned = append(owned, r3Mock(fmt.Sprintf("mock-%d-%d", k, i), start(k).Add(time.Duration(i+2)*time.Second), "GET", fmt.Sprintf("/big/%d/%d", k, i), "", backing[i*piece:(i+1)*piece]))
		}
		mm.ServeExactTest(fmt.Sprintf("test-%d", k), owned)
		return weak.Make(unsafe.StringData(backing))
	}
	for k := 0; k < tests; k++ {
		p.ContinueTestErrorCapture()
		recordings := open(k)
		if code, body := sendSettled(t, h, p, mm, httpGet(fmt.Sprintf("/own/%d", k))); code != http.StatusOK || body != fmt.Sprintf(`{"own":%d}`, k) {
			t.Fatalf("test %d: its own call got %d %s", k, code, body)
		}
		mm.ServeExactPool(models.PoolKeyNone, poolNames)
		p.EndTestErrorCapture()
		runtime.GC()
		runtime.GC()
		if recordings.Value() != nil {
			t.Fatalf("test %d ended: its %d MiB of recordings are still reachable: an ended test's are kept", k, testSize>>20)
		}
		if code, body := sendSettled(t, h, p, mm, httpGet(fmt.Sprintf("/pool/%d", k))); code != http.StatusOK || body != fmt.Sprintf(`{"pool":%d}`, k) {
			t.Fatalf("after test %d: the pool's call got %d %s", k, code, body)
		}
	}
	if misses, _ := p.GetMockErrors(context.Background()); len(misses) != 0 {
		t.Fatalf("misses %+v", misses)
	}
}
