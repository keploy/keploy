package replay

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

func asideCases(names ...string) []*models.TestCase {
	out := make([]*models.TestCase, 0, len(names))
	for _, n := range names {
		out = append(out, &models.TestCase{Name: n})
	}
	return out
}

// A test case's opening names the test cases set aside before it in recorded
// order that no earlier opening named, each with the recordings of its
// mapping (models.ExactServing.Aside); one recorded after it waits for a
// later opening, one whose mapping holds nothing is left out, and none is
// named twice. A test case run out of recorded order (the deferred streaming
// pass) names those before it that are still unnamed.
func TestAnOpeningNamesTheTestsSetAsideBeforeIt(t *testing.T) {
	p := &exactPlan{owned: map[string][]string{
		"test-1": {"mock-1"}, "test-2": {"mock-2", "mock-3"}, "test-3": {"mock-4"},
		"test-4": nil, "test-5": {"mock-5"}, "test-6": {"mock-6"},
	}}
	p.noteSetAside(asideCases("test-1", "test-2", "test-3", "test-4", "test-5", "test-6"), map[string]bool{"test-2": true, "test-4": true, "test-5": true})

	require.Nil(t, p.asideBefore("test-1"), "nothing was set aside before the first")
	got := p.asideBefore("test-3")
	require.Equal(t, []models.AsideTest{{Test: "test-2", Owned: []string{"mock-2", "mock-3"}}}, got)
	p.sentAside(got)
	require.Nil(t, p.asideBefore("test-3"), "a named one is not named again")
	require.Equal(t, []models.AsideTest{{Test: "test-5", Owned: []string{"mock-5"}}}, p.asideBefore("test-6"), "a set-aside test with no recordings is left out")
	require.Nil(t, p.asideBefore("unknown"))

	// Out of recorded order: test-6 opens first, then test-3.
	q := &exactPlan{owned: p.owned}
	q.noteSetAside(asideCases("test-1", "test-2", "test-3", "test-4", "test-5", "test-6"), map[string]bool{"test-2": true, "test-5": true})
	first := q.asideBefore("test-6")
	require.Equal(t, []models.AsideTest{{Test: "test-2", Owned: []string{"mock-2", "mock-3"}}, {Test: "test-5", Owned: []string{"mock-5"}}}, first)
	q.sentAside(first)
	require.Nil(t, q.asideBefore("test-3"))

	// None set aside, or no plan: nothing is ever named.
	var none *exactPlan
	none.noteSetAside(asideCases("test-1"), map[string]bool{"test-1": true})
	require.Nil(t, none.asideBefore("test-1"))
	r := &exactPlan{owned: p.owned}
	r.noteSetAside(asideCases("test-1", "test-2"), nil)
	require.Nil(t, r.asideBefore("test-2"))
}

// What a set-aside test case's recording answered is no consume of the test
// case it landed in: it is taken out of its consumes, in order, and named as
// ServedAsideLine names it. Consumes with none are returned as they are.
func TestASetAsideRecordingsConsumeIsNamedNotCounted(t *testing.T) {
	own := []models.MockState{{Name: "mock-2"}, {Name: "mock-3"}}
	got, lines := splitServedAside(own)
	require.Equal(t, own, got)
	require.Nil(t, lines)

	got, lines = splitServedAside([]models.MockState{{Name: "mock-2"}, {Name: "mock-1", SetAsideFrom: "TestQ"}, {Name: "mock-3"}, {Name: "mock-9", SetAsideFrom: "TestR"}})
	require.Equal(t, own, got)
	require.Equal(t, []string{"mock-1 served from TestQ (set aside)", "mock-9 served from TestR (set aside)"}, lines)
}

// The result names the serves under the switch, and nothing with it off.
func TestTheResultNamesWhatASetAsideRecordingServed(t *testing.T) {
	r := &Replayer{logger: zap.NewNop()}
	served := []string{"mock-1 served from TestQ (set aside)"}
	t.Setenv(provenswitch.Env, "1")
	require.Equal(t, served, r.namedServedAside("TestK", served))
	require.Nil(t, r.namedServedAside("TestK", nil))
	t.Setenv(provenswitch.Env, "0")
	require.Nil(t, r.namedServedAside("TestK", served))
}

// Switch off, or nothing set aside: an opening's filter is byte for byte
// what it was before set-aside serving (no "aside" key).
func TestAnOpeningWithNothingSetAsideSendsTheSameFilter(t *testing.T) {
	b, err := json.Marshal(models.ExactServing{Test: "test-1", Owned: []string{"mock-1"}})
	require.NoError(t, err)
	require.Equal(t, `{"test":"test-1","owned":["mock-1"]}`, string(b))
	b, err = json.Marshal(models.MockState{Name: "mock-1"})
	require.NoError(t, err)
	require.NotContains(t, string(b), "setAsideFrom")
}
