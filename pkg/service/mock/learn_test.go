package mock

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml/mapdb"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// A record's full round keeps what it learned only on the recordings the
// record has just made, and on the pool's only when the record replaces it.
func TestLearnsOnto(t *testing.T) {
	m := &mockService{config: &config.Config{}}
	if !m.learnsOnto("o::A") || !m.learnsOnto("") {
		t.Fatal("without LearnTests every mock keeps what was learned")
	}
	m.config.Test.LearnTests = map[string]bool{"o::A": true}
	if !m.learnsOnto("o::A") || m.learnsOnto("o::B") {
		t.Fatal("only the tests the record made keep what was learned")
	}
	m.config.Test.LearnTests["web::api > GET /users"] = true
	if m.learnsOnto("web::api > GET /users/:id") {
		t.Fatal("a test named after a route is no subtest of another")
	}
	if m.learnsOnto("") {
		t.Fatal("a partial record never changes the pool")
	}
	m.config.Test.LearnPool = true
	if !m.learnsOnto("") {
		t.Fatal("a full record keeps what was learned on the pool")
	}
}

// learnedStore is a mock store whose PersistLearned says it added a field on
// each mock it is handed.
type learnedStore struct {
	MockDB
	got map[string]models.MockState
}

func (s *learnedStore) PersistLearned(_ context.Context, _ string, states map[string]models.MockState) (map[string][]string, error) {
	s.got = states
	out := map[string][]string{}
	for name := range states {
		out[name] = []string{"body.ts (noise)"}
	}
	return out, nil
}

// What was learned is said per test, and what was learned onto mocks no test
// consumed (the start-up, the pool) is said too, apart: the record prints
// every field it learned. A bind onto a recording the record did not make
// fails the round.
func TestPersistLearnedSaysEveryField(t *testing.T) {
	store := &learnedStore{}
	m := &mockService{config: &config.Config{}, mockDB: store, logger: zap.NewNop()}
	consumed := []models.MockState{{Name: "mock-1"}, {Name: "startup-0"}}
	flows := []FlowMocks{{Flow: "o::A", Consumed: []models.MockState{{Name: "mock-1"}}}}
	out, err := m.persistLearned(context.Background(), "s", consumed, nil, flows, map[string][]models.MockEntry{"o::A": {{Name: "mock-1"}}})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(out.byTest, map[string][]string{"o::A": {"mock-1 body.ts (noise)"}}) || !reflect.DeepEqual(out.pool, []string{"startup-0 body.ts (noise)"}) {
		t.Fatalf("by test %v, pool %v", out.byTest, out.pool)
	}

	m.config.Test.LearnTests = map[string]bool{"o::A": true}
	bound := []models.MockState{{Name: "mock-1", Binds: map[string]models.MockVariables{"mock-old": {"id": {Recorded: "1", Bind: "body.id"}}}}}
	if _, err := m.persistLearned(context.Background(), "s", bound, nil, flows, map[string][]models.MockEntry{"o::A": {{Name: "mock-1"}}, "o::B": {{Name: "mock-old"}}}); err == nil {
		t.Fatal("a bind onto a recording the record did not make was written")
	}
}

// clientTestsInstr is keploy mock's agent client, which says whether the
// agent's answer to the session's MockOutgoing said its learn step reads the
// session's tests as client tests (pkg.ClientTestsReader), and the version
// the answer named (pkg.AgentVersionReader; "" for none).
type clientTestsInstr struct {
	Instrumentation
	reads   bool
	version string
}

func (c clientTestsInstr) AgentReadsClientTests() bool { return c.reads }

func (c clientTestsInstr) AgentVersion() string { return c.version }

