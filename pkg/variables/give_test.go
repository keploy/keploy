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

// A note says why a variable of the open scope is not bound (Note): read
// while the variable is not bound (Unbound), copied with the table but not
// shared with it, and forgotten with the scope. None is taken before a
// scope begins.
func TestANoteSaysWhyAVariableOfTheScopeIsNotBound(t *testing.T) {
	var tb Table
	if tb.Note("key", "why") || tb.Unbound("key") != "" {
		t.Fatal("a note was taken before any scope began")
	}
	tb.Start(nil)
	if !tb.Note("key", "first") || !tb.Note("key", "the request bound nothing") || tb.Unbound("key") != "the request bound nothing" {
		t.Fatalf("noted %q; want the latest note", tb.Unbound("key"))
	}
	if tb.Unbound("other") != "" {
		t.Fatal("a variable never noted has a note")
	}
	if !tb.Note("second", "why 2") || tb.Unbound("key") != "the request bound nothing" || tb.Unbound("second") != "why 2" {
		t.Fatal("a note of another variable took the first one's")
	}
	c := tb.Clone()
	if c.Unbound("key") != "the request bound nothing" {
		t.Fatal("a clone lost the scope's note")
	}
	if _, ok := tb.Give(Binding{Name: "key", Value: "app-2", Recorded: "app-1"}); !ok || tb.Unbound("key") != "" {
		t.Fatal("a bound variable still reads its note")
	}
	if c.Unbound("key") != "the request bound nothing" {
		t.Fatal("a bind in the table reached its clone")
	}
	if !c.Note("second", "the clone's") || tb.Unbound("second") != "why 2" {
		t.Fatal("a note in the clone reached its table")
	}
	for name, next := range map[string]func(*Table){"Start": func(t *Table) { t.Start(nil) }, "Reset": (*Table).Reset, "NewSet": (*Table).NewSet} {
		c := tb.Clone()
		c.Note("other", "why")
		next(c)
		if c.Unbound("other") != "" {
			t.Fatalf("%s kept the ended scope's note", name)
		}
	}
}
