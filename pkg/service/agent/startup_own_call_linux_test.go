//go:build linux

package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// statedRates is GET /rates recorded once by a recorder that states its
// HTTP calls' lifetimes (spec.metadata.lifetime: per-test), at t0: a call a
// keploy.yaml sessionCalls entry can declare (models.StartupDeclarable).
func statedRates(name string) *models.Mock {
	m := pingMock(name, "/rates", "rates", t0)
	m.Spec.Metadata = map[string]string{"type": string(models.HTTPClient), models.MetadataLifetimeKey: models.MetadataLifetimePerTest}
	return m
}

// startupOwnAgent is a replaying agent, under the proven switch, whose set
// recorded GET /rates once outside every test (mock-0, its startup: entry,
// in the pool, its lifetime stated: statedRates) and TestReload's own
// GET /b. o.worker places mock-0 in the worker scope: a startup: true
// rule's recording, or one learned between tests. o.declared names
// GET /rates exactly in keploy.yaml sessionCalls. o.noBoot leaves out the
// app's start-up call (GET /rates, before any test), which else is made,
// and answered, before TestReload begins.
func startupOwnAgent(t *testing.T, o startupOwn) (*Agent, *servingProxy) {
	t.Helper()
	t.Setenv(provenswitch.Env, "1")
	rates := statedRates("mock-0")
	if o.declared {
		t.Cleanup(func() { models.SetSessionCalls(nil) })
		models.SetSessionCalls([]models.SessionCallRule{{Host: "127.0.0.1:18093", Method: "GET", PathPrefix: "/rates"}})
	}
	a, p := unclaimedExactAgent(t, []*models.Mock{
		rates,
		pingMock("mock-1", "/b", "b", t0.Add(2)),
	}, map[string][]string{"o::TestReload": {"mock-1"}}, map[string][]string{"o": {"mock-0"}})
	// The real proxy hands the manager the scope table's pool membership
	// (Proxy.SetPoolNames), by which a miss says a recording was made
	// outside every test (models.RepeatNote.OutsideTests).
	a.Proxy = poolNamingProxy{p}
	ctx := context.Background()
	require.NoError(t, a.SetScopePool(ctx, map[string][]string{"o": {"mock-0"}}))
	if o.worker {
		require.NoError(t, a.SetScopeWorker(ctx, map[string][]string{"o": {"mock-0"}}))
	}
	require.NoError(t, a.SetScopeBackground(ctx, []string{"mock-0"}))
	claimSelf(t, a)
	if !o.noBoot {
		code, body := p.call(t, "/rates")
		require.Equal(t, 200, code, "the start-up call was not answered outside every test: %s", body)
	}
	return a, p
}

// startupOwn is how startupOwnAgent's set covers mock-0, and whether the app
// makes its start-up call.
type startupOwn struct{ worker, declared, noBoot bool }

// poolNamingProxy is servingProxy with the real proxy's pool membership
// (Proxy.SetPoolNames).
type poolNamingProxy struct{ *servingProxy }

func (p poolNamingProxy) SetPoolNames(names []string, owners map[string]string, tests map[string][]string) error {
	p.mm.SetPoolOwners(owners)
	p.mm.SetPoolTests(tests)
	p.mm.SetPoolNames(names)
	return nil
}

