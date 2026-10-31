package http

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// The shape of api-server's TestAppCRUD (run 5's mock-199): a start-up call
// of the pool (mock-17, a janitor's find with expires_at <= now) reads the
// application's clock half a second before the first test begins, and the
// test's PUT stamps lastUsedAt with its own read, in seconds, in the same
// second of the recording. F, in keploy mock's client tests, runs the test
// late, its clock moved at the begin. The PUT's lastUsedAt is a read of the
// clock in its own call's window in both runs. Where F gave the application
// the start-up call as recorded, or its view shows another second there, the
// value moved and the start-up's did not move with it: the start-up is no
// source, and lastUsedAt is learned as a read of the clock. Where the view
// holds the second this call sent, the start-up and the call read the clock
// in one second in both runs, which keploy cannot tell from a copy; the
// call's own window holds the value in both runs, so it is learned as the
// call's own read of the clock, checked in that window at every replay (the
// rule for a start-up's same-second read). Refused, naming the start-up,
// where that evidence is not whole: keploy has no view of what F's start-up
// sent; the call's own window holds the value in the recording alone; F
// served the start-up's recording in the test too; or the recording made it
// after its test began. So too where the second stands in the start-up's
// answer, which the application was given, F giving it as recorded, whether
// or not its request sent it too: an instant an answer gave is followed whole
// (a founder decision), and a start-up's is no source a test's call may
// follow.
func TestASameSecondClockReadOfAnEarlierCallIsNoSourceWhereThatCallMovedApart(t *testing.T) {
	recBegin := learnAt.Add(800 * time.Millisecond)
	base := learnAt.Add(10 * time.Minute) // F's begin, on the real clock
	late := recBegin.Add(4660 * time.Millisecond)
	before := learnAt.Add(5*time.Second + 300*time.Millisecond).Sub(base.Add(-time.Second)) // F's start-up reads 5.3 s past learnAt
	appClock := func(r time.Time) (time.Time, bool) {
		if r.Before(base) {
			return r.Add(before), true
		}
		return late.Add(r.Sub(base)), true // the begin moved the app's clock
	}
	startUpText := func(sec string) string {
		return `{"find":"branches","filter":{"status":"merged","expires_at":{"$lte":{"$numberLong":"` + sec + `"}}}}`
	}
	startUpOf := func(sec string) *models.Mock {
		return &models.Mock{Name: "mock-17", Kind: models.GENERIC, Spec: models.MockSpec{
			ReqTimestampMock: learnAt.Add(297 * time.Millisecond), ResTimestampMock: learnAt.Add(298 * time.Millisecond),
			GenericRequests: []models.Payload{{Origin: models.FromClient, Message: []models.OutputBinary{{Type: "utf-8", Data: startUpText(sec)}}}}}}
	}
	// The start-up whose answer holds the second: a find that gave the
	// application a branch's expires_at, its filter sending sent.
	answeredOf := func(sent, sec string) *models.Mock {
		m := startUpOf(sent)
		m.Spec.GenericResponses = []models.Payload{{Origin: models.FromServer, Message: []models.OutputBinary{{Type: "utf-8", Data: `{"cursor":{"firstBatch":[{"_id":"b1","expires_at":{"$numberLong":"` + sec + `"}}]}}`}}}}
		return m
	}
	viewOf := func(sec string) func() (*models.Mock, error) {
		return func() (*models.Mock, error) { return startUpOf(sec), nil }
	}
	cases := []struct {
		name   string
		served func(tr *learn.Tracker, m *models.Mock)
		learns bool
		// answer: the start-up's answer holds the second, its request
		// sending sent.
		answer bool
		sent   string
		// put is the second F's PUT sends ("" for the one its own window
		// holds); inTest serves the start-up's recording again in the test,
		// with its view; recBegin moves the recorded test's begin (0: 800ms
		// past learnAt, after the start-up's recording).
		put      string
		inTest   bool
		recBegin time.Duration
		// off: the proven switch is off. rec is the second the start-up
		// and the PUT sent when recorded ("" for 1791972000).
		off bool
		rec string
	}{
		{"served with a view of another second", func(tr *learn.Tracker, m *models.Mock) { tr.ServedMockLive(m, "a", viewOf("1791972004")) }, true, false, "", "", false, 0, false, ""},
		{"served as recorded", func(tr *learn.Tracker, m *models.Mock) { tr.ServedMockAsRecorded(m) }, true, false, "", "", false, 0, false, ""},
		{"served with a view of the second this call sent", func(tr *learn.Tracker, m *models.Mock) { tr.ServedMockLive(m, "a", viewOf("1791972005")) }, true, false, "", "", false, 0, false, ""},
		{"served with no view", func(tr *learn.Tracker, m *models.Mock) { tr.ServedMock(m) }, false, false, "", "", false, 0, false, ""},
		{"served with a view of the second this call sent, which its own window holds in the recording alone", func(tr *learn.Tracker, m *models.Mock) { tr.ServedMockLive(m, "a", viewOf("1791972004")) }, false, false, "", "1791972004", false, 0, false, ""},
		{"served with a view of the second this call sent, and again in the test", func(tr *learn.Tracker, m *models.Mock) { tr.ServedMockLive(m, "a", viewOf("1791972005")) }, false, false, "", "", true, 0, false, ""},
		{"served with a view of the second this call sent, the proven switch off", func(tr *learn.Tracker, m *models.Mock) { tr.ServedMockLive(m, "a", viewOf("1791972005")) }, false, false, "", "", false, 0, true, ""},
		{"an expiry thirty days on, sent in the same second, with a view of the second this call sent", func(tr *learn.Tracker, m *models.Mock) { tr.ServedMockLive(m, "a", viewOf("1794564005")) }, false, false, "", "1794564005", false, 0, false, "1794564000"},
		{"served with a view of the second this call sent, recorded after its test began", func(tr *learn.Tracker, m *models.Mock) { tr.ServedMockLive(m, "a", viewOf("1791972005")) }, false, false, "", "", false, 200 * time.Millisecond, false, ""},
		{"the second in its answer, served as recorded", func(tr *learn.Tracker, m *models.Mock) { tr.ServedMockAsRecorded(m) }, false, true, "0", "", false, 0, false, ""},
		{"the second in its request and its answer, served as recorded", func(tr *learn.Tracker, m *models.Mock) { tr.ServedMockAsRecorded(m) }, false, true, "1791972000", "", false, 0, false, ""},
		{"the second in its request and its answer, with a view of the second this call sent", func(tr *learn.Tracker, m *models.Mock) { tr.ServedMockLive(m, "a", viewOf("1791972005")) }, false, true, "1791972000", "", false, 0, false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, n, opts := learnOn(t)
			if tc.off {
				t.Setenv(provenswitch.Env, "0")
			}
			tr := learn.NewTracker()
			tr.SetClientTests(true)
			tr.SetAppClock(appClock)
			recSec := tc.rec
			if recSec == "" {
				recSec = "1791972000"
			}
			startUp := startUpOf(recSec)
			if tc.answer {
				startUp = answeredOf(tc.sent, recSec)
			}
			tr.ServedMockAt(startUp, base.Add(-time.Second))
			tc.served(tr, startUp)
			if tc.recBegin != 0 {
				tr.RecordedBegin(learnAt.Add(tc.recBegin))
			} else {
				tr.RecordedBegin(recBegin)
			}
			tr.BeginAt(base)
			if tc.inTest {
				tr.ServedMockAt(startUp, base.Add(time.Millisecond))
				tc.served(tr, startUp)
			}
			ctx := learn.WithTracker(context.Background(), tr)
			get := learnRecorded("mock-196", 817257*time.Microsecond, "GET", "/v1/secret/data/pat/e2e-go-key", "", nil,
				`{"data":{"data":{"lastUsedAt":0,"name":"e2e-go"},"metadata":{"version":1}}}`)
			get.Spec.ResTimestampMock = learnAt.Add(817624 * time.Microsecond)
			put := learnRecorded("mock-199", 817800*time.Microsecond, "PUT", "/v1/secret/data/pat/e2e-go-key",
				`{"data":{"lastUsedAt":`+recSec+`,"name":"e2e-go"},"options":{"cas":1}}`, nil, `{"data":{"version":2}}`)
			db := &learnDb{perTest: []*models.Mock{get, put}, pool: map[time.Time]bool{startUp.Spec.ReqTimestampMock: true}, staged: []*models.Mock{startUp}}
			in := live("GET", "/v1/secret/data/pat/e2e-go-key", "", nil)
			in.arrived = base.Add(2 * time.Millisecond)
			if got, miss, err := h.learnMatch(ctx, in, db, n, opts); err != nil || got == nil {
				t.Fatalf("the GET: served %v miss %+v err %v", got != nil, miss, err)
			}
			putSec := tc.put
			if putSec == "" {
				putSec = "1791972005"
			}
			in = live("PUT", "/v1/secret/data/pat/e2e-go-key", `{"data":{"lastUsedAt":`+putSec+`,"name":"e2e-go"},"options":{"cas":1}}`, nil)
			in.arrived = base.Add(3 * time.Millisecond)
			got, miss, err := h.learnMatch(ctx, in, db, n, opts)
			if err != nil {
				t.Fatal(err)
			}
			if tc.learns && (got == nil || !learnedClockRead(db.consumed[len(db.consumed)-1].Spec, "body.data.lastUsedAt")) {
				t.Fatalf("served %v miss %+v; want body.data.lastUsedAt learned as a read of the clock", got != nil, miss)
			}
			if !tc.learns && (got != nil || miss == nil || !strings.Contains(miss.res.Why, "body.data.lastUsedAt carries a value the test's recording holds earlier (in mock-17)")) {
				t.Fatalf("served %v miss %+v; want it refused, naming mock-17", got != nil, miss)
			}
		})
	}
}