// keploy mock keeps what record's full round learned only where its agent
// said its learn step reads the session's tests as client tests: an agent
// that did not (older than this keploy), and an instrumentation that cannot
// say, keep nothing, and the round, whose tests ran on what was learned,
// fails, saying what it did not keep and why, naming the agent by the
// version its answer named, or as one that named none, and this keploy's;
// one whose agent learned nothing there lost nothing. What could not be read
// fails the round.
func TestKeployMockKeepsWhatItsAgentLearnedOnlyFromAClientTestsReader(t *testing.T) {
	own := utils.Version
	utils.Version = "v3.7.0"
	t.Cleanup(func() { utils.Version = own })
	consumed := []models.MockState{{Name: "mock-1", ReqBodyNoise: map[string][]string{"body.ts": {}}}}
	flows := []FlowMocks{{Flow: "o::A", Consumed: consumed}}
	expected := map[string][]models.MockEntry{"o::A": {{Name: "mock-1"}}}
	const named, unnamed = "the agent (keploy v3.6.100) did not say it reads keploy mock's tests as a test harness's client tests, as an agent older than this keploy (v3.7.0) does not",
		"the agent did not say it reads keploy mock's tests as a test harness's client tests, nor its version, as an agent older than this keploy (v3.7.0) does not"
	for _, tc := range []struct {
		name  string
		instr Instrumentation
		kept  bool
		why   string
	}{
		{"an agent that reads client tests", clientTestsInstr{reads: true, version: "v3.7.0"}, true, ""},
		{"an agent that does not", clientTestsInstr{reads: false, version: "v3.6.100"}, false, named},
		{"an agent that does not, nor names its version", clientTestsInstr{reads: false}, false, unnamed},
		{"an instrumentation that cannot say", nil, false, unnamed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &learnedStore{}
			core, logs := observer.New(zap.WarnLevel)
			m := &mockService{config: &config.Config{}, mockDB: store, logger: zap.New(core), instrumentation: tc.instr}
			out, err := m.keepLearned(context.Background(), "s", consumed, nil, nil, flows, expected)
			if tc.kept {
				if err != nil {
					t.Fatal(err)
				}
				if store.got == nil || len(out.byTest["o::A"]) != 1 {
					t.Fatalf("persisted %v, by test %v; want what was learned kept", store.got, out.byTest)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), "keploy kept nothing its agent learned in record's full round (mock-1 body.ts (noise)): "+tc.why+", so ") || !strings.Contains(err.Error(), "run keploy mock record with the keploy agent of its own version (v3.7.0)") {
				t.Fatalf("err %v; want the round failed, saying what it did not keep, and %q", err, tc.why)
			}
			if store.got != nil || out.byTest != nil {
				t.Fatalf("persisted %v, by test %v; want nothing kept", store.got, out.byTest)
			}
			if said := logs.FilterMessageSnippet("keploy kept nothing its agent learned").FilterField(zap.String("agent", tc.why)); said.Len() != 1 {
				t.Fatalf("logs %v; want the round to say it kept nothing, and %q", logs.All(), tc.why)
			}
			// Nothing learned: nothing lost, and the round completes.
			if _, err := m.keepLearned(context.Background(), "s", []models.MockState{{Name: "mock-1"}}, nil, nil, flows, expected); err != nil {
				t.Fatalf("nothing learned: %v", err)
			}
			// Each kind of learning, alone, is learning not kept: a variable,
			// an echo, a variable bound at an earlier mock of the test, and a
			// clock read each fail the round, which names it.
			for _, only := range []struct {
				learned models.MockState
				said    string
			}{
				{models.MockState{Name: "mock-1", Variables: models.MockVariables{"id": {Recorded: "1", Answer: []string{"body.id"}}}}, "mock-1 id (variable)"},
				{models.MockState{Name: "mock-1", Variables: models.MockVariables{"rid": {Recorded: "r-1", Echo: true}}}, "mock-1 rid (echo)"},
				{models.MockState{Name: "mock-1", Binds: map[string]models.MockVariables{"mock-0": {"id": {Recorded: "1", Bind: "body.id"}}}}, "mock-1 id (bound at mock-0)"},
				{models.MockState{Name: "mock-1", Clocks: map[string]models.ClockOffset{"body.since": {Res: time.Millisecond}}}, "mock-1 body.since (clock)"},
			} {
				if _, err := m.keepLearned(context.Background(), "s", []models.MockState{only.learned}, nil, nil, flows, expected); err == nil || !strings.Contains(err.Error(), "keploy kept nothing its agent learned in record's full round ("+only.said+"): ") {
					t.Fatalf("only %s learned: err %v; want the round failed, naming it", only.said, err)
				}
			}
		})
	}
	m := &mockService{config: &config.Config{}, mockDB: &learnedStore{}, logger: zap.NewNop(), instrumentation: clientTestsInstr{reads: true}}
	if _, err := m.keepLearned(context.Background(), "s", consumed, errors.New("agent gone"), nil, flows, expected); err == nil {
		t.Fatal("a round whose consumed mocks could not be read kept what was learned")
	}
}

// outcomeInstr is keploy mock's agent client at the round's end: what the
// agent served, with what the learn step learned on it, no miss, and whether
// its answer to the session's MockOutgoing said it reads the session's tests
// as client tests.
type outcomeInstr struct {
	clientTestsInstr
	consumed []models.MockState
}

func (o outcomeInstr) GetConsumedMocks(context.Context) ([]models.MockState, error) {
	return o.consumed, nil
}

func (o outcomeInstr) GetMockErrors(context.Context) ([]models.UnmatchedCall, error) {
	return nil, nil
}

