package mock

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// caseVarsDB is a mappings store that keeps what UpsertCases is handed.
type caseVarsDB struct {
	MappingDB
	got map[string]models.MappedTestCase
}

func (d *caseVarsDB) UpsertCases(_ context.Context, _ string, byTest map[string]models.MappedTestCase, _ []models.MockEntry, _ []models.SuiteSpan) error {
	d.got = byTest
	return nil
}

// A variable bound at a client test case's request (a bind at "test case
// <name>", learn.Place.Client) is the test's: record's full round writes it on
// the test's mappings entry, bound as "test case <name>, request <place>", and
// says so for the test, never onto a recording; a bind at a recording stays
// the recording's. One bound so from a recording no test lists, and one the
// test's calls bind at two places, fail the round.
func TestAVariableAClientTestsRequestBindsIsWrittenOnTheTestsEntry(t *testing.T) {
	store, maps := &learnedStore{}, &caseVarsDB{}
	m := &mockService{config: &config.Config{}, mockDB: store, mappingDB: maps, logger: zap.NewNop()}
	name := models.MockVariables{"name": {Recorded: "app-1", Bind: "body.name"}}
	consumed := []models.MockState{{Name: "mock-2", Binds: map[string]models.MockVariables{"test case post-apps-1": name, "mock-1": {"id": {Recorded: "7", Bind: "body.id"}}}}}
	flows := []FlowMocks{{Flow: "o::A", Consumed: consumed}}
	expected := map[string][]models.MockEntry{"o::A": {{Name: "mock-1"}, {Name: "mock-2"}}}
	out, err := m.persistLearned(context.Background(), "s", consumed, nil, flows, expected)
	if err != nil {
		t.Fatal(err)
	}
	want := models.MockVariables{"name": {Recorded: "app-1", Bind: "test case post-apps-1, request body.name"}}
	if got := maps.got["o::A"]; got.ID != "o::A" || !reflect.DeepEqual(got.Variables, want) || len(maps.got) != 1 {
		t.Fatalf("the mappings entries written: %+v; want o::A's with %v", maps.got, want)
	}
	if b := store.got["mock-2"].Binds; !reflect.DeepEqual(b, map[string]models.MockVariables{"mock-1": {"id": {Recorded: "7", Bind: "body.id"}}}) {
		t.Fatalf("the binds written onto the recordings: %v; want mock-1's alone", b)
	}
	if !reflect.DeepEqual(out.byTest["o::A"], []string{"mock-2 body.ts (noise)", "variable name bound at test case post-apps-1, request body.name"}) {
		t.Fatalf("said for o::A: %v", out.byTest["o::A"])
	}
	maps.got = nil
	if _, err := m.persistLearned(context.Background(), "s", consumed, nil, flows, map[string][]models.MockEntry{"o::A": {{Name: "mock-1"}}}); err == nil || !strings.Contains(err.Error(), "no test lists") || maps.got != nil {
		t.Fatalf("a bind at a test case from a recording no test lists: err %v, written %v; want the round failed, nothing written", err, maps.got)
	}
	twice := append(consumed, models.MockState{Name: "mock-1", Binds: map[string]models.MockVariables{"test case post-apps-1": {"name": {Recorded: "app-1", Bind: "body.title"}}}})
	if _, err := m.persistLearned(context.Background(), "s", twice, nil, flows, expected); err == nil || !strings.Contains(err.Error(), "bound the variable name") || maps.got != nil {
		t.Fatalf("a variable bound at two places of the request: err %v, written %v; want the round failed, nothing written", err, maps.got)
	}
	none := &mockService{config: &config.Config{}, mockDB: &learnedStore{}, logger: zap.NewNop()}
	if _, err := none.persistLearned(context.Background(), "s", consumed, nil, flows, expected); err == nil {
		t.Fatal("a store that cannot write a test's variables: the round passed")
	}
}
