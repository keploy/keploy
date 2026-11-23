package proxy

import (
	"context"
	"net/http"
	"reflect"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	httpint "go.keploy.io/server/v3/pkg/agent/proxy/integrations/http"
	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// The agent's read of a test's misses keeps a parser's background mark
// (MockMismatchReport.Background): the replay lists the call and counts it
// against no test. An unmarked miss stays unmarked.
func TestTheMissReadKeepsTheBackgroundMark(t *testing.T) {
	p := &Proxy{logger: zap.NewNop(), errChannel: make(chan error, 16)}
	p.BeginTestErrorCapture()
	bg := mockMiss("findAndModify ptw_endpoints")
	bg.MismatchReport.Background = true
	bound := models.BackgroundBinding{Recording: "mock-2954", Place: "body.docs.msgs.0.q.app_id", Variable: "appId", BoundMock: "mock-226", BoundPlace: "body.docs.msgs.0.appId"}
	bg.MismatchReport.BackgroundBound = []models.BackgroundBinding{bound}
	p.errChannel <- bg
	p.errChannel <- mockMiss("find users")
	got, err := p.GetMockErrors(context.Background())
	if err != nil || len(got) != 2 {
		t.Fatalf("read %+v, %v; want both misses", got, err)
	}
	for _, c := range got {
		if want := c.ActualSummary == "findAndModify ptw_endpoints"; c.Background != want || c.CountsAgainstTest() == want {
			t.Errorf("%s: Background %v, counts against its test %v", c.ActualSummary, c.Background, c.CountsAgainstTest())
		}
		if want := c.Background; (len(c.BackgroundBound) == 1 && c.BackgroundBound[0] == bound) != want {
			t.Errorf("%s: BackgroundBound %+v; want the parser's places only on the background miss", c.ActualSummary, c.BackgroundBound)
		}
	}
}

// bgRec is a background recording of kind named name, sent at sec seconds.
func bgRec(name string, kind models.Kind, sec int64) models.BackgroundRecording {
	return models.BackgroundRecording{Mock: &models.Mock{Name: name, Kind: kind, Spec: models.MockSpec{ReqTimestampMock: time.Unix(1_700_000_000+sec, 0)}}}
}

// bgNames is the names of recs, in order.
func bgNames(recs []models.BackgroundRecording) []string {
	var out []string
	for _, r := range recs {
		out = append(out, r.Mock.Name)
	}
	return out
}

// The set's background recordings are read by kind, in recorded order, through
// any view (a test's allowlist names none of them), and they follow the
// windows' lifecycle: a seed at a set boundary waits for that set's staging,
// a set that seeds none has none, and an aborted set's park is dropped.
func TestBackgroundRecordingsFollowTheSetsStaging(t *testing.T) {
	mm := NewMockManager(nil, nil, zap.NewNop())
	defer mm.Close()
	if mm.BackgroundRecordings(models.Mongo) != nil {
		t.Fatal("a fresh manager has background recordings")
	}
	mm.SeedBackgroundRecordings([]models.BackgroundRecording{bgRec("poll-2", models.Mongo, 4), bgRec("dns-1", models.DNS, 1), bgRec("poll-1", models.Mongo, 2)})
	if got := bgNames(mm.BackgroundRecordings(models.Mongo)); !reflect.DeepEqual(got, []string{"poll-1", "poll-2"}) {
		t.Fatalf("the Mongo background recordings are %v; want [poll-1 poll-2], in recorded order", got)
	}
	var view integrations.MockMemDb = &scopedMockDb{MockMemDb: mm}
	if got := bgNames(integrations.BackgroundRecordingsOf(view, models.Mongo)); !reflect.DeepEqual(got, []string{"poll-1", "poll-2"}) {
		t.Fatalf("a test's view reads %v; want the set's [poll-1 poll-2] whatever it serves", got)
	}

	mm.ResetForReplaySession()
	mm.SeedBackgroundRecordings([]models.BackgroundRecording{bgRec("b-poll", models.Mongo, 9)})
	if got := bgNames(mm.BackgroundRecordings(models.Mongo)); !reflect.DeepEqual(got, []string{"poll-1", "poll-2"}) {
		t.Fatalf("the next set's seed replaced this set's before it was staged: %v", got)
	}
	mm.SetMocksWithWindow(nil, nil, models.BaseTime, time.Now())
	if got := bgNames(mm.BackgroundRecordings(models.Mongo)); !reflect.DeepEqual(got, []string{"b-poll"}) {
		t.Fatalf("after staging the next set: %v, want [b-poll]", got)
	}

	mm.ResetForReplaySession()
	mm.SetMocksWithWindow(nil, nil, models.BaseTime, time.Now())
	if got := mm.BackgroundRecordings(models.Mongo); got != nil {
		t.Fatalf("a set that seeded none inherited the previous set's %v", bgNames(got))
	}

	mm.ResetForReplaySession()
	mm.SeedBackgroundRecordings([]models.BackgroundRecording{bgRec("d-poll", models.Mongo, 1)})
	mm.ResetForReplaySession() // d's staging was aborted
	mm.SetMocksWithWindow(nil, nil, models.BaseTime, time.Now())
	if got := mm.BackgroundRecordings(models.Mongo); got != nil {
		t.Fatalf("an aborted set's parked background recordings were installed for the next set: %v", bgNames(got))
	}
}

// The agent's read of a test's misses keeps a parser's trail mark
// (MockMismatchReport.TrailsOf): the call carries the named test's kept
// value, and still counts against the tests. An unmarked miss names none.
func TestTheMissReadKeepsTheTrailMark(t *testing.T) {
	p := &Proxy{logger: zap.NewNop(), errChannel: make(chan error, 16)}
	p.BeginTestErrorCapture()
	tr := mockMiss("delete uiTestSetJoins")
	tr.MismatchReport.TrailsOf = "o::TestA"
	p.errChannel <- tr
	p.errChannel <- mockMiss("find users")
	got, err := p.GetMockErrors(context.Background())
	if err != nil || len(got) != 2 {
		t.Fatalf("read %+v, %v; want both misses", got, err)
	}
	for _, c := range got {
		want := ""
		if c.ActualSummary == "delete uiTestSetJoins" {
			want = "o::TestA"
		}
		if c.TrailsOf != want || !c.CountsAgainstTest() {
			t.Errorf("%s: TrailsOf %q, counts against the tests %v; want %q, true", c.ActualSummary, c.TrailsOf, c.CountsAgainstTest(), want)
		}
	}
}

// An HTTP health poll on the application's own timer inside a test window,
// matching a worker: or startup: recording seeded on MockManager, is marked
// Background and counts against no test under both KEPLOY_PROVEN unset and =1.
// A test's own HTTP call (including an extra-call miss) and an unowned call
// matching another test's per-test recording are never marked Background.
func TestHTTPBackgroundPollInsideTestWindowCountsAgainstNoTest(t *testing.T) {
	start := time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC)
	mkHTTP := func(name, method, path, body, resp string, at time.Time) *models.Mock {
		return r3Mock(name, at, method, path, body, resp)
	}
	for _, proven := range []string{"", "1"} {
		mode := "proven=unset"
		if proven == "1" {
			mode = "proven=1"
		}
		t.Run(mode, func(t *testing.T) {
			t.Setenv(provenswitch.Env, proven)
			p := startedDrainProxy(t, 32)
			if proven == "1" {
				p.learnTracker = learn.NewTracker()
			}
			mm := NewMockManager(nil, nil, zap.NewNop())
			t.Cleanup(mm.Close)
			workerHealth := mkHTTP("worker-health-0", "GET", "/v1/sys/health", "", `{"initialized":true}`, start.Add(-5*time.Second))
			startupInit := mkHTTP("startup-init-0", "GET", "/v1/sys/init", "", `{"initialized":true}`, start.Add(-10*time.Second))
			ownSeal := mkHTTP("test-a-seal", "GET", "/v1/sys/seal-status", "", `{"sealed":false}`, start.Add(100*time.Millisecond))
			otherTestKV := mkHTTP("test-b-kv", "GET", "/v1/kv/data/other", "", `{"data":{}}`, start.Add(1100*time.Millisecond))

			if proven == "1" {
				mm.SeedRecordedWindows([]models.TestWindow{
					{TestCase: "test-a", Start: start, End: start.Add(900 * time.Millisecond)},
					{TestCase: "test-b", Start: start.Add(time.Second), End: start.Add(1900 * time.Millisecond)},
				})
				mm.SetMocksWithWindow(nil, []*models.Mock{ownSeal, otherTestKV}, models.BaseTime, start.Add(time.Hour))
				mm.ServeExactPool(models.PoolKeyNone, nil)
				mm.ServeExactTest("test-a", []*models.Mock{roundMock(mm, "test-a-seal")})
			} else {
				// Stage both test-a's and test-b's per-test recordings in
				// MockManager while narrowing the active window to test-a,
				// so otherTestKV is genuinely present as another test's
				// per-test mock (outside test-a's window, not in worker:/startup:).
				mm.SetMocksWithWindow([]*models.Mock{ownSeal, otherTestKV}, nil, start, start.Add(900*time.Millisecond))
			}
			mm.SeedBackgroundRecordings([]models.BackgroundRecording{
				{Mock: startupInit, From: start.Add(-15 * time.Second)},
				{Mock: workerHealth, From: start.Add(-10 * time.Second)},
			})
			p.setMockManager(mm)
			t.Cleanup(func() { p.setMockManager(nil) })
			h := httpint.New(zap.NewNop())
			opts := models.OutgoingOptions{}

			// 1. Health poll on app timer inside test-a's window -> 502, marked Background, does not count.
			p.ContinueTestErrorCapture()
			if code, _ := r3SendVia(t, context.Background(), h, p, mm, opts, httpGet("/v1/sys/health")); code != http.StatusBadGateway {
				t.Fatalf("health poll got %d; want 502 miss", code)
			}
			calls, err := p.GetMockErrors(context.Background())
			if err != nil || len(calls) != 1 || !calls[0].Background || calls[0].CountsAgainstTest() {
				t.Fatalf("health poll misses = %+v, err=%v; want 1 Background miss with CountsAgainstTest=false", calls, err)
			}

			// 2. Startup poll inside test-a's window -> 502, marked Background, does not count.
			p.ContinueTestErrorCapture()
			if code, _ := r3SendVia(t, context.Background(), h, p, mm, opts, httpGet("/v1/sys/init")); code != http.StatusBadGateway {
				t.Fatalf("startup poll got %d; want 502 miss", code)
			}
			calls, err = p.GetMockErrors(context.Background())
			if err != nil || len(calls) != 1 || !calls[0].Background || calls[0].CountsAgainstTest() {
				t.Fatalf("startup poll misses = %+v, err=%v; want 1 Background miss with CountsAgainstTest=false", calls, err)
			}

			// 3. Test-a's own call: first answered 200; second misses and MUST count against test-a.
			p.ContinueTestErrorCapture()
			if code, _ := r3SendVia(t, context.Background(), h, p, mm, opts, httpGet("/v1/sys/seal-status")); code != http.StatusOK {
				t.Fatalf("test-a own call got %d; want 200", code)
			}
			if code, _ := r3SendVia(t, context.Background(), h, p, mm, opts, httpGet("/v1/sys/seal-status")); code != http.StatusBadGateway {
				t.Fatalf("test-a second own call got %d; want 502 miss", code)
			}
			calls, err = p.GetMockErrors(context.Background())
			if err != nil || len(calls) != 1 || calls[0].Background || !calls[0].CountsAgainstTest() {
				t.Fatalf("test-a own extra-call misses = %+v, err=%v; want 1 non-Background miss with CountsAgainstTest=true", calls, err)
			}

			// 4. Call matching test-b's per-test recording (NOT in worker:/startup:) MUST count against test-a.
			p.ContinueTestErrorCapture()
			if code, _ := r3SendVia(t, context.Background(), h, p, mm, opts, httpGet("/v1/kv/data/other")); code != http.StatusBadGateway {
				t.Fatalf("other-test call got %d; want 502 miss", code)
			}
			calls, err = p.GetMockErrors(context.Background())
			if err != nil || len(calls) != 1 || calls[0].Background || !calls[0].CountsAgainstTest() {
				t.Fatalf("other-test per-test call misses = %+v, err=%v; want 1 non-Background miss with CountsAgainstTest=true", calls, err)
			}

			// 5. With no worker:/startup: recordings seeded, health poll MUST count.
			mm.SeedBackgroundRecordings(nil)
			p.ContinueTestErrorCapture()
			if code, _ := r3SendVia(t, context.Background(), h, p, mm, opts, httpGet("/v1/sys/health")); code != http.StatusBadGateway {
				t.Fatalf("unseeded health poll got %d; want 502 miss", code)
			}
			calls, err = p.GetMockErrors(context.Background())
			if err != nil || len(calls) != 1 || calls[0].Background || !calls[0].CountsAgainstTest() {
				t.Fatalf("unseeded health poll misses = %+v, err=%v; want 1 non-Background miss with CountsAgainstTest=true", calls, err)
			}
		})
	}
}
