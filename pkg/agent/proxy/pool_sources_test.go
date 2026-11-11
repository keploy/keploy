package proxy

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// The mock manager answers the sources declared rules give a recording's
// fields from the replay's scope table, a copy each time; none before a
// table, for a recording no rule declares one of, for a name the table
// gives none, and once a table with none replaced it. The worker's view
// forwards them, a store that cannot tell has none, and a proxy sets them
// on its mock manager (none: errNoMockManager).
func TestTheMockManagerAnswersARecordingsDeclaredSources(t *testing.T) {
	mm := NewMockManager(nil, nil, zap.NewNop())
	require.Nil(t, mm.SourcesOf("mock-9"), "before any table")
	minted := models.DeclaredSource{Rule: "id@0a1b2c3d", Kind: models.DeclaredSourceMinted, Field: "body.docs.msgs.0._id", Minted: &models.MintedForm{MinLen: 36, MaxLen: 36}}
	mm.SetPoolSources(map[string][]models.DeclaredSource{"mock-9": {minted}, "mock-8": {}, "": {minted}})
	got := mm.SourcesOf("mock-9")
	require.Equal(t, []models.DeclaredSource{minted}, got)
	got[0].Field = "changed" // the caller's copy is its own
	require.Equal(t, []models.DeclaredSource{minted}, mm.SourcesOf("mock-9"))
	for _, name := range []string{"mock-8", "mock-1", ""} {
		require.Nil(t, mm.SourcesOf(name), "%q has no declared source", name)
	}
	var db integrations.MockMemDb = &scopedMockDb{MockMemDb: mm}
	require.Equal(t, []models.DeclaredSource{minted}, integrations.SourcesOf(db, "mock-9"), "the wrapper forwards them")
	mm.SetPoolSources(nil)
	require.Nil(t, mm.SourcesOf("mock-9"), "a table with no sources leaves none")
	require.Nil(t, integrations.SourcesOf(nil, "mock-9"), "a store that cannot tell has none")

	p := &Proxy{learnTracker: learn.NewTracker()}
	require.ErrorIs(t, p.SetPoolSources(map[string][]models.DeclaredSource{"mock-9": {minted}}), errNoMockManager)
	p.mockManager = mm
	require.NoError(t, p.SetPoolSources(map[string][]models.DeclaredSource{"mock-9": {minted}}))
	require.Equal(t, []models.DeclaredSource{minted}, mm.SourcesOf("mock-9"))
}

// The mock manager answers whether the replay's scope table turned the
// time default on: off before any table, on once one did, off again once a
// table turned it off. The worker's view forwards it, a store that cannot
// tell has it off, and a proxy sets it on its mock manager (none:
// errNoMockManager).
func TestTheMockManagerAnswersTheTimeDefault(t *testing.T) {
	mm := NewMockManager(nil, nil, zap.NewNop())
	require.False(t, mm.TimeForm(), "before any table")
	var db integrations.MockMemDb = &scopedMockDb{MockMemDb: mm}
	mm.SetPoolTimeForm(true)
	require.True(t, mm.TimeForm())
	require.True(t, integrations.TimeFormOf(db), "the wrapper forwards it")
	mm.SetPoolTimeForm(false)
	require.False(t, mm.TimeForm(), "a table that turns it off")
	require.False(t, integrations.TimeFormOf(db))
	require.False(t, integrations.TimeFormOf(nil), "a store that cannot tell has it off")

	p := &Proxy{learnTracker: learn.NewTracker()}
	require.ErrorIs(t, p.SetPoolTimeForm(true), errNoMockManager)
	p.mockManager = mm
	require.NoError(t, p.SetPoolTimeForm(true))
	require.True(t, mm.TimeForm())
}
