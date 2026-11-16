package proxy

import (
	"context"
	"net/http"
	"testing"
	"time"

	httpint "go.keploy.io/server/v3/pkg/agent/proxy/integrations/http"
	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// fullRoundBackgroundRun stages, under the background rules, a declared
// series of three recordings of GET /v1/sys/health (a liveness probe the
// application makes on its own clock), recorded inside test-0's window and
// placed in the set's worker scope as one series, as a background rule
// places them before record's full round; and test-1's own GET /dup, with
// extraAt's recording (when set; start is test-0's window's) in the worker
// scope beside the series. The view is
// between tests, with pairing on, as in record's full round.
func fullRoundBackgroundRun(t *testing.T, extraAt func(start time.Time) *models.Mock) (*Proxy, *MockManager) {
	t.Helper()
	t.Setenv(provenswitch.Env, "1")
	p := startedDrainProxy(t, 100)
	p.learnTracker = learn.NewTracker()
	start := time.Now().Add(time.Second)
	mks := []*models.Mock{
		r3Mock("probe-1", start.Add(100*time.Millisecond), "GET", "/v1/sys/health", "", `{"n":1}`),
		r3Mock("probe-2", start.Add(200*time.Millisecond), "GET", "/v1/sys/health", "", `{"n":2}`),
		r3Mock("probe-3", start.Add(300*time.Millisecond), "GET", "/v1/sys/health", "", `{"n":3}`),
		r3Mock("mock-own-1", start.Add(1100*time.Millisecond), "GET", "/dup", "", `{"test":1}`),
	}
	worker := []string{"probe-1", "probe-2", "probe-3"}
	if extraAt != nil {
		extra := extraAt(start)
		mks = append(mks, extra)
		worker = append(worker, extra.Name)
	}
	mm := NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(mm.Close)
	mm.SeedRecordedWindows([]models.TestWindow{
		{TestCase: "test-0", Start: start, End: start.Add(900 * time.Millisecond)},
		{TestCase: "test-1", Start: start.Add(time.Second), End: start.Add(1900 * time.Millisecond)},
	})
	mm.SetMocksWithWindow(nil, mks, models.BaseTime, start.Add(time.Hour))
	mm.ServeExactBackground(models.RecordedWithS12Routing, nil, worker)
	mm.ServeExactWorker(worker)
	mm.SetPoolSeries(map[string][]string{"liveness/-/1": {"probe-1", "probe-2", "probe-3"}})
	p.setMockManager(mm)
	t.Cleanup(func() { p.setMockManager(nil) })
	mm.ServeExactPool(models.PoolKeyNone, nil)
	mm.SetPairing(true)
	return p, mm
}

// In record's full round, a declared background series answers each call it
// matches, in any test, on its last again once all its recordings answered,
// as a replay serves it: in the test whose window recorded it, past the count
// it recorded, and in a test that recorded none of it, more than once. Its
// recordings are the worker scope's, no test's own, whatever tier shows them
// to the test or window recorded them, so none is spent as the test's own:
// a learn step that took them for the test's own read the series' second
// call in a test as one call more than the test recorded, and missed it.
// Under a full round that runs a test longer than its record (a failing test
// waits out its deadlines), every probe past the first in it missed.
func TestADeclaredSeriesAnswersOnItsLastAgainInTheFullRound(t *testing.T) {
	p, mm := fullRoundBackgroundRun(t, nil)
	h := httpint.New(zap.NewNop())
	opts := models.OutgoingOptions{Learn: true}
	for _, tc := range []struct {
		test string
		own  []*models.Mock
		want []string
	}{
		{"test-0", nil, []string{`{"n":1}`, `{"n":2}`, `{"n":3}`, `{"n":3}`}},
		{"test-1", []*models.Mock{roundMock(mm, "mock-own-1")}, []string{`{"n":3}`, `{"n":3}`, `{"n":3}`}},
	} {
		mm.ServeExactPool(models.PoolKeyNone, nil)
		mm.ServeExactTest(tc.test, tc.own)
		p.ContinueTestErrorCapture()
		for i, want := range tc.want {
			code, body, err := callWithinOpts(h, p, mm, opts, httpGet("/v1/sys/health"), true)
			if err != nil || code != http.StatusOK || body != want {
				t.Fatalf("%s's probe %d got %d %q (%v); want the series' %s", tc.test, i+1, code, body, err, want)
			}
		}
		calls, err := p.GetMockErrors(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if len(calls) != 0 {
			t.Fatalf("%s: reported %+v; want no miss: the series answers every probe", tc.test, calls)
		}
	}
}

// In record's full round, the background never answers a call one more than
// the test recorded: test-1 recorded GET /dup once, and the worker scope
// holds an identical GET /dup beside the declared series. test-1's second
// GET /dup is a miss, one call more than it recorded, never the worker
// scope's copy: the test's own recording stays its own and spent once it
// answered.
func TestInTheFullRoundTheWorkerScopesCopyNeverAnswersAnOwnCallOneMoreThanRecorded(t *testing.T) {
	p, mm := fullRoundBackgroundRun(t, func(start time.Time) *models.Mock {
		// Recorded inside test-1's window, as the call it copies.
		return r3Mock("mock-bg-dup", start.Add(1500*time.Millisecond), "GET", "/dup", "", `{"background":1}`)
	})
	h := httpint.New(zap.NewNop())
	opts := models.OutgoingOptions{Learn: true}
	mm.ServeExactPool(models.PoolKeyNone, nil)
	mm.ServeExactTest("test-1", []*models.Mock{roundMock(mm, "mock-own-1")})
	p.ContinueTestErrorCapture()
	if code, body := r3SendVia(t, context.Background(), h, p, mm, opts, httpGet("/dup")); code != http.StatusOK || body != `{"test":1}` {
		t.Fatalf("test-1's GET /dup got %d %s; want its own mock-own-1", code, body)
	}
	if code, body, err := callWithinOpts(h, p, mm, opts, httpGet("/dup"), true); err == nil && code == http.StatusOK {
		t.Fatalf("test-1's second GET /dup, one more than it recorded, was answered %s; want a miss, never the worker scope's copy", body)
	}
	calls, err := p.GetMockErrors(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || calls[0].Kind != models.MissExtraCall || calls[0].Recorded != 1 {
		t.Fatalf("reported %+v; want one extra_call miss of GET /dup, recorded 1", calls)
	}
}
