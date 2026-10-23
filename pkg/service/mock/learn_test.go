package mock

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
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
	byTest, pool, err := m.persistLearned(context.Background(), "s", consumed, flows, map[string][]models.MockEntry{"o::A": {{Name: "mock-1"}}})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(byTest, map[string][]string{"o::A": {"mock-1 body.ts (noise)"}}) || !reflect.DeepEqual(pool, []string{"startup-0 body.ts (noise)"}) {
		t.Fatalf("by test %v, pool %v", byTest, pool)
	}

	m.config.Test.LearnTests = map[string]bool{"o::A": true}
	bound := []models.MockState{{Name: "mock-1", Binds: map[string]models.MockVariables{"mock-old": {"id": {Recorded: "1", Bind: "body.id"}}}}}
	if _, _, err := m.persistLearned(context.Background(), "s", bound, flows, map[string][]models.MockEntry{"o::A": {{Name: "mock-1"}}, "o::B": {{Name: "mock-old"}}}); err == nil {
		t.Fatal("a bind onto a recording the record did not make was written")
	}
}

// clientTestsInstr is keploy mock's agent client, which says whether the
// agent's answer to the session's MockOutgoing said its learn step reads the
// session's tests as client tests (pkg.ClientTestsReader).
type clientTestsInstr struct {
	Instrumentation
	reads bool
}

func (c clientTestsInstr) AgentReadsClientTests() bool { return c.reads }

// keploy mock keeps what record's full round learned only where its agent
// said its learn step reads the session's tests as client tests: an agent
// that did not (older than this keploy), and an instrumentation that cannot
// say, keep nothing, and the round says so; what could not be read fails the
// round.
func TestKeployMockKeepsWhatItsAgentLearnedOnlyFromAClientTestsReader(t *testing.T) {
	consumed := []models.MockState{{Name: "mock-1", ReqBodyNoise: map[string][]string{"body.ts": {}}}}
	flows := []FlowMocks{{Flow: "o::A", Consumed: consumed}}
	expected := map[string][]models.MockEntry{"o::A": {{Name: "mock-1"}}}
	for _, tc := range []struct {
		name  string
		instr Instrumentation
		kept  bool
	}{
		{"an agent that reads client tests", clientTestsInstr{reads: true}, true},
		{"an agent that does not", clientTestsInstr{reads: false}, false},
		{"an instrumentation that cannot say", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &learnedStore{}
			core, logs := observer.New(zap.WarnLevel)
			m := &mockService{config: &config.Config{}, mockDB: store, logger: zap.New(core), instrumentation: tc.instr}
			byTest, _, err := m.keepLearned(context.Background(), "s", consumed, nil, flows, expected)
			if err != nil {
				t.Fatal(err)
			}
			if tc.kept {
				if store.got == nil || len(byTest["o::A"]) != 1 {
					t.Fatalf("persisted %v, by test %v; want what was learned kept", store.got, byTest)
				}
				return
			}
			if store.got != nil || byTest != nil {
				t.Fatalf("persisted %v, by test %v; want nothing kept", store.got, byTest)
			}
			if logs.FilterMessageSnippet("keploy kept nothing its agent learned").Len() != 1 {
				t.Fatalf("logs %v; want the round to say it kept nothing", logs.All())
			}
		})
	}
	m := &mockService{config: &config.Config{}, mockDB: &learnedStore{}, logger: zap.NewNop(), instrumentation: clientTestsInstr{reads: true}}
	if _, _, err := m.keepLearned(context.Background(), "s", consumed, errors.New("agent gone"), flows, expected); err == nil {
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
// from any other agent it writes nothing onto the recording, and says so.
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
			if counts.learnErr != nil {
				t.Fatal(counts.learnErr)
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
