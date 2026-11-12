package integrations

import (
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

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
				followed := c.Take().For(CallRound{})
				c.Answered(tc.db, s.answered, followed, CallRound{})
				f := c.Take()
				if next := f.For(CallRound{}); next != s.wantNext || next != "" && f.Series != "s" {
					t.Fatalf("step %d (%s answered): next %q of series %q, want next %q", i, s.answered, next, f.Series, s.wantNext)
				}
				// Take ended it: put it back for the next step.
				c.next, c.series = f.Next, f.Series
			}
		})
	}
	var none OnceChainOnConn
	none.Answered(struct{ MockMemDb }{}, "h", "", CallRound{})
	if next := none.Take().Next; next != "" {
		t.Fatalf("a store with no OnceChains began a chain at %q", next)
	}
}

// A chain is followed only by a call marked in the view the call that
// carried it on was marked in: the same staging and boundary count of the
// store's round. A view installed between the two (a test's begin or end),
// a set staged, a mark with no boundary count, and a mark of no read
// against one of a read end it; two marks of no read (a store that marks no
// round) keep it.
func TestAOnceChainIsFollowedOnlyInTheViewItWasCarriedOnIn(t *testing.T) {
	db := chainsDB{links: map[string]OnceLink{"h": {Series: "s", First: true, Next: "n1"}, "n1": {Series: "s"}}}
	at := func(epoch, bounds uint64) CallRound {
		return NewCallRound(epoch, nil).ReadAt(RoundAt{Epoch: epoch, Gen: 7, Bounds: bounds})
	}
	for _, tc := range []struct {
		name          string
		carried, call CallRound
		want          string
	}{
		{"the same view", at(1, 4), at(1, 4), "n1"},
		{"the same view at another generation", at(1, 4), NewCallRound(1, nil).ReadAt(RoundAt{Epoch: 1, Gen: 9, Bounds: 4}), "n1"},
		{"a view installed since", at(1, 4), at(1, 5), ""},
		{"a set staged since", at(1, 4), at(2, 4), ""},
		{"a mark with no boundary count", NewCallRound(1, nil), NewCallRound(1, nil), ""},
		{"the call's mark keeps no boundary count", at(1, 4), NewCallRound(1, nil), ""},
		{"a mark of no read after one of a read", at(1, 4), CallRound{}, ""},
		{"a mark of a read after one of no read", CallRound{}, at(1, 4), ""},
		{"two marks of no read", CallRound{}, CallRound{}, "n1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var c OnceChainOnConn
			c.Answered(db, "h", "", tc.carried)
			f := c.Take()
			if f.Next != "n1" || f.Series != "s" {
				t.Fatalf("the series' first began no chain: %+v", f)
			}
			if got := f.For(tc.call); got != tc.want {
				t.Fatalf("For = %q, want %q", got, tc.want)
			}
			if c.Take().Next != "" {
				t.Fatal("Take left the chain in progress")
			}
		})
	}
}

// bgOwnDB is a store that tells once links, the background's recordings and
// which recordings the open test owns.
type bgOwnDB struct {
	chainsDB
	bg, own map[string]bool
}

func (d bgOwnDB) InBackground(name string) bool { return d.bg[name] }

func (d bgOwnDB) OwnedByOpenTest(m *models.Mock) bool { return d.own[m.Name] }

// A recording's declared clock fields are checked by their rules
// (DeclaredBackground) where the background answers it: one of the store's
// background recordings, and a once series' recording wherever it lands
// unless the open test owns it. A recording of neither, a once recording
// the open test owns, one of a store that cannot tell ownership, and a nil
// recording are not.
func TestDeclaredBackgroundIsTheBackgroundsAndAOnceSeriesNotTheOpenTestsOwn(t *testing.T) {
	links := chainsDB{links: map[string]OnceLink{"once": {Series: "s", First: true}}}
	db := bgOwnDB{chainsDB: links, bg: map[string]bool{"bg": true}, own: map[string]bool{"owned-once": true}}
	db.links["owned-once"] = OnceLink{Series: "s"}
	for name, want := range map[string]bool{"bg": true, "once": true, "owned-once": false, "plain": false} {
		if got := DeclaredBackground(db, &models.Mock{Name: name}); got != want {
			t.Errorf("DeclaredBackground(%s) = %v, want %v", name, got, want)
		}
	}
	if DeclaredBackground(links, &models.Mock{Name: "once"}) {
		t.Error("a once recording of a store that cannot tell whether the open test owns it is the background's")
	}
	if DeclaredBackground(db, nil) {
		t.Error("a nil recording is the background's")
	}
}
