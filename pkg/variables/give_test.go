package variables

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// A value the open scope's test gave binds in that scope, beside what the
// scope bound, without beginning another one: a call checked before it binds
// on. It is refused before any scope began (it would be the run's, which
// every later scope reads), and when its name is bound; the next scope
// forgets it.
func TestTable_GiveBindsInTheOpenScopeOnly(t *testing.T) {
	tb := &Table{}
	tb.KeepRun()
	have, ok := tb.Give(Binding{Name: "given:app", Value: "app-2", Recorded: "app-1"})
	require.False(t, ok, "no scope has begun")
	require.Empty(t, have.Name)
	require.Empty(t, tb.Snapshot())

	tb.Start(nil)
	_, ok = tb.Bind(Binding{Name: "id", Value: "u2", Recorded: "u1"})
	require.True(t, ok)
	gen := tb.Gen()
	got, ok := tb.Give(Binding{Name: "given:app", Value: "app-2", Recorded: "app-1", Place: "apps_test.go:20"})
	require.True(t, ok)
	require.True(t, got.Given)
	require.Equal(t, gen, tb.Gen(), "a given begins no scope")
	snap := tb.Snapshot()
	require.Len(t, snap, 2)
	require.Equal(t, Binding{Name: "given:app", Value: "app-2", Recorded: "app-1", Place: "apps_test.go:20", Given: true}, snap[0])
	require.Equal(t, "id", snap[1].Name)

	have, ok = tb.Give(Binding{Name: "given:app", Value: "app-3", Recorded: "app-1"})
	require.False(t, ok, "a name binds once")
	require.Equal(t, "app-2", have.Value)
	b, _ := tb.Lookup("given:app")
	require.Equal(t, "app-2", b.Value)

	tb.Start(nil)
	_, ok = tb.Lookup("given:app")
	require.False(t, ok, "the next scope forgets the last test's givens")
}