// A round's end that learns (record's full round of keploy mock record, the
// proven switch on and config.Test.Learn) keeps what the learn step learned
// only where the agent said it reads the session's tests as client tests:
// from any other agent it writes nothing onto the recording, says so, and
// the round did not complete (learnErr), so no test passes on what it did not
// keep.
func TestARoundsEndKeepsWhatWasLearnedOnlyFromAClientTestsReader(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	consumed := []models.MockState{{Name: "mock-1", ReqBodyNoise: map[string][]string{"body.ts": {}}}}
	for _, reads := range []bool{true, false} {
		t.Run(map[bool]string{true: "an agent that reads client tests", false: "an agent that does not"}[reads], func(t *testing.T) {
			store := &learnedStore{}
			cfg := &config.Config{}
			cfg.Test.Learn = true
			core, logs := observer.New(zap.WarnLevel)
			m := &mockService{config: cfg, mockDB: store, logger: zap.New(core), instrumentation: outcomeInstr{clientTestsInstr{reads: reads}, consumed}}
			counts := m.reportOutcome(context.Background(), 1, replayDetail{set: "s"}, func() bool { return false })
			if reads != (counts.learnErr == nil) {
				t.Fatalf("reads %v: learnErr %v", reads, counts.learnErr)
			}
			if reads {
				if store.got == nil || !reflect.DeepEqual(counts.learnedPool, []string{"mock-1 body.ts (noise)"}) {
					t.Fatalf("persisted %v, said %v; want what was learned kept", store.got, counts.learnedPool)
				}
				return
			}
			if store.got != nil || counts.learnedPool != nil {
				t.Fatalf("persisted %v, said %v; want nothing kept", store.got, counts.learnedPool)
			}
			if logs.FilterMessageSnippet("keploy kept nothing its agent learned").Len() != 1 {
				t.Fatalf("logs %v; want the round to say it kept nothing", logs.All())
			}
		})
	}
}

// A value test B binds for itself and a later test C sends again is one
// variable at one place on the recordings keploy mock writes: the run's,
// which C binds at B's call, in the place of B's own, B's read reading it
// too (learn.RunTakesPlaces, each mock by its test's mapping). The record
// says so, naming the tests' calls and fields, never the value.
func TestPersistLearnedGivesTheRunsVariableAnEarlierTestsOwnPlace(t *testing.T) {
	store := &learnedStore{}
	core, logs := observer.New(zap.InfoLevel)
	m := &mockService{config: &config.Config{}, mockDB: store, logger: zap.New(core)}
	v := func(bind string, use ...string) models.MockVariable {
		return models.MockVariable{Recorded: "o-1", Bind: bind, Use: use}
	}
	consumed := []models.MockState{
		{Name: "mock-0"},
		{Name: "mock-1", Variables: models.MockVariables{"order": v("", "path1")}, Binds: map[string]models.MockVariables{"mock-0": {"order": v("body.order")}}},
		{Name: "mock-2", Variables: models.MockVariables{"run-order": v("", "path1")}, Binds: map[string]models.MockVariables{"mock-0": {"run-order": v("body.order")}}},
	}
	expected := map[string][]models.MockEntry{"o::B": {{Name: "mock-0"}, {Name: "mock-1"}}, "o::C": {{Name: "mock-2"}}}
	if _, err := m.persistLearned(context.Background(), "s", consumed, nil, nil, expected); err != nil {
		t.Fatal(err)
	}
	if got := store.got["mock-1"]; !reflect.DeepEqual(got.Variables, models.MockVariables{"run-order": v("", "path1")}) ||
		!reflect.DeepEqual(got.Binds, map[string]models.MockVariables{"mock-0": {"run-order": v("body.order")}}) {
		t.Fatalf("test B's read is written %+v; want it to read the run's variable, bound at its create", got)
	}
	if got := store.got["mock-2"]; !reflect.DeepEqual(got, consumed[2]) {
		t.Fatalf("test C's read is written %+v; want it as learned", got)
	}
	lines := logs.FilterMessageSnippet("own variable as the test set's").All()
	if len(lines) != 1 {
		t.Fatalf("logged %d lines of the run's variable taking test B's place; want one", len(lines))
	}
	f := lines[0].ContextMap()
	if f["test"] != "o::B" || f["variable"] != "order" || f["setVariable"] != "run-order" || f["mock"] != "mock-0" || f["field"] != "body.order" {
		t.Fatalf("the line says %v; want test B's variable, the run's, and B's call and field", f)
	}
	for _, c := range lines[0].Context {
		if strings.Contains(c.String, "o-1") {
			t.Fatalf("the line's %s carries the value: %q", c.Key, c.String)
		}
	}
}

