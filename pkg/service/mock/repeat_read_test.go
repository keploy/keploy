package mock

import (
	"reflect"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

// keploy alone names no read; a registered namer names them from each test's
// own recordings the agent holds, and only reads of at least two of them go
// to the agent.
func TestARegisteredNamerNamesEachTestsRepeatedReads(t *testing.T) {
	loaded := []*models.Mock{{Name: "mock-1"}, {Name: "mock-2"}, {Name: "mock-3"}, nil, {Name: "mock-5"}}
	table := map[string][]string{"o::TestA": {"mock-1", "mock-2", "mock-4"}, "o::TestB": {"mock-3", "mock-5"}}
	if got := namedRepeatReads("o", table, loaded); got != nil {
		t.Fatalf("keploy alone named %v", got)
	}
	var gotSet string
	var gotOwned map[string][]string
	unregister := RegisterRepeatReadNamer(func(set string, owned map[string][]*models.Mock) map[string][]models.RepeatRead {
		gotSet, gotOwned = set, map[string][]string{}
		for test, mks := range owned {
			for _, mk := range mks {
				gotOwned[test] = append(gotOwned[test], mk.Name)
			}
		}
		return map[string][]models.RepeatRead{
			"o::TestA": {
				{Name: "onboarding", Mocks: []string{"mock-1", "mock-2"}},
				{Name: "once", Mocks: []string{"mock-1"}},
				{Name: "", Mocks: []string{"mock-1", "mock-2"}},
				{Name: "not-held", Mocks: []string{"mock-1", "mock-4"}},
				{Name: "another-tests", Mocks: []string{"mock-1", "mock-3"}},
			},
			"o::TestB": {{Name: "list", Mocks: []string{"mock-3", "mock-5"}}},
		}
	})
	defer unregister()
	got := namedRepeatReads("o", table, loaded)
	if gotSet != "o" || !reflect.DeepEqual(gotOwned, map[string][]string{"o::TestA": {"mock-1", "mock-2"}, "o::TestB": {"mock-3", "mock-5"}}) {
		t.Fatalf("the namer was handed %q %v", gotSet, gotOwned)
	}
	want := map[string][]models.RepeatRead{
		"o::TestA": {{Name: "onboarding", Mocks: []string{"mock-1", "mock-2"}}},
		"o::TestB": {{Name: "list", Mocks: []string{"mock-3", "mock-5"}}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("named %v, want %v", got, want)
	}
	func() {
		defer func() {
			if recover() == nil {
				t.Error("a second namer registered")
			}
		}()
		RegisterRepeatReadNamer(func(string, map[string][]*models.Mock) map[string][]models.RepeatRead { return nil })
	}()
	unregister()
	if got := namedRepeatReads("o", table, loaded); got != nil {
		t.Fatalf("an unregistered namer named %v", got)
	}
}