// Under the proven switch a test's call identical to a plain startup:
// recording (made before the first test, covered by no declared rule) is
// the test's own call, as it was before keploy mock replay served the set's
// startup background (v3.7.17): TestReload is not served mock-0 again, its
// miss counts against it and fails it, and the miss says mock-0 was made
// outside every test (models.RepeatNote.OutsideTests), whose next step is
// to declare the call in keploy.yaml sessionCalls. It is never a background
// miss of the test. Mutation: serve the plain startup recording in the test,
// keep its pool scope on the repeat, or mark the call a background miss.
func TestMockReplayPlainStartupIsTheTestsOwnCallUnderTheSwitch(t *testing.T) {
	a, p := startupOwnAgent(t, startupOwn{})
	got, e := runTest(t, a, p, "TestReload", "/b", "/rates")
	require.Equal(t, "200 b", got[0])
	require.True(t, strings.HasPrefix(got[1], "502"), "TestReload was served the plain startup recording: %s", got[1])
	require.Equal(t, models.ScopeActionFail, e.Action, "TestReload's own call of the plain startup recording did not fail it")
	require.Contains(t, e.Reason, "GET /rates", "TestReload's failure does not name the call: %s", e.Reason)
	ev := evidenceOf(t, a, "o::TestReload")
	require.Len(t, ev.Misses, 1, "the call is not TestReload's miss")
	m := ev.Misses[0]
	require.Equal(t, "GET /rates", m.ActualSummary)
	require.False(t, m.Background, "the call is a background miss")
	require.True(t, m.CountsAgainstTest())
	require.NotNil(t, m.Repeat, "the miss names no earlier answer")
	require.Equal(t, "mock-0", m.Repeat.Mock)
	require.Empty(t, m.Repeat.Background, "the miss is told it repeats the set's background")
	require.True(t, m.Repeat.OutsideTests, "the miss is not told mock-0 was made outside every test")
	require.Equal(t, "mock-0", m.ClosestMock, "the miss does not name the recording")
	require.Contains(t, m.NextSteps, "made outside every test", "the miss's next step does not say mock-0 was made outside every test: %s", m.NextSteps)
	require.Contains(t, m.NextSteps, "keploy.yaml sessionCalls", "the miss's next step does not say how to declare the call: %s", m.NextSteps)
	require.Empty(t, ev.Background)
}

// A recording made before the first test that a startup: true rule placed
// in worker:, or one learned between tests (worker:), is the background's
// in a test's window too: TestReload passes, and nothing of the call is
// evidence against it. Mutation: treat a worker: recording as a plain
// startup one.
func TestMockReplayWorkerStartupStaysTheBackgroundsUnderTheSwitch(t *testing.T) {
	a, p := startupOwnAgent(t, startupOwn{worker: true})
	got, e := runTest(t, a, p, "TestReload", "/b", "/rates")
	require.Equal(t, "200 b", got[0])
	require.NotEqual(t, models.ScopeActionFail, e.Action, "a worker: recording's call failed TestReload: %s (%v)", e.Reason, got)
	ws, err := a.GetScopeWindows(context.Background())
	require.NoError(t, err)
	for _, w := range ws {
		if w.Name == "o::TestReload" && w.Evidence != nil {
			require.Empty(t, w.Evidence.Misses, "a worker: recording's call is TestReload's miss")
		}
	}
}

