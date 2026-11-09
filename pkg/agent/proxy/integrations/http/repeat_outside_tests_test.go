package http

import (
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// pooledEarlierDb is an earlierDb whose replay sent a scope table
// (integrations.PoolMembership): pool names the mocks no test lists, known
// whether a table was sent.
type pooledEarlierDb struct {
	earlierDb
	pool  map[string]bool
	known bool
}

func (d pooledEarlierDb) InPool(m *models.Mock) (bool, bool) { return d.pool[m.Name], d.known }

// Under the proven switch, a call identical to a recording made outside
// every test (TestMain's load, listed under no test by the scope table),
// which already answered at this replay, is named as that recording: not as
// an earlier test's. Its next step says why a test is not served it (only a
// call keploy.yaml's sessionCalls declare is), and ends with the hint that
// declares the call, which the note keeps (Hint) so the replay's rewrite of
// the next step keeps it too. A call keploy.yaml declares already is told
// what the declaration serves. The same recording listed under a test, or
// with no table sent, is an earlier test's, as before. Without the switch
// the hint is the switch-off one. A call an entry covers only by a path
// prefix is told through the real manager
// (proxy.TestUnderTheSwitchATestsCallIdenticalToAHiddenStartUpRecordingNamesIt):
// here the answered log is the row's, and the manager's decides which
// recordings it holds.
func TestARepeatOfARecordingMadeOutsideEveryTestNamesItAndHowToServeIt(t *testing.T) {
	const addr, host = "127.0.0.1:18094", "127.0.0.1:18094"
	const path = "/echo/rates"
	rec := time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC)
	startup := onHost("mock-0", path, host)
	startup.Spec.ReqTimestampMock = rec
	startup.Spec.Metadata = map[string]string{models.MetadataLifetimeKey: models.MetadataLifetimePerTest}
	answeredAs := func(background string, skipped bool) []integrations.AnsweredCall {
		return []integrations.AnsweredCall{{Mock: startup, AnsweredAt: time.Now().Add(-time.Second), Background: background, SkippedRead: skipped}}
	}
	// What the store still shows: another call's recording, which states
	// its lifetime as the recorder writes every HTTP mock.
	near := onHost("mock-1", "/echo/quote", host)
	near.Spec.Metadata = map[string]string{models.MetadataLifetimeKey: models.MetadataLifetimePerTest}
	t.Cleanup(func() { models.SetSessionCalls(nil) })
	for _, c := range []struct {
		name     string
		proven   bool
		pool     bool
		known    bool
		declared bool
		outside  bool
		// background and skipped are how the store noted the earlier
		// answer (integrations.AnsweredCall): a background spend or a
		// skipped test's read keeps its own words.
		background string
		skipped    bool
		want       []string
		not        []string
	}{
		{name: "switch on, undeclared, made outside every test", proven: true, pool: true, known: true, outside: true,
			want: []string{"identical to mock-0, a recording made outside every test (in TestMain or at start-up)", "only for a call a keploy.yaml sessionCalls entry names exactly, by its method and whole path", `sessionCalls: [{host: "127.0.0.1:18094", method: "GET", pathPrefix: "/echo/rates"}]`},
			not:  []string{"an earlier test", "No recorded mocks"}},
		{name: "switch on, declared, made outside every test", proven: true, pool: true, known: true, declared: true, outside: true,
			want: []string{"a recording made outside every test", "keploy.yaml declares this call a session call"},
			not:  []string{"an earlier test", "sessionCalls: ["}},
		{name: "switch on, listed under a test", proven: true, pool: false, known: true,
			want: []string{"a recording of an earlier test of this set", "a recording another test made of it is served to that test only"},
			not:  []string{"outside every test (in TestMain or at start-up), which already"}},
		{name: "switch on, no scope table", proven: true, pool: true, known: false,
			want: []string{"a recording of an earlier test of this set"},
			not:  []string{"a recording made outside every test"}},
		{name: "switch on, a spend of the background pool", proven: true, pool: true, known: true, background: models.RepeatBackgroundPool,
			want: []string{"a recording no test of the set lists (the set's pool)"},
			not:  []string{"a recording made outside every test"}},
		{name: "switch on, a skipped test's read", proven: true, pool: true, known: true, skipped: true,
			want: []string{"the read of a test this replay skips"},
			not:  []string{"a recording made outside every test"}},
		{name: "switch off", proven: false, pool: true, known: true,
			want: []string{"a recording of an earlier test of this set", "If another test recorded this call"},
			not:  []string{"a recording made outside every test"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv(provenswitch.Env, map[bool]string{false: "0", true: "1"}[c.proven])
			models.SetSessionCalls(nil)
			if c.declared {
				models.SetSessionCalls([]models.SessionCallRule{{Host: "127.0.0.1", Method: "GET", PathPrefix: path}})
			}
			db := pooledEarlierDb{
				earlierDb: earlierDb{mockMemDb: &mockMemDb{mocks: []*models.Mock{near}}, answered: answeredAs(c.background, c.skipped)},
				pool:      map[string]bool{"mock-0": c.pool}, known: c.known,
			}
			r := missReport(t, db, addr, host, path, models.OutgoingOptions{})
			if r.Repeat == nil || r.Repeat.Mock != "mock-0" {
				t.Fatalf("the miss carries note %+v, want one naming mock-0", r.Repeat)
			}
			if r.Repeat.OutsideTests != c.outside {
				t.Errorf("the note says made outside every test %v, want %v", r.Repeat.OutsideTests, c.outside)
			}
			if r.Repeat.Background != c.background {
				t.Errorf("the note names background %q, want %q: a recording made outside every test moves no charge", r.Repeat.Background, c.background)
			}
			if r.Repeat.Hint == "" || !strings.HasSuffix(r.NextSteps, r.Repeat.Hint) {
				t.Errorf("the next step %q does not end with the note's hint %q", r.NextSteps, r.Repeat.Hint)
			}
			// The replay rewrites the next step from the note alone.
			if again := r.Repeat.Clone().ExplainMiss(r.NotOwned, r.ClosestMock, "", "keploy mock record"); !strings.Contains(again, r.Repeat.Hint) {
				t.Errorf("the replay's next step %q drops the hint %q", again, r.Repeat.Hint)
			}
			for _, w := range c.want {
				if !strings.Contains(r.NextSteps, w) {
					t.Errorf("the next step %q does not say %q", r.NextSteps, w)
				}
			}
			for _, w := range c.not {
				if strings.Contains(r.NextSteps, w) {
					t.Errorf("the next step %q says %q", r.NextSteps, w)
				}
			}
		})
	}
}

// A miss that repeats no earlier answer keeps the session-call hint in its
// next step, as before: the hint is added once, whether or not a repeat
// note takes it over.
func TestAMissWithNoRepeatKeepsTheSessionCallHint(t *testing.T) {
	const addr, host = "127.0.0.1:18094", "127.0.0.1:18094"
	t.Setenv(provenswitch.Env, "1")
	models.SetSessionCalls(nil)
	near := onHost("mock-1", "/echo/quote", host)
	near.Spec.Metadata = map[string]string{models.MetadataLifetimeKey: models.MetadataLifetimePerTest}
	db := pooledEarlierDb{earlierDb: earlierDb{mockMemDb: &mockMemDb{mocks: []*models.Mock{near}}}, pool: map[string]bool{}, known: true}
	r := missReport(t, db, addr, host, "/echo/rates", models.OutgoingOptions{})
	const hint = "If the application made this call outside every test (in TestMain or at start-up)"
	if r.Repeat != nil || strings.Count(r.NextSteps, hint) != 1 {
		t.Fatalf("the miss carries note %+v and next step %q; want no note and the hint once", r.Repeat, r.NextSteps)
	}
}
