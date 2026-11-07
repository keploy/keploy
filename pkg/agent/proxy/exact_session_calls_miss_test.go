package proxy

import (
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// startUpCall is a recording of GET http://deps<path>, answered body, stated
// per-test as the recorder writes every HTTP call, its lifetime derived
// under the session calls installed now.
func startUpCall(name string, at time.Duration, path, body string) *models.Mock {
	m := workerProbe(name, at, path, body)
	m.TestModeInfo.Consume = models.ConsumeReuse
	m.TestModeInfo.Lifetime = models.LifetimePerTest
	m.TestModeInfo.LifetimeDerived = false
	m.Spec.Metadata = map[string]string{"type": models.HTTPClient, models.MetadataLifetimeKey: models.MetadataLifetimePerTest}
	m.DeriveLifetime()
	return m
}

// Under the proven switch, a test's call identical to a recording made
// outside every test (the pool's: TestMain's load, at start-up) that the
// test's view hides misses, and its miss names that recording and why it
// was not served, whatever lifetime the recording has: one a keploy.yaml
// entry covers only by a path prefix or with no method (session, by
// DeriveLifetime), and one no entry covers (per-test). One that answered TestMain's call is named as the recording made
// outside every test that already answered, with the hint that names the
// exact entry to declare; one that answered nothing is named as a recording
// made where the replay did not make it. Never the next step of a set with
// no recording of the call ("No recorded mocks were available ... Re-record
// the mock set"). A call an entry names exactly is served. Each row uses the
// real manager and the real HTTP parser: the answer the start-up recording
// gave is what the manager noted, not what a row says it noted.
func TestUnderTheSwitchATestsCallIdenticalToAHiddenStartUpRecordingNamesIt(t *testing.T) {
	const exact = `declare sessionCalls: [{host: "deps", method: "GET", pathPrefix: "/echo/rates"}]`
	for _, c := range []struct {
		name  string
		rules []models.SessionCallRule
		// unanswered: TestMain does not make the call at this replay.
		unanswered bool
		served     bool
		want, not  []string
		notOwned   bool
	}{
		{name: "declared exactly", rules: []models.SessionCallRule{{Host: "deps", Method: "GET", PathPrefix: "/echo/rates"}}, served: true},
		{name: "declared by a path prefix", rules: []models.SessionCallRule{{Host: "deps", Method: "GET", PathPrefix: "/echo"}},
			want: []string{"identical to mock-0, a recording made outside every test (in TestMain or at start-up), which already answered a call at this replay", "covers this call only by a path prefix or with no method", exact},
			not:  []string{"keploy.yaml declares this call a session call"}},
		{name: "declared with no method", rules: []models.SessionCallRule{{Host: "deps", PathPrefix: "/echo/rates"}},
			want: []string{"identical to mock-0, a recording made outside every test", "covers this call only by a path prefix or with no method", exact}},
		{name: "undeclared",
			want: []string{"identical to mock-0, a recording made outside every test", "If the application made this call outside every test", `(sessionCalls: [{host: "deps", method: "GET", pathPrefix: "/echo/rates"}])`}},
		{name: "declared by a path prefix, unanswered", rules: []models.SessionCallRule{{Host: "deps", Method: "GET", PathPrefix: "/echo"}}, unanswered: true, notOwned: true,
			want: []string{"This request is mock-0"}},
		{name: "undeclared, unanswered", unanswered: true, notOwned: true,
			want: []string{"This request is mock-0"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv(provenswitch.Env, "1")
			declareSessionCalls(t, c.rules...)
			rates := startUpCall("mock-0", 0, "/echo/rates", "R0")
			other := startUpCall("mock-1", 5*time.Second, "/echo/quote", "Q1")
			mm := stagedManager(t, nil, []*models.Mock{rates, other})
			mm.SetPoolNames([]string{"mock-0"})
			mm.ServeExactPool("k", []string{"mock-0"})
			if !c.unanswered {
				if got, r := httpMissCall(t, mm, "/echo/rates"); got != "R0" {
					t.Fatalf("TestMain's call was answered %q (miss %+v); want the start-up recording's R0", got, r)
				}
			}
			// The test that loads the rates again owns no recording of them.
			mm.ServeExactTest("T1", nil)
			got, r := httpMissCall(t, mm, "/echo/rates")
			if c.served {
				if got != "R0" {
					t.Fatalf("the declared call in T1 was answered %q (miss %+v); want the start-up recording's R0", got, r)
				}
				return
			}
			if r == nil {
				t.Fatalf("the call in T1 was answered %q; want a miss", got)
			}
			if r.ClosestMock != "mock-0" || r.NotOwned != c.notOwned {
				t.Fatalf("the miss names %q (not owned %v); want mock-0 (not owned %v): %s", r.ClosestMock, r.NotOwned, c.notOwned, r.NextSteps)
			}
			if !c.notOwned && (r.Repeat == nil || r.Repeat.Mock != "mock-0" || !r.Repeat.OutsideTests) {
				t.Fatalf("the miss carries note %+v; want one naming mock-0 made outside every test", r.Repeat)
			}
			for _, w := range append([]string{"No recorded mocks were available", "Re-record the mock set"}, c.not...) {
				if strings.Contains(r.NextSteps, w) {
					t.Errorf("the next step %q says %q", r.NextSteps, w)
				}
			}
			for _, w := range c.want {
				if !strings.Contains(r.NextSteps, w) {
					t.Errorf("the next step %q does not say %q", r.NextSteps, w)
				}
			}
		})
	}
}

// A start-up recording answers TestMain's calls again and again: the manager
// notes it once, and each answer after that moves only when it last
// answered, so a later test's miss measures its gap from the last answer.
func TestAStartUpRecordingThatAnswersAgainIsNotedOnceAtItsLastAnswer(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	declareSessionCalls(t, models.SessionCallRule{Host: "deps", Method: "GET", PathPrefix: "/echo"})
	rates := startUpCall("mock-0", 0, "/echo/rates", "R0")
	mm := stagedManager(t, nil, []*models.Mock{rates})
	mm.SetPoolNames([]string{"mock-0"})
	mm.ServeExactPool("k", []string{"mock-0"})
	var last time.Time
	for i := 0; i < 3; i++ {
		if got, _ := httpMissCall(t, mm, "/echo/rates"); got != "R0" {
			t.Fatalf("TestMain's call %d was answered %q; want R0", i+1, got)
		}
		last = time.Now()
	}
	mm.answered.mu.Lock()
	n, at := 0, time.Time{}
	for _, q := range mm.answered.queue {
		if q.own() && q.e.mock.Name == "mock-0" {
			n, at = n+1, q.e.at
		}
	}
	mm.answered.mu.Unlock()
	if n != 1 {
		t.Fatalf("three answers of the start-up recording made %d entries; want 1", n)
	}
	if at.After(last) || last.Sub(at) > time.Second {
		t.Fatalf("the entry says it answered at %v; want its last answer, before %v", at, last)
	}
}

// Under the switch an extra call (one more than the test recorded) keeps the
// session-call hint too: once its own recordings are spent, a declared
// call's start-up recording answers it.
func TestAnExtraCallsMissKeepsTheSessionCallHint(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	declareSessionCalls(t)
	own := startUpCall("own", 5*time.Second, "/echo/rates", "R1")
	mm := stagedManager(t, nil, []*models.Mock{own})
	mm.SetPoolNames(nil)
	mm.ServeExactPool("k", nil)
	mm.ServeExactTest("T1", []*models.Mock{own})
	if got, r := httpMissCall(t, mm, "/echo/rates"); got != "R1" {
		t.Fatalf("T1's own call was answered %q (miss %+v); want R1", got, r)
	}
	_, r := httpMissCall(t, mm, "/echo/rates")
	if r == nil || r.MissKind != models.MissExtraCall {
		t.Fatalf("T1's second call missed as %+v; want an extra call", r)
	}
	const hint = `If the application made this call outside every test (in TestMain or at start-up) and every test may make it`
	if !strings.Contains(r.NextSteps, hint) {
		t.Fatalf("the extra call's next step %q does not carry the session-call hint", r.NextSteps)
	}
}

// noted counts the log's entries of the recording named name.
func noted(mm *MockManager, name string) int {
	mm.answered.mu.Lock()
	defer mm.answered.mu.Unlock()
	n := 0
	for _, q := range mm.answered.queue {
		if q.own() && q.e.mock.Name == name {
			n++
		}
	}
	return n
}

// A door that keeps a recording (UpdateUnFilteredMock, MarkMockAsUsed) notes
// it only when a test's view may hide it: under exact serving, one of the
// pool's by the replay's scope table. Without exact serving no view hides
// it, a recording the table does not put in the pool is noted where it is
// served or spent, and an update that filed nothing answered nothing.
func TestAKeptDoorNotesOnlyAPoolRecordingUnderExactServing(t *testing.T) {
	for _, c := range []struct {
		name  string
		exact bool
		pool  []string
		door  func(t *testing.T, mm *MockManager, rates *models.Mock)
		want  int
	}{
		{name: "the pool's, answered", exact: true, pool: []string{"mock-0"}, want: 1},
		{name: "the pool's, marked used", exact: true, pool: []string{"mock-0"}, want: 1,
			door: func(t *testing.T, mm *MockManager, rates *models.Mock) {
				if !mm.MarkMockAsUsed(*rates) {
					t.Fatalf("the start-up recording was not marked used")
				}
			}},
		{name: "no exact serving", pool: []string{"mock-0"}},
		{name: "not the pool's", exact: true, pool: []string{}},
		{name: "an update that files nothing", exact: true, pool: []string{"mock-0"},
			door: func(t *testing.T, mm *MockManager, rates *models.Mock) {
				ghost := *rates
				ghost.TestModeInfo.ID, ghost.TestModeInfo.SortOrder = 99, 99
				next := ghost
				if mm.UpdateUnFilteredMock(&ghost, &next) {
					t.Fatalf("an update of a recording the trees do not hold filed it")
				}
			}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv(provenswitch.Env, "1")
			declareSessionCalls(t, models.SessionCallRule{Host: "deps", Method: "GET", PathPrefix: "/echo"})
			rates := startUpCall("mock-0", 0, "/echo/rates", "R0")
			mm := stagedManager(t, nil, []*models.Mock{rates})
			mm.SetPoolNames(c.pool)
			if c.exact {
				mm.ServeExactPool("k", []string{"mock-0"})
			}
			if c.door != nil {
				c.door(t, mm, rates)
			} else if got, r := httpMissCall(t, mm, "/echo/rates"); got != "R0" {
				t.Fatalf("the call was answered %q (miss %+v); want R0", got, r)
			}
			if n := noted(mm, "mock-0"); n != c.want {
				t.Fatalf("the log holds %d entries of the start-up recording; want %d", n, c.want)
			}
		})
	}
}