// The set's plain startup: mocks, under the switch, are its background less
// every key's worker scope; with the switch off none is
// (plainStartupNames). backgroundRecordingsOf marks those it is handed under
// the switch alone, and never one a keploy.yaml sessionCalls entry names
// exactly (TestMockReplayDeclaredStartupCallIsServedToTheTestUnderTheSwitch).
// Mutation: name a worker-scope recording plain startup, or mark one with
// the switch off.
func TestPlainStartupNamesAreTheBackgroundLessTheWorkerScope(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	got := plainStartupNames([]string{"w1", "s1", "", "s2"}, map[string][]string{"o": {"w1"}, models.PoolKeyNone: {"s2"}})
	require.Equal(t, []string{"s1"}, got)
	s1 := pingMock("s1", "/s1", "s", t0)
	s1.Spec.Metadata = map[string]string{"type": string(models.HTTPClient), models.MetadataLifetimeKey: models.MetadataLifetimePerTest}
	mocks := [][]*models.Mock{{s1, pingMock("w1", "/w1", "w", t0.Add(1))}}
	flags := func() map[string]bool {
		recs, err := backgroundRecordingsOf([]string{"s1", "w1"}, []string{"s1"}, mocks, nil)
		require.NoError(t, err)
		out := map[string]bool{}
		for _, r := range recs {
			out[r.Mock.Name] = r.Startup
		}
		return out
	}
	require.Equal(t, map[string]bool{"s1": true, "w1": false}, flags())

	// An HTTP recording whose recorder stated no lifetime is never marked:
	// no sessionCalls entry can declare it.
	recs, err := backgroundRecordingsOf([]string{"u1"}, []string{"u1"}, [][]*models.Mock{{pingMock("u1", "/u1", "u", t0)}}, nil)
	require.NoError(t, err)
	require.Len(t, recs, 1)
	require.False(t, recs[0].Startup, "an HTTP recording with no stated lifetime was marked plain startup")

	// Nor is one that names an AWS JSON API's action (X-Amz-Target, its
	// name in any case): no sessionCalls entry can narrow by it.
	aws := pingMock("a1", "/", "a", t0)
	aws.Spec.Metadata = map[string]string{"type": string(models.HTTPClient), models.MetadataLifetimeKey: models.MetadataLifetimePerTest}
	aws.Spec.HTTPReq.Header["x-amz-target"] = "AmazonSQS.ReceiveMessage"
	recs, err = backgroundRecordingsOf([]string{"a1"}, []string{"a1"}, [][]*models.Mock{{aws}}, nil)
	require.NoError(t, err)
	require.Len(t, recs, 1)
	require.False(t, recs[0].Startup, "an HTTP recording naming an AWS JSON API's action was marked plain startup")

	// A recording of a kind no one can declare (Mongo, until a rule system
	// registers it) is never marked.
	base := time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC)
	mongoFlag := func() bool {
		recs, err := backgroundRecordingsOf([]string{"m1"}, []string{"m1"}, [][]*models.Mock{{mongoIndexMock("m1", "find", "rates", "boot", base)}}, nil)
		require.NoError(t, err)
		require.Len(t, recs, 1)
		return recs[0].Startup
	}
	require.False(t, mongoFlag(), "a Mongo recording was marked plain startup with no rule system to declare its call")
	unregister := models.RegisterStartupDeclarableKinds(models.Mongo)
	require.True(t, mongoFlag(), "a Mongo recording was not marked plain startup once Mongo was registered")
	unregister()
	require.False(t, mongoFlag(), "a Mongo recording was marked plain startup after its registration was undone")

	t.Setenv(provenswitch.Env, "")
	require.Nil(t, plainStartupNames([]string{"s1"}, nil), "with the switch off a recording was named plain startup")
	require.Equal(t, map[string]bool{"s1": false, "w1": false}, flags(), "with the switch off a recording keploy test named plain startup was marked")
}