// settleInstr is outcomeInstr with the agent's end-of-run hook (RunSettler),
// which records each call in order and fails when told to.
type settleInstr struct {
	outcomeInstr
	calls *[]string
	err   error
}

func (s settleInstr) AfterTestRun(_ context.Context, _ string, sets []string, _ models.TestCoverage) error {
	*s.calls = append(*s.calls, "AfterTestRun "+strings.Join(sets, ","))
	return s.err
}

func (s settleInstr) GetConsumedMocks(ctx context.Context) ([]models.MockState, error) {
	*s.calls = append(*s.calls, "GetConsumedMocks")
	return s.outcomeInstr.GetConsumedMocks(ctx)
}

// composeSettleInstr is a compose agent that left account (or none, err) as
// it stopped, and that the CLI must not call: it is gone.
type composeSettleInstr struct {
	settleInstr
	account models.MockOutcome
	err     error
}

func (c composeSettleInstr) ComposeAgentOutcome() (models.MockOutcome, error) {
	return c.account, c.err
}

// Under docker compose the agent is stopped with the project, before the
// CLI could call its end-of-run hook: under the proven switch it settles as
// it stops, and says so in its account. An account that does not say it
// settled (its hook failed, or the agent predates this keploy) is a run the
// agent did not finish: what it served is unread, and the run fails as
// keploy's, naming the cause. An agent that left no account (killed, or
// crashed) is unread whole. Without the switch the account is read as it
// is, settled or not, as before. The CLI never calls the stopped agent.
func TestAComposeReplayIsJudgedOnlyOnAnAccountTheAgentSettled(t *testing.T) {
	served := []models.MockState{{Name: "mock-0"}}
	for _, c := range []struct {
		name         string
		proven       bool
		account      models.MockOutcome
		err          error
		wantConsumed int
		wantSettle   string
	}{
		{"off, unsettled", false, models.MockOutcome{Consumed: served}, nil, 1, ""},
		{"settled", true, models.MockOutcome{Consumed: served, Settled: true}, nil, 1, ""},
		{"hook failed", true, models.MockOutcome{Consumed: served, SettleError: "keploy could not write the row of a program off the run's clock"}, nil, -1,
			"keploy's agent did not finish checking the run's tests as it stopped: keploy could not write the row of a program off the run's clock"},
		{"no word", true, models.MockOutcome{Consumed: served}, nil, -1,
			"keploy's agent did not finish checking the run's tests as it stopped: its account of the replay does not say it did (an agent older than this keploy)"},
		{"killed", true, models.MockOutcome{}, errors.New("keploy did not read the keploy-agent container before it was removed"), -1, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			sw := ""
			if c.proven {
				sw = "1"
			}
			t.Setenv(provenswitch.Env, sw)
			var calls []string
			cfg := &config.Config{CommandType: string(utils.DockerCompose)}
			cfg.Mock.Name = "s"
			instr := composeSettleInstr{settleInstr: settleInstr{outcomeInstr: outcomeInstr{consumed: served}, calls: &calls}, account: c.account, err: c.err}
			m := &mockService{config: cfg, mockDB: &learnedStore{}, logger: zap.NewNop(), instrumentation: instr}
			counts := m.reportOutcome(context.Background(), 1, replayDetail{set: "s"}, func() bool { return false })
			if len(calls) != 0 {
				t.Fatalf("called the stopped agent: %v", calls)
			}
			if counts.consumed != c.wantConsumed {
				t.Fatalf("consumed %d, want %d", counts.consumed, c.wantConsumed)
			}
			got := ""
			if counts.settleErr != nil {
				got = counts.settleErr.Error()
			}
			if got != c.wantSettle {
				t.Fatalf("settleErr %q, want %q", got, c.wantSettle)
			}
			if c.err != nil && counts.missed != -1 {
				t.Fatalf("missed %d for an account never left, want unread (-1)", counts.missed)
			}
		})
	}
}

