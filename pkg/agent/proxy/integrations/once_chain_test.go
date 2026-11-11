package integrations

import "testing"

// chainsDB is a store that tells once series' links (OnceChains).
type chainsDB struct {
	MockMemDb
	links map[string]OnceLink
}

func (c chainsDB) OnceLink(mock string) (OnceLink, bool) {
	l, ok := c.links[mock]
	return l, ok
}

// A connection's once chain begins with the series' first recording, goes
// on with each recording it names next, and ends at the series' last, at
// any other answer, and when a later recording of the series answers a
// call the chain did not name (its first answered on another connection).
// A store that tells no links begins none.
func TestAConnectionsOnceChainFollowsItsSeriesInTurn(t *testing.T) {
	db := chainsDB{links: map[string]OnceLink{
		"h":  {Series: "s", First: true, Next: "n1"},
		"n1": {Series: "s", Next: "n2"},
		"n2": {Series: "s"},
	}}
	type step struct{ answered, wantNext string }
	for _, tc := range []struct {
		name  string
		db    MockMemDb
		steps []step
	}{
		{"through the series", db, []step{{"h", "n1"}, {"n1", "n2"}, {"n2", ""}, {"n1", ""}}},
		{"another answer ends it", db, []step{{"h", "n1"}, {"other", ""}, {"n1", ""}}},
		{"a later recording begins none", db, []step{{"n1", ""}, {"n2", ""}}},
		{"the first again begins it again", db, []step{{"h", "n1"}, {"h", "n1"}}},
		{"a store that tells no links", chainsDB{}, []step{{"h", ""}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var c OnceChainOnConn
			for i, s := range tc.steps {
				followed, _ := c.Take()
				c.Answered(tc.db, s.answered, followed)
				next, series := c.Take()
				if next != s.wantNext || next != "" && series != "s" {
					t.Fatalf("step %d (%s answered): next %q of series %q, want next %q", i, s.answered, next, series, s.wantNext)
				}
				// Take ended it: put it back for the next step.
				c.next, c.series = next, series
			}
		})
	}
	var none OnceChainOnConn
	none.Answered(struct{ MockMemDb }{}, "h", "")
	if next, _ := none.Take(); next != "" {
		t.Fatalf("a store with no OnceChains began a chain at %q", next)
	}
}