// Under the proven switch a plain startup: recording is a test's own call
// only where its user can declare the call (models.StartupDeclarable). Of a
// kind no one can declare (Mongo with no rule system registered) it stays
// the background's in a test's window, as in v3.7.18: the window shows it,
// a by-kind answer reads it, and a call in the test reads it as background.
// Once Mongo is registered (enterprise's startup: true rules), it is the
// test's own call there. With the switch off it is the background's either
// way. keploy test's staging (StartupMocks) and keploy mock replay's mark it
// alike (backgroundRecordingsOf).
// Mutation: mark a recording whatever its kind, or ignore a registration.
func TestAPlainStartupRecordingIsATestsOwnCallOnlyWhereItsCallCanBeDeclared(t *testing.T) {
	base := time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name     string
		proven   string
		register bool
		own      bool
	}{
		{"switch on, Mongo not declarable", "1", false, false},
		{"switch on, Mongo registered", "1", true, true},
		{"switch off, Mongo not declarable", "", false, false},
		{"switch off, Mongo registered", "", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			a, _ := wireAgent(t, models.MODE_TEST)
			t.Setenv(provenswitch.Env, tc.proven)
			if tc.register {
				t.Cleanup(models.RegisterStartupDeclarableKinds(models.Mongo))
			}
			p := newServingProxy()
			a.Proxy = p
			require.NoError(t, a.StoreMocks(ctx, nil, []*models.Mock{
				mongoIndexMock("mock-boot", "find", "rates", "from-boot", base),
				mongoIndexMock("mock-own", "find", "users", "from-own", base.Add(2*time.Second)),
			}))
			require.NoError(t, a.UpdateMockParams(ctx, models.MockFilterParams{
				AfterTime: models.BaseTime, BeforeTime: time.Now(), StrictMockWindow: true,
				FirstRecordedTestStart: base.Add(time.Second),
				BackgroundMocks:        []string{"mock-boot"},
				StartupMocks:           []string{"mock-boot"},
				Exact:                  &models.ExactServing{Stage: true, Background: models.RecordedWithS12Routing, Pool: []string{"mock-boot"}, Order: []string{"test-1"}},
			}))
			require.NoError(t, a.UpdateMockParams(ctx, models.MockFilterParams{Exact: &models.ExactServing{Test: "test-1", Owned: []string{"mock-own"}}}))

			recs := p.mm.BackgroundRecordings(models.Mongo)
			require.Len(t, recs, 1)
			require.Equal(t, tc.own, recs[0].Startup, "the agent's plain startup mark")
			want := []string{"mock-boot"}
			if tc.own {
				want = []string{}
			}
			if tc.proven == "1" {
				// What test-1's window shows; with the switch off keploy
				// test's window is served by today's rules, which read no
				// mark.
				require.Equal(t, want, namesOf(p.mm.Background(models.Mongo)), "test-1's window's background")
				unused, spent := p.mm.KindBackground(models.Mongo)
				require.Equal(t, want, namesOf(unused), "test-1's window's background by kind")
				require.Empty(t, spent)
				if tc.own {
					require.False(t, p.mm.SpendByKind(recs[0].Mock), "a test's call spent the plain startup recording by kind")
				}
			}
			inTest := integrations.NewCallRound(p.mm.StagingEpoch(), nil).InView(true, true)
			var read []string
			for _, r := range p.mm.BackgroundRecordingsIn(inTest, models.Mongo) {
				read = append(read, r.Mock.Name)
			}
			if tc.own {
				require.Empty(t, read, "a call in test-1 reads the plain startup recording as background")
			} else {
				require.Equal(t, []string{"mock-boot"}, read, "a call in test-1 does not read the recording as background")
			}
		})
	}
}

// The app's start-up call made only inside a test (none before the first
// test), identical to a plain startup: recording, is the test's own call
// too: TestReload is not served the unspent recording, and its miss counts
// against it and fails it. It is no background miss, and it repeats no
// earlier answer: its next step names mock-0 as made outside every test and
// says how to declare the call, never as a recording made elsewhere.
// Mutation: show the unspent plain startup recording in the test's window,
// read the call as background, or name it a recording made elsewhere.
func TestMockReplayPlainStartupCallMadeOnlyInATestIsItsOwnCall(t *testing.T) {
	a, p := startupOwnAgent(t, startupOwn{noBoot: true})
	got, e := runTest(t, a, p, "TestReload", "/b", "/rates")
	require.Equal(t, "200 b", got[0])
	require.True(t, strings.HasPrefix(got[1], "502"), "TestReload was served the unspent plain startup recording: %s", got[1])
	require.Equal(t, models.ScopeActionFail, e.Action, "TestReload's own call of the plain startup recording did not fail it")
	require.Contains(t, e.Reason, "GET /rates", "TestReload's failure does not name the call: %s", e.Reason)
	ev := evidenceOf(t, a, "o::TestReload")
	require.Len(t, ev.Misses, 1, "the call is not TestReload's miss")
	m := ev.Misses[0]
	require.Equal(t, "GET /rates", m.ActualSummary)
	require.False(t, m.Background, "the call is a background miss")
	require.True(t, m.CountsAgainstTest())
	require.Equal(t, models.MissInTest, m.Kind)
	require.Equal(t, "mock-0", m.ClosestMock, "the miss does not name the recording")
	require.False(t, m.NotOwned, "the miss names the plain startup recording as one made elsewhere")
	require.Contains(t, m.NextSteps, "made outside every test", "the miss's next step does not say mock-0 was made outside every test: %s", m.NextSteps)
	require.Contains(t, m.NextSteps, "sessionCalls", "the miss's next step does not say how to declare the call: %s", m.NextSteps)
	require.NotContains(t, m.NextSteps, "worker scope", "the miss's next step gives a recording made elsewhere's advice: %s", m.NextSteps)
	require.Empty(t, ev.Background)
}