// Under the proven switch the replay's end calls the agent's end-of-run hook
// (AfterTestRun) once the test command has exited, before it reads what the
// agent served: the agent finishes what it still does for the run's tests
// first, as the row of a process the last test reached that an enterprise
// agent judges after the test's end. A hook that fails leaves what was
// served unread, so no test is judged on it. Without the switch it is not
// called, as before.
func TestTheReplaysEndSettlesTheAgentBeforeItReadsTheOutcome(t *testing.T) {
	for _, c := range []struct {
		proven bool
		err    error
		want   []string
	}{
		{false, nil, []string{"GetConsumedMocks"}},
		{true, nil, []string{"AfterTestRun s", "GetConsumedMocks"}},
		{true, errors.New("agent gone"), []string{"AfterTestRun s"}},
	} {
		t.Run(fmt.Sprintf("proven=%v err=%v", c.proven, c.err), func(t *testing.T) {
			sw := ""
			if c.proven {
				sw = "1"
			}
			t.Setenv(provenswitch.Env, sw)
			var calls []string
			cfg := &config.Config{}
			cfg.Mock.Name = "s"
			m := &mockService{config: cfg, mockDB: &learnedStore{}, logger: zap.NewNop(), instrumentation: settleInstr{outcomeInstr: outcomeInstr{consumed: []models.MockState{{Name: "mock-0"}}}, calls: &calls, err: c.err}}
			counts := m.reportOutcome(context.Background(), 1, replayDetail{set: "s"}, func() bool { return false })
			if !reflect.DeepEqual(calls, c.want) {
				t.Fatalf("calls %v, want %v", calls, c.want)
			}
			if want := map[bool]int{true: -1, false: 1}[c.err != nil]; counts.consumed != want {
				t.Fatalf("consumed %d, want %d", counts.consumed, want)
			}
		})
	}
}

// outgoingInstr is the agent keploy mock replay starts, keeping the options
// the replay handed MockOutgoing.
type outgoingInstr struct {
	*runnerInstr
	got []models.OutgoingOptions
}

func (o *outgoingInstr) MockOutgoing(ctx context.Context, opts models.OutgoingOptions) error {
	o.got = append(o.got, opts)
	return o.runnerInstr.MockOutgoing(ctx, opts)
}

// keploy mock replay hands the agent the tests its learn step learns onto
// (config.Test.LearnTests) while it learns, an empty list as an empty list,
// and nil when it does not learn or learns onto every test. That one field is
// what tells the agent a carried test neither ends nor moves the recorded
// order (keepRecordedOrder): without it every test is fresh to the agent, and
// a carried test run after a newer one stops the round's learning again.
func TestMockReplayHandsTheAgentTheTestsItLearnsOnto(t *testing.T) {
	for _, c := range []struct {
		name   string
		sw     bool
		learn  bool
		tests  map[string]bool
		want   map[string]bool
		wantOn bool
	}{
		{"learning onto the tests a record made anew", true, true, map[string]bool{"o::TestA": true}, map[string]bool{"o::TestA": true}, true},
		{"learning onto no test", true, true, map[string]bool{}, map[string]bool{}, true},
		{"learning onto every test", true, true, nil, nil, true},
		{"not learning", true, false, map[string]bool{"o::TestA": true}, nil, false},
		{"without the switch", false, true, map[string]bool{"o::TestA": true}, nil, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			if c.sw {
				provenOn(t)
			} else {
				t.Setenv(provenswitch.Env, "")
			}
			instr := &outgoingInstr{runnerInstr: newRunnerInstr(t)}
			repo := t.TempDir()
			t.Chdir(repo)
			keployDir := filepath.Join(repo, "keploy")
			require.NoError(t, os.MkdirAll(filepath.Join(keployDir, "set"), 0o755))
			require.NoError(t, os.WriteFile(filepath.Join(keployDir, "set", "mocks.yaml"), []byte("version: api.keploy.io/v1beta1\nkind: Http\nname: mock-0\n"), 0o644))
			writeMapping(t, keployDir)
			instr.consumedMocks = []models.MockState{{Name: "mock-0"}}
			cfg := instrConfig(instr.composeInstr, utils.Native, "go test -p 1 -json ./...")
			cfg.Path = keployDir
			cfg.Mock.OnMiss = "fail"
			cfg.Test.Learn = c.learn
			cfg.Test.LearnTests = c.tests
			utils.ErrCode = 0
			t.Cleanup(func() { utils.ErrCode = 0 })
			require.NoError(t, New(zap.NewNop(), instr, named("mock-0"), mapdb.New(zap.NewNop(), keployDir, ""), nil, nil, cfg).Replay(context.Background()))

			require.Len(t, instr.got, 1, "the replay did not put the agent in mock-serving mode once")
			got := instr.got[0]
			require.Equal(t, c.wantOn, got.Learn)
			require.Equal(t, c.want == nil, got.LearnTests == nil, "learn tests %v, want %v", got.LearnTests, c.want)
			require.Equal(t, len(c.want), len(got.LearnTests))
			for name := range c.want {
				require.True(t, got.LearnTests[name], "%s is not handed to the agent", name)
			}
			if c.tests != nil && got.LearnTests != nil {
				got.LearnTests["o::Other"] = true
				require.False(t, cfg.Test.LearnTests["o::Other"], "the agent was handed the config's own map")
			}
		})
	}
}