// A value test A's call sent where its learn step learned a plain read of the
// clock (a vault PUT's lastUsedAt), which test B's call sends in the same
// second, is B's own read of the clock where nothing else explains it, and
// refused where B's answer echoes it (B's POST stamps createdAt with its own
// clock and its answer gives it back): keploy cannot tell that from a copy.
// Either way nothing binds the run's variable at A's clock read, so A's
// recording is still served at a replay: followed, A's place would carry a
// variable and a clock read at once, which the HTTP replay refuses for test A
// too, in every replay.
func TestLearnMatchASameSecondEchoedCopyOfAnEarlierTestsClockReadLeavesItServed(t *testing.T) {
	lead := learn.FullRoundLead
	for _, echoed := range []bool{false, true} {
		h, n, opts := learnOn(t)
		// keploy mock's client tests: each test's first call has a window
		// from its begin, in the recording and in F, on F's clock.
		tr := learn.NewTracker()
		tr.SetClientTests(true)
		tr.SetAppClock(func(r time.Time) (time.Time, bool) { return r, true })
		tr.NewSet()
		ctx := learn.WithTracker(context.Background(), tr)
		boot := learnRecorded("mock-0", 0, "GET", "/v1/health", "", nil, `{}`)
		tr.ServedMockAt(boot, learnAt.Add(lead))
		tr.ServedMockAsRecorded(boot)
		tr.RecordedBegin(learnAt.Add(time.Second))
		tr.BeginAt(learnAt.Add(lead + time.Second))
		recA := learnRecorded("mock-a", 1100*time.Millisecond, "PUT", "/v1/secret/tokens", `{"data":{"lastUsedAt":1791972001}}`, nil, `{}`)
		cpA := *recA
		a := &learnDb{perTest: []*models.Mock{&cpA}}
		in := live("PUT", "/v1/secret/tokens", `{"data":{"lastUsedAt":1791972062}}`, nil)
		in.arrived = learnAt.Add(lead + 1100*time.Millisecond)
		if served, miss, err := h.learnMatch(ctx, in, a, n, opts); err != nil || served == nil || miss != nil || !learnedClockRead(a.consumed[0].Spec, "body.data.lastUsedAt") {
			t.Fatalf("echoed %v: test A's call: served %v miss %+v err %v; want lastUsedAt learned as a read of the clock", echoed, served, miss, err)
		}
		tr.End()
		tr.RecordedBegin(learnAt.Add(1300 * time.Millisecond))
		tr.BeginAt(learnAt.Add(lead + 1300*time.Millisecond))
		answer := `{}`
		if echoed {
			answer = `{"app":{"createdAt":1791972001}}`
		}
		recB := learnRecorded("mock-1", 1400*time.Millisecond, "POST", "/v1/apps", `{"createdAt":1791972001}`, nil, answer)
		cpB := *recB
		db := &learnDb{perTest: []*models.Mock{&cpB}}
		in = live("POST", "/v1/apps", `{"createdAt":1791972062}`, nil)
		in.arrived = learnAt.Add(lead + 1400*time.Millisecond)
		served, miss, err := h.learnMatch(ctx, in, db, n, opts)
		const why = "body.createdAt changed, and its recorded value was sent in mock-a at body.data.lastUsedAt, an earlier test's call, where keploy learned it as a read of the application's clock, and this call does not hold it as its own read of the clock"
		switch {
		case err != nil:
			t.Fatal(err)
		case echoed && (served != nil || miss == nil || !strings.Contains(miss.res.Why, why) || strings.Contains(miss.res.Why, "179197200")):
			t.Fatalf("echoed: test B served %v miss %+v; want it refused: %s, never the value", served != nil, miss, why)
		case !echoed && (served == nil || !learnedClockRead(db.consumed[0].Spec, "body.createdAt")):
			t.Fatalf("test B served %v miss %+v; want createdAt learned as its own read of the clock", served != nil, miss)
		}
		// What the replay serves: the learned blocks onto the recordings,
		// after the run's variables take their places (learn.RunTakesPlaces).
		recs := learnedOntoTaking([]*models.Mock{recA, recB}, append(append([]models.Mock{}, a.consumed...), db.consumed...), map[string]string{"mock-a": "TestA", "mock-1": "TestB"})
		if len(recs[0].Spec.Variables) != 0 {
			t.Fatalf("echoed %v: test A's PUT binds %+v; want nothing bound at its clock read", echoed, recs[0].Spec.Variables)
		}
		base := learnAt.Add(time.Hour)
		r := live("PUT", "/v1/secret/tokens", `{"data":{"lastUsedAt":`+strconv.FormatInt(base.Unix(), 10)+`}}`, nil)
		r.arrived = base.Add(100 * time.Millisecond)
		if ok, _, diag := matchVarIn(t, clockConn(true, base), newHTTP(), &varDb{perTest: []*models.Mock{recs[0]}}, r); !ok {
			t.Fatalf("echoed %v: test A's recording is not served at a replay: %+v", echoed, diag)
		}
	}
}