// A recording made before the first test that a keploy.yaml sessionCalls
// entry names exactly is declared (rule (a)): it is never a plain startup
// one (backgroundRecordingsOf), and every test is served it under the
// switch, as v3.7.17 did. TestReload passes, and nothing of the call is
// evidence against it.
// Mutation: mark a declared session call's recording plain startup.
func TestMockReplayDeclaredStartupCallIsServedToTheTestUnderTheSwitch(t *testing.T) {
	a, p := startupOwnAgent(t, startupOwn{declared: true})
	got, e := runTest(t, a, p, "TestReload", "/b", "/rates")
	require.Equal(t, []string{"200 b", "200 rates"}, got, "TestReload was not served the declared start-up recording")
	require.NotEqual(t, models.ScopeActionFail, e.Action, "a declared call failed TestReload: %s", e.Reason)
	ws, err := a.GetScopeWindows(context.Background())
	require.NoError(t, err)
	for _, w := range ws {
		if w.Name == "o::TestReload" && w.Evidence != nil {
			require.Empty(t, w.Evidence.Misses, "a declared call is TestReload's miss")
		}
	}
}

// keploy test's path stages the set by its filter parameters alone
// (UpdateMockParams, StartupMocks from mappings.yaml), with no scope table:
// the proxy knows no pool names (integrations.InPool). A test's call of its
// plain startup: recording, after the app's start-up call spent it between
// tests, is still the test's own miss, and its note says the recording was
// made outside every test, whose next step declares the call in keploy.yaml
// sessionCalls (the finder says so: integrations.AnsweredCall.OutsideTests).
// So too in a set served by today's rules, which shows the pool in no test.
// A recording keploy test names in no StartupMocks (covered: a declared or
// worker: one) stays the background's: its repeat is a background miss; so
// does one whose lifetime its recorder did not state, and one that names an
// AWS JSON API's action (X-Amz-Target), which no sessionCalls entry can
// declare (models.StartupDeclarable), as in v3.7.18.
// Mutation: drop the finder's outside-tests flag or the note's read of it,
// ask the set's background rules for the mark, or mark an undeclarable
// recording (no stated lifetime, an AWS JSON API's action).
func TestKeployTestPlainStartupRepeatIsTheTestsOwnMissNamingSessionCalls(t *testing.T) {
	for _, tc := range []struct {
		name    string
		startup []string
		// rules is the set's background rules: s12-routing, or today's
		// (""), which shows the pool in no test.
		rules string
		// unstated leaves mock-0's lifetime unstated, as a recorder before
		// lifetimes did: no sessionCalls entry can declare it.
		unstated bool
		// awsTarget gives mock-0's request an X-Amz-Target header (an AWS
		// JSON API's action), which no sessionCalls entry can narrow by.
		awsTarget bool
		own       bool
	}{
		{name: "plain", startup: []string{"mock-0"}, rules: models.RecordedWithS12Routing, own: true},
		{name: "plain, today's rules", startup: []string{"mock-0"}, own: true},
		{name: "covered", rules: models.RecordedWithS12Routing},
		{name: "plain, lifetime unstated", startup: []string{"mock-0"}, rules: models.RecordedWithS12Routing, unstated: true},
		{name: "plain, AWS JSON API action", startup: []string{"mock-0"}, rules: models.RecordedWithS12Routing, awsTarget: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			a, _ := wireAgent(t, models.MODE_TEST)
			t.Setenv(provenswitch.Env, "1")
			p := newServingProxy()
			a.Proxy = p
			rates := statedRates("mock-0")
			if tc.unstated {
				rates = pingMock("mock-0", "/rates", "rates", t0)
			}
			if tc.awsTarget {
				rates.Spec.HTTPReq.Header["X-Amz-Target"] = "DynamoDB_20120810.DescribeTable"
			}
			require.NoError(t, a.StoreMocks(ctx, nil, []*models.Mock{
				rates,
				pingMock("mock-1", "/b", "b", t0.Add(2*time.Second)),
			}))
			require.NoError(t, a.UpdateMockParams(ctx, models.MockFilterParams{
				AfterTime: models.BaseTime, BeforeTime: time.Now(), StrictMockWindow: true,
				FirstRecordedTestStart: t0.Add(time.Second),
				BackgroundMocks:        []string{"mock-0"},
				StartupMocks:           tc.startup,
				Exact:                  &models.ExactServing{Stage: true, Background: tc.rules, Pool: []string{"mock-0"}, Order: []string{"test-1"}},
			}))
			callRates := func() (int, string) {
				if tc.awsTarget {
					return p.callWith(t, "/rates", "X-Amz-Target: DynamoDB_20120810.DescribeTable\r\n")
				}
				return p.call(t, "/rates")
			}
			code, body := callRates()
			require.Equal(t, 200, code, "the start-up call was not answered between tests: %s", body)
			require.NoError(t, a.UpdateMockParams(ctx, models.MockFilterParams{Exact: &models.ExactServing{Test: "test-1", Owned: []string{"mock-1"}}}))
			code, body = p.call(t, "/b")
			require.Equal(t, 200, code, "test-1's own call: %s", body)
			code, body = callRates()
			require.Equal(t, 502, code, "test-1 was answered the spent start-up recording: %s", body)
			misses, err := p.GetMockErrors(ctx)
			require.NoError(t, err)
			var m *models.UnmatchedCall
			for i := range misses {
				if misses[i].ActualSummary == "GET /rates" {
					m = &misses[i]
				}
			}
			require.NotNil(t, m, "no miss of GET /rates in %+v", misses)
			require.NotNil(t, m.Repeat, "the miss names no earlier answer")
			require.Equal(t, "mock-0", m.Repeat.Mock)
			if !tc.own {
				require.Equal(t, models.RepeatBackgroundPool, m.Repeat.Background, "a covered or undeclarable recording's repeat is not the background's")
				require.False(t, m.Repeat.OutsideTests)
				require.True(t, m.Background, "a covered or undeclarable recording's repeat is not a background miss")
				require.False(t, m.CountsAgainstTest())
				return
			}
			require.Empty(t, m.Repeat.Background, "the plain startup recording's repeat is told it is the set's background")
			require.True(t, m.Repeat.OutsideTests, "keploy test's miss is not told mock-0 was made outside every test")
			require.Contains(t, m.NextSteps, "made outside every test", "keploy test's next step does not say mock-0 was made outside every test: %s", m.NextSteps)
			require.Contains(t, m.NextSteps, "keploy.yaml sessionCalls", "keploy test's next step does not say how to declare the call: %s", m.NextSteps)
			require.NotEmpty(t, m.Repeat.Hint, "keploy test's note keeps no sessionCalls entry to declare")
			require.False(t, m.Background, "the call is a background miss")
			require.True(t, m.CountsAgainstTest(), "the call does not count against test-1")
		})
	}
}