// mergingStore is a mock store whose PersistLearned merges each state onto
// a recording that carries nothing yet (learn.MergeLearned), as mockdb's
// does, and says what that added.
type mergingStore struct {
	MockDB
	got map[string]models.MockState
}

func (s *mergingStore) PersistLearned(_ context.Context, _ string, states map[string]models.MockState) (map[string][]string, error) {
	s.got = states
	out := map[string][]string{}
	for name, st := range states {
		if a := learn.MergeLearned(&models.Mock{Name: name}, st, nil); len(a) > 0 {
			out[name] = a
		}
	}
	return out, nil
}

// What record's full round's learn step taught a recording from a call made
// in another window than its own (the agent's LearnedElsewhere) is written
// onto the recording beside what consumed recordings learned: a timer's
// recording no test consumed carries its clock reads. It is no consumption:
// no test's flow holds it, the consumed states are not changed, and a
// recording of a test the round does not learn onto keeps nothing. The
// record says it apart, under the test whose recording it is, with the
// windows that taught it and the evidence: never as the set's start-up and
// pool's, nor on a consumer's line. A recording its own test's call
// consumed keeps what that pairing learned alone: what another
// window taught it is voided, and the record says which evidence won.
func TestKeployMockWritesWhatACallInAnotherWindowTaughtOntoTheRecording(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	minute := models.ClockOffset{Min: -time.Minute, Max: -time.Minute, Res: time.Millisecond}
	consumed := []models.MockState{{Name: "mock-1", ReqBodyNoise: map[string][]string{"body.ts": {}}, Clocks: map[string]models.ClockOffset{"lease_at": minute}}}
	began := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	inC := []models.TeachingWindow{{Test: "o::C", At: began.Add(time.Second)}}
	elsewhere := []models.MockState{
		{Name: "mock-9", Clocks: map[string]models.ClockOffset{"claimed_at": minute}, TaughtIn: []models.TeachingWindow{{At: began.Add(2 * time.Second)}, {Test: "o::C", At: began}}},
		{Name: "mock-1", Clocks: map[string]models.ClockOffset{"dirty_at": minute, "lease_at": minute}, TaughtIn: inC},
	}
	flows := []FlowMocks{{Flow: "o::A", Consumed: []models.MockState{{Name: "mock-1"}}}}
	expected := map[string][]models.MockEntry{"o::A": {{Name: "mock-1"}}, "o::B": {{Name: "mock-9"}}}
	merging := &mergingStore{}
	core, logs := observer.New(zap.InfoLevel)
	m := &mockService{config: &config.Config{}, mockDB: merging, logger: zap.New(core), instrumentation: clientTestsInstr{reads: true}}
	out, err := m.keepLearned(context.Background(), "s", consumed, nil, elsewhere, flows, expected)
	if err != nil {
		t.Fatal(err)
	}
	clock := " (" + minute.String() + ")"
	if !reflect.DeepEqual(out.byTest, map[string][]string{"o::A": {"mock-1 body.ts (noise)", "mock-1 lease_at" + clock}}) || out.pool != nil {
		t.Fatalf("by test %v, on the set's start-up and pool %v; want o::A's own fields alone, and none on the pool", out.byTest, out.pool)
	}
	want := []LearnedElsewhere{
		{Mock: "mock-9", Owner: "o::B", Fields: []string{"claimed_at" + clock}, TaughtIn: []models.TeachingWindow{{Test: "o::C", At: began}, {At: began.Add(2 * time.Second)}}},
	}
	if !reflect.DeepEqual(out.elsewhere, want) {
		t.Fatalf("said as learned elsewhere %+v; want %+v: mock-1, its own test's to consume, learns nothing from another window", out.elsewhere, want)
	}
	if got := out.elsewhere[0].Windows(); got != "o::C's window, between tests" {
		t.Fatalf("mock-9's windows said %q", got)
	}
	learned := logs.FilterMessageSnippet("from calls made in other windows").AllUntimed()
	if len(learned) != 1 || learned[0].ContextMap()["mock"] != "mock-9" || !strings.Contains(fmt.Sprint(learned[0].ContextMap()["evidence"]), "no call of the round consumed") {
		t.Fatalf("the learned lines %+v; want mock-9's alone, naming its evidence", learned)
	}
	void := logs.FilterMessageSnippet("voided what a call made in another window taught").AllUntimed()
	if len(void) != 1 {
		t.Fatalf("the voided lines %+v; want mock-1's", void)
	}
	if c := void[0].ContextMap(); c["mock"] != "mock-1" || c["test"] != "o::A" || c["taughtIn"] != "o::C's window" || c["evidence"] != "its own pairing in the round" || !reflect.DeepEqual(c["voided"], []interface{}{"dirty_at", "lease_at"}) {
		t.Fatalf("mock-1's voided line said %v; want o::A's recording, the clock reads o::C's window taught, its own pairing as the evidence that won", c)
	}
	if len(consumed) != 1 || len(consumed[0].Clocks) != 1 || len(flows[0].Consumed) != 1 {
		t.Fatalf("the consumed states changed: %+v, flows %+v", consumed, flows)
	}

	store := &learnedStore{}
	m.mockDB = store
	out, err = m.keepLearned(context.Background(), "s", consumed, nil, elsewhere, flows, expected)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := store.got["mock-9"].Clocks["claimed_at"]; !ok {
		t.Fatalf("the timer's recording no test consumed was written %+v; want its claimed_at clock read", store.got["mock-9"])
	}
	one := store.got["mock-1"]
	if _, ok := one.Clocks["dirty_at"]; ok {
		t.Fatalf("a recording its own test consumed was written %+v; want the dirty_at another window taught voided", one)
	}
	if _, ok := one.ReqBodyNoise["body.ts"]; !ok || !reflect.DeepEqual(one.Clocks, map[string]models.ClockOffset{"lease_at": minute}) {
		t.Fatalf("a consumed recording was written %+v; want its own learning whole, its own lease_at alone", one)
	}

	// Clock reads alone: what a call made in another window carried as
	// noise, or a variable, is never written, whatever the agent sent (an
	// agent built before the rule was narrowed sends noise too), and a
	// state with no clock read writes nothing.
	store.got = nil
	loose := []models.MockState{
		{Name: "mock-9", Clocks: map[string]models.ClockOffset{"claimed_at": minute}, ReqBodyNoise: map[string][]string{"body._id": {}}},
		{Name: "mock-1", ReqBodyNoise: map[string][]string{"body.lease": {}}, Variables: models.MockVariables{"id": {Recorded: "1", Bind: "body.id"}}},
		{Name: "mock-7", ReqBodyNoise: map[string][]string{"body._id": {}}},
	}
	if _, err := m.keepLearned(context.Background(), "s", consumed, nil, loose, flows, expected); err != nil {
		t.Fatal(err)
	}
	if nine := store.got["mock-9"]; len(nine.ReqBodyNoise) != 0 || len(nine.Clocks) != 1 {
		t.Fatalf("mock-9 was written %+v; want its claimed_at clock read alone", nine)
	}
	if one := store.got["mock-1"]; !reflect.DeepEqual(one.ReqBodyNoise, map[string][]string{"body.ts": {}}) || len(one.Variables) != 0 {
		t.Fatalf("mock-1 was written %+v; want its own noise alone", one)
	}
	if _, ok := store.got["mock-7"]; ok {
		t.Fatalf("a recording another window's call taught only noise was written: %+v", store.got["mock-7"])
	}

	// A test the round does not learn onto keeps its recording as it is.
	m.config.Test.LearnTests = map[string]bool{"o::A": true}
	store.got = nil
	if _, err := m.keepLearned(context.Background(), "s", consumed, nil, elsewhere, flows, expected); err != nil {
		t.Fatal(err)
	}
	if _, ok := store.got["mock-9"]; ok {
		t.Fatalf("a recording of a test the round does not learn onto was written: %+v", store.got["mock-9"])
	}

	// Nor is a voiding said of a recording the round does not learn onto:
	// mock-1 stays as it is, so nothing of it was voided.
	logs.TakeAll()
	m.config.Test.LearnTests = map[string]bool{"o::B": true}
	if _, err := m.keepLearned(context.Background(), "s", consumed, nil, elsewhere, flows, expected); err != nil {
		t.Fatal(err)
	}
	if void := logs.FilterMessageSnippet("voided what a call made in another window taught").AllUntimed(); len(void) != 0 {
		t.Fatalf("the voided lines %+v; want none for a test the round does not learn onto", void)
	}

	// An agent that does not read client tests keeps none of it, and says so.
	m.config.Test.LearnTests = nil
	m.instrumentation = clientTestsInstr{reads: false}
	if _, err := m.keepLearned(context.Background(), "s", nil, nil, elsewhere[:1], nil, expected); err == nil || !strings.Contains(err.Error(), "mock-9 claimed_at") {
		t.Fatalf("err %v; want the round failed, naming mock-9's clock read it did not keep", err)
	}
}

