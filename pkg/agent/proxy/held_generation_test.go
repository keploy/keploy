package proxy

import (
	"slices"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// heldGenCache is a parser that keeps what it read of a store for one generation
// (integrations.StagingGenerations, or the revision before it): it samples
// the generation, reads, samples it again and keeps the read when both
// agree, and reuses it while the generation it samples is the kept one.
type heldGenCache struct {
	gen   func(integrations.MockMemDb) uint64
	kept  bool
	at    uint64
	names []string
	reads int
}

func (c *heldGenCache) recordings(t *testing.T, db integrations.MockMemDb) []string {
	t.Helper()
	g := c.gen(db)
	if c.kept && c.at == g {
		return c.names
	}
	c.reads++
	mocks, err := db.GetPerTestMocksInWindow()
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, mk := range mocks {
		names = append(names, mk.Name)
	}
	slices.Sort(names)
	if c.gen(db) == g {
		c.kept, c.at, c.names = true, g, names
	}
	return names
}

// heldGenerationSamplers are the generations a parser may key what it read
// on: the staging generations (every kind's and one kind's) and the
// revisions before them.
var heldGenerationSamplers = map[string]func(integrations.MockMemDb) uint64{
	"staging generation": func(db integrations.MockMemDb) uint64 {
		g, _ := integrations.StagingGenerationOf(db, "")
		return g
	},
	"staging generation of HTTP": func(db integrations.MockMemDb) uint64 {
		g, _ := integrations.StagingGenerationOf(db, models.HTTP)
		return g
	},
	"revision": func(db integrations.MockMemDb) uint64 {
		return db.(interface{ Revision() uint64 }).Revision()
	},
	"revision of HTTP": func(db integrations.MockMemDb) uint64 {
		return db.(interface{ RevisionByKind(models.Kind) uint64 }).RevisionByKind(models.HTTP)
	},
}

// heldGenerationStore is a manager serving one test exactly, the store a
// parser is handed (the manager, or a connection's view over it), and
// stage, which stages the test's set again, as the replay does at a set's
// start.
func heldGenerationStore(t *testing.T, view bool) (store integrations.MockMemDb, stage func(), want []string) {
	t.Helper()
	t.Setenv(provenswitch.Env, "1")
	now := time.Now()
	first := r3Mock("t-jobs-1", now, "GET", "/jobs/1", "", `{"job":1}`)
	second := r3Mock("t-jobs-2", now.Add(time.Millisecond), "GET", "/jobs/2", "", `{"job":2}`)
	mm := NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(mm.Close)
	stage = func() {
		mocks := []*models.Mock{first, second}
		mm.SetMocksWithWindow(nil, mocks, models.BaseTime, now.Add(time.Hour))
		mm.ServeExactTest("T", mocks)
	}
	store = mm
	if view {
		store = &scopedMockDb{MockMemDb: mm}
	}
	stage()
	return store, stage, []string{"t-jobs-1", "t-jobs-2"}
}

func holdCall(db integrations.MockMemDb) (integrations.MockMemDb, func() bool) {
	return integrations.HoldToRound(db, integrations.MarkCallRound(db))
}

// A store held to a call (HoldToRound) reads nothing once the replay staged
// the set again after the call's mark. A parser that keys what it read on
// the store's generation must then never keep that nothing for the set
// staged after the call: a call marked after the staging (or an unheld
// read) finds the whole set. With the held store reporting the manager's
// generation (the next set's), the parser kept the empty read for it and
// every later call of the set missed.
func TestAHeldReadTheStagingMovedUnderIsNeverKeptForTheNextSet(t *testing.T) {
	for name, gen := range heldGenerationSamplers {
		for _, view := range []bool{false, true} {
			t.Run(name+viewName(view), func(t *testing.T) {
				store, stage, want := heldGenerationStore(t, view)
				c := &heldGenCache{gen: gen}

				held, restaged := holdCall(store)
				stage()
				if got := c.recordings(t, held); len(got) != 0 {
					t.Fatalf("a call held to the set staged before it read %v; want nothing", got)
				}
				if !restaged() {
					t.Fatal("a held call the staging moved under does not say the set was staged again")
				}

				next, nextRestaged := holdCall(store)
				if got := c.recordings(t, next); !slices.Equal(got, want) {
					t.Fatalf("a call marked after the staging read %v through a parser keyed on the %s; want %v (the parser kept the empty read of the call before it for this set)", got, name, want)
				}
				if nextRestaged() {
					t.Fatal("a call marked after the staging says the set was staged again")
				}
				if got := c.recordings(t, store); !slices.Equal(got, want) {
					t.Fatalf("an unheld read gave %v; want %v", got, want)
				}
			})
		}
	}
}

// A held call whose parser finds what it read for the empty set already
// kept, and so samples only the generation, still says the set was staged
// again: sampling the generation is a read of what the call can read.
func TestAHeldCallThatOnlySamplesTheGenerationSaysTheSetWasStagedAgain(t *testing.T) {
	for name, gen := range heldGenerationSamplers {
		t.Run(name, func(t *testing.T) {
			store, stage, _ := heldGenerationStore(t, false)
			c := &heldGenCache{gen: gen}

			held, _ := holdCall(store)
			stage()
			c.recordings(t, held)

			again, restaged := holdCall(store)
			stage()
			reads := c.reads
			if got := c.recordings(t, again); len(got) != 0 {
				t.Fatalf("a call held to the set staged before it read %v; want nothing", got)
			}
			if c.reads != reads {
				t.Fatalf("precondition: the parser read the store again (%d reads, was %d); want it to reuse the empty read it kept", c.reads, reads)
			}
			if !restaged() {
				t.Fatal("a held call that only sampled the generation, after the staging moved, does not say the set was staged again")
			}
		})
	}
}

// While the staging is still its call's, a held store reports the
// generation the manager does, so what a parser read through one call's
// held store (or the bare store) is reused by the next call's.
func TestAHeldStoreReportsTheGenerationOfTheSetItReads(t *testing.T) {
	for name, gen := range heldGenerationSamplers {
		for _, view := range []bool{false, true} {
			t.Run(name+viewName(view), func(t *testing.T) {
				store, _, want := heldGenerationStore(t, view)
				held, restaged := holdCall(store)
				if g, h := gen(store), gen(held); g != h {
					t.Fatalf("a held store reports %s %d; the store it holds reports %d", name, h, g)
				}
				c := &heldGenCache{gen: gen}
				for _, db := range []integrations.MockMemDb{store, held, func() integrations.MockMemDb { h, _ := holdCall(store); return h }()} {
					if got := c.recordings(t, db); !slices.Equal(got, want) {
						t.Fatalf("read %v; want %v", got, want)
					}
				}
				if c.reads != 1 {
					t.Fatalf("the parser read the store %d times for one generation; want once", c.reads)
				}
				if restaged() {
					t.Fatal("a held store the staging did not move under says the set was staged again")
				}
			})
		}
	}
}

// A store held to a call (the manager, or a connection's view over it)
// reports the shared generation, the shared part and the open test's
// unanswered recordings the store does while the staging is still the
// call's, and none of them once the staging moved, when it reads nothing: a
// parser keyed on SharedGen then keys on the staging generation, never
// files the held call's empty read under the next set's SharedGen, and never
// answers it from the shared part of the next set. A call marked after the
// staging reports them again.
func TestAHeldStoreReportsNoSharedPartOnceTheStagingMoved(t *testing.T) {
	for _, view := range []bool{false, true} {
		t.Run("held"+viewName(view), func(t *testing.T) {
			store, stage, _ := heldGenerationStore(t, view)
			held, restaged := holdCall(store)
			sr, ok := held.(integrations.SharedReads)
			if !ok {
				t.Fatal("a held store offers no shared reads")
			}
			for _, kind := range []models.Kind{"", models.HTTP} {
				g, ok := integrations.SharedGenerationOf(held, kind)
				gs, oks := integrations.SharedGenerationOf(store, kind)
				if !ok || !oks || g != gs {
					t.Fatalf("before the staging moved, a held store reports shared generation %v (%v) of kind %q; the store %v (%v)", g, ok, kind, gs, oks)
				}
			}
			if _, _, ok := sr.SharedMocks(); !ok {
				t.Fatal("before the staging moved, a held store reports no shared part")
			}
			if _, ok := sr.OwnedUnanswered(); !ok {
				t.Fatal("before the staging moved, a held store reports no open test")
			}
			if v, owned, ok := sr.OwnedRecordings(); !ok {
				t.Fatal("before the staging moved, a held store reports no open test's recordings")
			} else if _, ok := sr.OwnedUnansweredAt(v, make([]int, len(owned))); !ok {
				t.Fatal("before the staging moved, a held store reads nothing of the open test's view")
			}

			stage()
			for _, kind := range []models.Kind{"", models.HTTP} {
				if g, ok := integrations.SharedGenerationOf(held, kind); ok {
					t.Fatalf("a held store reports shared generation %v of kind %q after the staging moved", g, kind)
				}
			}
			if p, s, ok := sr.SharedMocks(); ok {
				t.Fatalf("a held store reports a shared part after the staging moved: %v %v", exactNames(p), exactNames(s))
			}
			if o, ok := sr.OwnedUnanswered(); ok {
				t.Fatalf("a held store reports the open test's recordings after the staging moved: %v", exactNames(o))
			}
			if _, o, ok := sr.OwnedRecordings(); ok {
				t.Fatalf("a held store reports the open test's view after the staging moved: %v", exactNames(o))
			}
			// The store's view now, read through the held store: nothing.
			v, owned, ok := store.(integrations.SharedReads).OwnedRecordings()
			if !ok {
				t.Fatal("the store reports no open test after the staging")
			}
			if live, ok := sr.OwnedUnansweredAt(v, make([]int, len(owned))); ok {
				t.Fatalf("a held store reads positions %v of the open test's view after the staging moved", live)
			}
			if !restaged() {
				t.Fatal("a held call the staging moved under does not say the set was staged again")
			}

			next, _ := holdCall(store)
			for _, kind := range []models.Kind{"", models.HTTP} {
				g, ok := integrations.SharedGenerationOf(next, kind)
				gs, _ := integrations.SharedGenerationOf(store, kind)
				if !ok || g != gs {
					t.Fatalf("a call marked after the staging reports shared generation %v (%v) of kind %q; the store %v", g, ok, kind, gs)
				}
			}
		})
	}
}

func viewName(view bool) string {
	if view {
		return " through a connection's view"
	}
	return ""
}