// The staging marks the set's plain startup: recordings by the worker scope
// it read. A scope table that then places mock-0 in worker: (a startup: true
// rule's recording) stages the set again before it is served, so mock-0 is
// no longer marked, and TestReload's call of it fails nothing.
// Mutation: keep the staging when the worker scope changes the marks.
func TestAWorkerScopeThatChangesThePlainStartupNamesStagesTheSetAgain(t *testing.T) {
	a, p := startupOwnAgent(t, startupOwn{})
	recs := p.mm.BackgroundRecordings(models.HTTP)
	require.Len(t, recs, 1)
	require.True(t, recs[0].Startup, "the staging did not mark mock-0 plain startup")
	require.NoError(t, a.SetScopeWorker(context.Background(), map[string][]string{"o": {"mock-0"}}))
	got, e := runTest(t, a, p, "TestReload", "/b", "/rates")
	require.Equal(t, "200 b", got[0])
	recs = p.mm.BackgroundRecordings(models.HTTP)
	require.Len(t, recs, 1)
	require.False(t, recs[0].Startup, "mock-0 is still marked plain startup once the worker scope places it")
	require.NotEqual(t, models.ScopeActionFail, e.Action, "a worker: recording's call failed TestReload: %s (%v)", e.Reason, got)
}

// A call that repeats a spent background recording in a test's window (a
// worker: recording's second call there, its one answer in the windows
// given) is charged to no test and fails none, and is listed in the test's
// evidence as its background miss (ScopeEvidence.Background), naming the
// recording, as a background miss the proxy names no repeat of is.
// Mutation: leave a background repeat out of the test's evidence.
func TestABackgroundRepeatInATestsWindowIsListedAsItsBackgroundMiss(t *testing.T) {
	a, p := startupOwnAgent(t, startupOwn{worker: true})
	got, e := runTest(t, a, p, "TestReload", "/b", "/rates", "/rates")
	require.Equal(t, []string{"200 b", "200 rates"}, got[:2])
	require.True(t, strings.HasPrefix(got[2], "502"), "the worker: recording answered twice in the windows: %s", got[2])
	require.NotEqual(t, models.ScopeActionFail, e.Action, "a background repeat failed TestReload: %s", e.Reason)
	ev := evidenceOf(t, a, "o::TestReload")
	require.Empty(t, ev.Misses, "a background repeat is TestReload's miss")
	require.Len(t, ev.Background, 1, "the background repeat is not listed in TestReload's evidence")
	require.Equal(t, "GET /rates", ev.Background[0].ActualSummary)
	require.NotNil(t, ev.Background[0].Repeat)
	require.Equal(t, models.RepeatBackgroundWorker, ev.Background[0].Repeat.Background)
}