// Two teachings of one recording (the agent taught it in two windows) are
// said as one: every clock read either taught, with both windows. None of
// it is said as the set's start-up and pool's.
func TestKeployMockSaysEveryTeachingOfARecordingAcrossWindows(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	minute := models.ClockOffset{Min: -time.Minute, Max: -time.Minute, Res: time.Millisecond}
	began := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	elsewhere := []models.MockState{
		{Name: "mock-9", Clocks: map[string]models.ClockOffset{"claimed_at": minute}, TaughtIn: []models.TeachingWindow{{Test: "o::C", At: began}}},
		{Name: "mock-9", Clocks: map[string]models.ClockOffset{"expires_at": minute}, TaughtIn: []models.TeachingWindow{{At: began.Add(time.Second)}}},
	}
	m := &mockService{config: &config.Config{}, mockDB: &mergingStore{}, logger: zap.NewNop(), instrumentation: clientTestsInstr{reads: true}}
	out, err := m.keepLearned(context.Background(), "s", nil, nil, elsewhere, nil, map[string][]models.MockEntry{"o::B": {{Name: "mock-9"}}})
	if err != nil {
		t.Fatal(err)
	}
	clock := " (" + minute.String() + ")"
	want := []LearnedElsewhere{{Mock: "mock-9", Owner: "o::B", Fields: []string{"claimed_at" + clock, "expires_at" + clock}, TaughtIn: []models.TeachingWindow{{Test: "o::C", At: began}, {At: began.Add(time.Second)}}}}
	if !reflect.DeepEqual(out.elsewhere, want) || out.pool != nil {
		t.Fatalf("said as learned elsewhere %+v, on the pool %v; want %+v and nothing on the pool", out.elsewhere, out.pool, want)
	}
}

