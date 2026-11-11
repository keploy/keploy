package proxy

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.uber.org/zap"
)

// The mock manager answers the test a worker recording trails with the
// mocks that test lists in the same scope table (SetPoolTests), which the
// learn step reads as the call's own scope; the zero Trail for a recording
// that trails none, for a trail naming a test the table does not list
// (an empty ID included), and until a replay sent both. A wrapper any
// worker reads forwards it, and a table with none clears the previous one.
func TestTheMockManagerAnswersTheTestARecordingTrails(t *testing.T) {
	mm := NewMockManager(nil, nil, zap.NewNop())
	require.Equal(t, learn.Trail{}, mm.TrailOf("mock-9"), "before any table")
	mm.SetPoolTrails(map[string]string{"mock-9": "o::TestA"})
	require.Equal(t, learn.Trail{}, mm.TrailOf("mock-9"), "trails without the table's tests")
	mm.SetPoolTests(map[string][]string{"o::TestA": {"mock-1", "mock-2"}, "o::TestB": {"mock-3"}, "": {"mock-4"}})
	mm.SetPoolTrails(map[string]string{"mock-9": "o::TestA", "mock-8": "o::TestGone", "mock-7": "", "": "o::TestA"})
	got := mm.TrailOf("mock-9")
	require.Equal(t, learn.Trail{Test: "o::TestA", Mocks: []string{"mock-1", "mock-2"}}, got)
	got.Mocks[0] = "changed" // the caller's copy is its own
	require.Equal(t, []string{"mock-1", "mock-2"}, mm.TrailOf("mock-9").Mocks)
	for _, name := range []string{"mock-8", "mock-7", "mock-1", ""} {
		require.Equal(t, learn.Trail{}, mm.TrailOf(name), "%q trails no test the table lists", name)
	}
	var db integrations.MockMemDb = &scopedMockDb{MockMemDb: mm}
	require.Equal(t, "o::TestA", integrations.TrailOf(db, "mock-9").Test, "the wrapper forwards it")
	mm.SetPoolTrails(nil)
	require.Equal(t, learn.Trail{}, mm.TrailOf("mock-9"), "a table with no trails leaves none")
	require.Equal(t, learn.Trail{}, integrations.TrailOf(nil, "mock-9"), "a store that cannot tell trails none")
}

// The proxy installs the trails on its mock manager, and refuses them with
// none.
func TestAProxySetsItsTrailsOnItsMockManager(t *testing.T) {
	p := &Proxy{learnTracker: learn.NewTracker()}
	require.ErrorIs(t, p.SetPoolTrails(map[string]string{"mock-9": "o::TestA"}), errNoMockManager)
	mm := NewMockManager(nil, nil, zap.NewNop())
	mm.SetPoolTests(map[string][]string{"o::TestA": {"mock-1"}})
	p.mockManager = mm
	require.NoError(t, p.SetPoolTrails(map[string]string{"mock-9": "o::TestA"}))
	require.Equal(t, learn.Trail{Test: "o::TestA", Mocks: []string{"mock-1"}}, mm.TrailOf("mock-9"))
}

// The proxy installs the series on its mock manager, and refuses them with
// none.
func TestAProxySetsItsSeriesOnItsMockManager(t *testing.T) {
	p := &Proxy{learnTracker: learn.NewTracker()}
	require.ErrorIs(t, p.SetPoolSeries(map[string][]string{"sweep/o::TestA/1": {"mock-8", "mock-9"}}), errNoMockManager)
	mm := NewMockManager(nil, nil, zap.NewNop())
	p.mockManager = mm
	require.NoError(t, p.SetPoolSeries(map[string][]string{"sweep/o::TestA/1": {"mock-8", "mock-9"}}))
	s := mm.exact.poolSeries.Load()
	require.True(t, s.has("mock-8") && s.has("mock-9"))
	require.True(t, s.of["mock-9"].last)
}