// A background repeat read at a test's end that was made outside the test's
// window (here, one the proxy stamped before the test began) is charged to no
// test and is not listed in that test's evidence: only one made in its
// window is its background miss.
// Mutation: list a background repeat whatever window it was made in.
func TestABackgroundRepeatMadeOutsideATestsWindowIsNotItsBackgroundMiss(t *testing.T) {
	a, p := startupOwnAgent(t, startupOwn{worker: true})
	ctx := context.Background()
	_, serr := a.ProvenBegin(ctx, begin("TestReload"))
	require.Nil(t, serr)
	code, body := p.call(t, "/b")
	require.Equal(t, 200, code, "TestReload's own call: %s", body)
	p.mu.Lock()
	p.misses = append(p.misses, models.UnmatchedCall{
		Protocol: "HTTP", Kind: models.MissInTest, ActualSummary: "GET /rates", At: time.Now().Add(-time.Hour),
		Background: true, ClosestMock: "mock-0", Repeat: &models.RepeatNote{Mock: "mock-0", Background: models.RepeatBackgroundWorker},
	})
	p.mu.Unlock()
	e, serr := a.ProvenEnd(ctx, models.ScopeReq{Name: "TestReload", CanSkip: true, Outcome: models.ScopeOutcomePassed})
	require.Nil(t, serr)
	require.NotEqual(t, models.ScopeActionFail, e.Action, "a background repeat made outside TestReload's window failed it: %s", e.Reason)
	ws, err := a.GetScopeWindows(ctx)
	require.NoError(t, err)
	for _, w := range ws {
		if w.Name == "o::TestReload" && w.Evidence != nil {
			require.Empty(t, w.Evidence.Background, "a background repeat made outside TestReload's window is listed as its background miss")
			require.Empty(t, w.Evidence.Misses)
		}
	}
}
