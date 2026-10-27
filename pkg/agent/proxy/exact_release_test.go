package proxy

import (
	"context"
	"fmt"
	"net/http"
	"runtime"
	"strings"
	"testing"
	"time"

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
