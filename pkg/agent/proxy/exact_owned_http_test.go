package proxy

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	httpint "go.keploy.io/server/v3/pkg/agent/proxy/integrations/http"
	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// ownedHTTPRun stages one test that owns n HTTP recordings, GETs of
// /item/<j>?size=small recorded in its window, opens it, and serves exactly:
// the latency lane's set of one test with many calls.
func ownedHTTPRun(t *testing.T, n int) (*Proxy, *MockManager) {
	t.Helper()
	t.Setenv(provenswitch.Env, "1")
	p := startedDrainProxy(t, 100)
	p.learnTracker = learn.NewTracker()
	now := time.Now()
	start := now.Add(time.Second)
	var staged []*models.Mock
	for j := 0; j < n; j++ {
		staged = append(staged, r3Mock(fmt.Sprintf("mock-own-%d", j), start.Add(time.Duration(j+1)*50*time.Microsecond), "GET", fmt.Sprintf("/item/%d?size=small", j), "", fmt.Sprintf(`{"id":%d}`, j)))
	}
	mm := NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(mm.Close)
	mm.SeedRecordedWindows([]models.TestWindow{{Start: start, End: start.Add(900 * time.Millisecond)}})
	mm.SetMocksWithWindow(staged, nil, models.BaseTime, now.Add(time.Hour))
	p.setMockManager(mm)
	t.Cleanup(func() { p.setMockManager(nil) })
	owned := make([]*models.Mock, n)
	for j := range owned {
		owned[j] = roundMock(mm, fmt.Sprintf("mock-own-%d", j))
	}
	mm.ServeExactTest("test-0", owned)
	p.ContinueTestErrorCapture()
	return p, mm
}

// A test's HTTP call reads only the test's own recordings of its method and
// path, however many the test owns: a test that owns 100 recordings and one
// that owns 1000, each calling every one in recorded order, read the same few
// of their own per call after the first, which files them. (The learn step
// and the strict pass each read every recording the test had not answered:
// up to 2000 a call at 1000, 200 at 100.)
func TestATestsHTTPCallReadsOnlyItsOwnRecordingsOfItsPath(t *testing.T) {
	defer exactViewVerify.Store(exactViewVerify.Load())
	exactViewVerify.Store(false) // the verify mode reads the whole set on every call: counted alone
	most := map[int]uint64{}
	for _, n := range []int{100, 1000} {
		p, mm := ownedHTTPRun(t, n)
		h := httpint.New(zap.NewNop())
		for j := 0; j < n; j++ {
			before := unansweredVisited.Load()
			code, body := r3SendVia(t, context.Background(), h, p, mm, models.OutgoingOptions{}, httpGet(fmt.Sprintf("/item/%d?size=small", j)))
			if code != http.StatusOK || body != fmt.Sprintf(`{"id":%d}`, j) {
				t.Fatalf("%d recordings: call %d got %d %s", n, j, code, body)
			}
			if j > 0 {
				most[n] = max(most[n], unansweredVisited.Load()-before)
			}
		}
		if misses, _ := p.GetMockErrors(context.Background()); len(misses) != 0 {
			t.Fatalf("%d recordings: misses %+v", n, misses)
		}
	}
	t.Logf("most of the test's own recordings a call read: %d at 100, %d at 1000", most[100], most[1000])
	if most[1000] != most[100] || most[1000] > 4 {
		t.Fatalf("a call read up to %d of the test's own recordings at 100 and %d at 1000; want the same few at both: a call reads every recording its test has not answered", most[100], most[1000])
	}
}
