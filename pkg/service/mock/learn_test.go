package mock

import (
	"context"
	"reflect"
	"testing"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
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
