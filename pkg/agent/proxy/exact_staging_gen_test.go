package proxy

import (
	"fmt"
	"math/rand"
	"slices"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// genCache is a parser's index of one read, kept as a parser that adopts
// integrations.StagingGenerations keeps it: built again only when the staging
// generation moves, and filtered at lookup by MockSpent.
type genCache struct {
	read   func() ([]*models.Mock, error)
	gen    uint64
	built  bool
	names  []string
	builds int
}

func (c *genCache) lookup(t *testing.T, db integrations.MockMemDb) []string {
	t.Helper()
	gen, ok := integrations.StagingGenerationOf(db, "")
	if !ok {
		t.Fatal("the store keeps no staging generation")
	}
	if !c.built || gen != c.gen {
		ms, err := c.read()
		if err != nil {
			t.Fatal(err)
		}
		if again, _ := integrations.StagingGenerationOf(db, ""); again != gen {
			t.Fatalf("the staging generation moved during a single-threaded read: %d then %d", gen, again)
		}
		c.names, c.gen, c.built = exactNames(ms), gen, true
		c.builds++
	}
	sg := db.(integrations.StagingGenerations)
	out := make([]string, 0, len(c.names))
	for _, n := range c.names {
		if !sg.MockSpent(n) {
			out = append(out, n)
		}
	}
	return out
}

func freshNames(t *testing.T, read func() ([]*models.Mock, error)) []string {
	t.Helper()
	ms, err := read()
	if err != nil {
		t.Fatal(err)
	}
	return exactNames(ms)
}

// TestAnIndexKeyedOnTheStagingGenerationIsBuiltOncePerView: under exact
// serving a consume moves Revision (and the consumed kind's RevisionByKind),
// so a parser keyed on them builds its index again on every call (lat-pg:
// every Parse rebuilds Postgres' three tiers from the whole set), but not the
// staging generations. Counted: over 40 consumes of the open test's own
// mocks, an index of the per-test read keyed on the staging generation is
// built once, and its lookup, less what MockSpent reports, is the fresh read
// after each consume; keyed on Revision it would be built 41 times. The next
// view, and a store into a tree, move the generation.
func TestAnIndexKeyedOnTheStagingGenerationIsBuiltOncePerView(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	const n = 40
	owned := make([]*models.Mock, n)
	for i := range owned {
		owned[i] = exactMock(fmt.Sprintf("own-%02d", i), time.Duration(i)*time.Millisecond, models.LifetimePerTest)
	}
	hs := configMock("handshake", time.Second)
	mm := stagedManager(t, owned, []*models.Mock{hs})
	mm.ServeExactTest("T", owned)
	kind := owned[0].Kind

	c := &genCache{read: mm.GetPerTestMocksInWindow}
	if got := c.lookup(t, mm); len(got) != n {
		t.Fatalf("precondition: the test's view reads %d mocks; want %d", len(got), n)
	}
	// A connection's scoped view made before exact serving began passes
	// through to the store: the same generation and the same spent mocks.
	scoped := &scopedMockDb{MockMemDb: mm}
	sc := &genCache{read: scoped.GetPerTestMocksInWindow}
	sc.lookup(t, scoped)
	gen, kgen, rev, krev := mm.StagingGeneration(), mm.StagingGenerationByKind(kind), mm.Revision(), mm.RevisionByKind(kind)
	for i, mk := range owned {
		if !mm.DeleteFilteredMock(*mk) {
			t.Fatalf("consume %d (%s) refused", i, mk.Name)
		}
		if !mm.MockSpent(mk.Name) {
			t.Fatalf("%s answered and is not reported spent", mk.Name)
		}
		if got, want := c.lookup(t, mm), freshNames(t, mm.GetPerTestMocksInWindow); !slices.Equal(got, want) {
			t.Fatalf("after consume %d the index less what is spent reads %v; the store reads %v", i, got, want)
		}
		if got, want := sc.lookup(t, scoped), freshNames(t, scoped.GetPerTestMocksInWindow); !slices.Equal(got, want) {
			t.Fatalf("after consume %d the scoped view's index less what is spent reads %v; the view reads %v", i, got, want)
		}
	}
	if c.builds != 1 || sc.builds != 1 {
		t.Fatalf("%d consumes built the index %d times, the scoped view's %d; want 1 each (each consume moved the staging generation)", n, c.builds, sc.builds)
	}
	if mm.StagingGeneration() != gen || mm.StagingGenerationByKind(kind) != kgen {
		t.Fatalf("consumes moved the staging generations: %d -> %d, by kind %d -> %d", gen, mm.StagingGeneration(), kgen, mm.StagingGenerationByKind(kind))
	}
	if mm.Revision()-rev < n || mm.RevisionByKind(kind)-krev < n {
		t.Fatalf("%d consumes moved Revision by %d and RevisionByKind by %d; want each at least %d (parsers keyed on them read again)", n, mm.Revision()-rev, mm.RevisionByKind(kind)-krev, n)
	}
	if mm.MockSpent(hs.Name) {
		t.Fatal("the unconsumed handshake is reported spent")
	}

	// The next view (the test begins again) moves the generation: the index
	// is built again, and nothing is spent in it.
	mm.ServeExactTest("T", owned)
	if got := c.lookup(t, mm); len(got) != n || c.builds != 2 {
		t.Fatalf("the next view reads %d mocks after %d builds; want %d after 2", len(got), c.builds, n)
	}
	if mm.MockSpent(owned[0].Name) {
		t.Fatal("a mock answered in the last view is reported spent in the next")
	}
	// A store into a tree (a re-stamp of the protocol-phase handshake) moves it.
	before := mm.StagingGeneration()
	restamped := hs.ShallowCopy()
	restamped.TestModeInfo.SortOrder++
	mm.UpdateUnFilteredMock(hs, restamped)
	if mm.StagingGeneration() == before {
		t.Fatal("a store into the session tree did not move the staging generation")
	}
}

// TestAStagingGenerationIndexIsTheReadUnderRandomChanges: over random
// stagings, test opens, pool views, worker scopes and consumes through each
// door, an index of each read built once per staging generation and filtered
// by MockSpent reads what the store reads, in the same order, after every
// step. Consumes build nothing again.
func TestAStagingGenerationIndexIsTheReadUnderRandomChanges(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	rng := rand.New(rand.NewSource(20261027))
	const tests, perTest = 4, 4
	var staged []*models.Mock
	byTest := make([][]*models.Mock, tests)
	at := time.Duration(0)
	for k := 0; k < tests; k++ {
		for j := 0; j < perTest; j++ {
			at += time.Millisecond
			mk := exactMock(fmt.Sprintf("t%d-%d", k, j), at, models.LifetimePerTest)
			byTest[k] = append(byTest[k], mk)
			staged = append(staged, mk)
		}
	}
	var session []*models.Mock
	for i := 0; i < 3; i++ {
		session = append(session, configMock(fmt.Sprintf("cfg-%d", i), time.Second+time.Duration(i)*time.Millisecond))
	}
	pool := []string{"p0", "p1", "p2", "p3"}
	for i, p := range pool {
		session = append(session, httpCall(p, 2*time.Second+time.Duration(i)*time.Millisecond, "/"+p))
	}
	workers := []string{"w0", "w1"}
	for i, w := range workers {
		session = append(session, httpCall(w, 3*time.Second+time.Duration(i)*time.Millisecond, "/"+w))
	}
	mm := stagedManager(t, staged, session)
	// Exact serving from the start, as keploy mock replay: a consume with no
	// view in force deletes from its tree, which moves the generation.
	mm.ServeExactPool("k", nil)

	reads := map[string]func() ([]*models.Mock, error){
		"per-test":           mm.GetPerTestMocksInWindow,
		"session":            mm.GetSessionMocks,
		"unfiltered":         mm.GetUnFilteredMocks,
		"filtered":           mm.GetFilteredMocks,
		"filtered-in-window": mm.GetFilteredMocksInWindow,
		"startup":            mm.GetStartupMocks,
	}
	order := []string{"per-test", "session", "unfiltered", "filtered", "filtered-in-window", "startup"}
	caches := map[string]*genCache{}
	for _, r := range order {
		caches[r] = &genCache{read: reads[r]}
	}
	subset := func(names []string) []string {
		var out []string
		for _, n := range names {
			if rng.Intn(2) == 0 {
				out = append(out, n)
			}
		}
		return out
	}
	const steps = 3000
	consumes, refused := 0, 0
	for step := 0; step < steps; step++ {
		what := ""
		gen := mm.StagingGeneration()
		consume := false
		switch r := rng.Intn(20); {
		case r == 0:
			what = "restage"
			mm.SetMocksWithWindow(staged, session, models.BaseTime, time.Now())
		case r <= 3:
			k := rng.Intn(tests)
			what = fmt.Sprintf("open t%d", k)
			mm.ServeExactWorker(subset(workers))
			mm.ServeExactTest(fmt.Sprintf("T%d", k), byTest[k])
		case r <= 5:
			what = "pool view"
			var ahead []*models.Mock
			if rng.Intn(2) == 0 {
				ahead = byTest[rng.Intn(tests)][:1+rng.Intn(perTest)]
			}
			mm.ServeExactPool("k", subset(pool), ahead...)
		default:
			// A consume of something a read shows now, through a random door.
			var shown []*models.Mock
			for _, r := range order {
				ms, _ := reads[r]()
				shown = append(shown, ms...)
			}
			if len(shown) == 0 {
				what = "nothing to consume"
				break
			}
			mk := shown[rng.Intn(len(shown))]
			consume = !mk.IsProtocolPhase() && mm.exactView() != nil
			var ok bool
			switch rng.Intn(3) {
			case 0:
				what = "delete-filtered " + mk.Name
				ok = mm.DeleteFilteredMock(*mk)
			case 1:
				what = "delete-unfiltered " + mk.Name
				ok = mm.DeleteUnFilteredMock(*mk)
			default:
				what = "update " + mk.Name
				ok = mm.UpdateUnFilteredMock(mk, mk)
				consume = false // an update door may store into a tree (a re-stamp)
			}
			if ok {
				consumes++
			} else {
				refused++
			}
		}
		if consume && mm.StagingGeneration() != gen {
			t.Fatalf("step %d (%s): a consume moved the staging generation", step, what)
		}
		for _, r := range order {
			got, want := caches[r].lookup(t, mm), freshNames(t, reads[r])
			if !slices.Equal(got, want) {
				t.Fatalf("step %d (%s): the %s index less what is spent reads %v; the store reads %v", step, what, r, got, want)
			}
		}
	}
	if consumes < steps/4 {
		t.Fatalf("only %d of %d steps consumed (%d refused): the walk does not exercise consumes", consumes, steps, refused)
	}
	if b := caches["per-test"].builds; b >= consumes {
		t.Fatalf("the per-test index was built %d times over %d consumes: the generation follows consumes", b, consumes)
	}
	t.Logf("%d steps, %d consumes (%d refused), per-test index built %d times", steps, consumes, refused, caches["per-test"].builds)
}