// A recording no test's mapping lists is said, on the line for what a call
// in another window taught it and on the line for what its own pairing
// voided, as the set's start-up and pool's, even when the mapping lists it
// under worker:: keploy mock record serves a worker: entry as the pool's
// (unknownBesideTests), so it names no worker scope. The receipt names no
// owner test for it either.
func TestKeployMockSaysARecordingNoTestOwnsAsTheSetsStartUpAndPools(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	minute := models.ClockOffset{Min: -time.Minute, Max: -time.Minute, Res: time.Millisecond}
	inA := []models.TeachingWindow{{Test: "o::A", At: time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)}}
	mp := &models.Mapping{
		TestCases: []models.MappedTestCase{{ID: "o::A", Mocks: []models.MockEntry{{Name: "mock-1"}}}},
		Worker:    []models.MockEntry{{Name: "mock-w1"}, {Name: "mock-w2"}},
	}
	// A poll in o::A's window taught both worker: recordings; a call of
	// the round consumed mock-w2, so its own pairing wins.
	consumed := []models.MockState{{Name: "mock-w2", Clocks: map[string]models.ClockOffset{"lease_at": minute}}}
	elsewhere := []models.MockState{
		{Name: "mock-w1", Clocks: map[string]models.ClockOffset{"claimed_at": minute}, TaughtIn: inA},
		{Name: "mock-w2", Clocks: map[string]models.ClockOffset{"dirty_at": minute}, TaughtIn: inA},
	}
	core, logs := observer.New(zap.InfoLevel)
	m := &mockService{config: &config.Config{}, mockDB: &mergingStore{}, mappingDB: setTableDB{mapping: mp}, logger: zap.New(core), instrumentation: clientTestsInstr{reads: true}}
	out, err := m.keepLearned(context.Background(), "s", consumed, nil, elsewhere, nil, map[string][]models.MockEntry{"o::A": {{Name: "mock-1"}}})
	if err != nil {
		t.Fatal(err)
	}
	const pool = "the set's start-up and pool"
	learned := logs.FilterMessageSnippet("from calls made in other windows").AllUntimed()
	if len(learned) != 1 || learned[0].ContextMap()["mock"] != "mock-w1" || learned[0].ContextMap()["test"] != pool {
		t.Fatalf("%d learned lines, the first %v; want mock-w1's alone, said as %s", len(learned), firstContext(learned), pool)
	}
	void := logs.FilterMessageSnippet("voided what a call made in another window taught").AllUntimed()
	if len(void) != 1 || void[0].ContextMap()["mock"] != "mock-w2" || void[0].ContextMap()["test"] != pool {
		t.Fatalf("%d voided lines, the first %v; want mock-w2's alone, said as %s", len(void), firstContext(void), pool)
	}
	if len(out.elsewhere) != 1 || out.elsewhere[0].Mock != "mock-w1" || out.elsewhere[0].Owner != "" {
		t.Fatalf("said as learned elsewhere %+v; want mock-w1's, with no owner test", out.elsewhere)
	}
}

// firstContext is the first line's fields, nil for none.
func firstContext(lines []observer.LoggedEntry) map[string]interface{} {
	if len(lines) == 0 {
		return nil
	}
	return lines[0].ContextMap()
}
