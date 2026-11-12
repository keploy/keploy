package proxy

import (
	"fmt"
	"math/rand"
	"slices"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	httpint "go.keploy.io/server/v3/pkg/agent/proxy/integrations/http"
	"go.keploy.io/server/v3/pkg/models"
)

// A miss's diagnosis by keys (FindStagedNotOwnedByKeys) takes up only the
// staged recordings filed under the call's request keys, so a miss costs the
// same whatever the set holds. It used to hand every staged recording of the
// kind to the parser's match, which for Mongo compares whole documents: on
// each miss, the whole set. Each staged tree's index is built on the first
// miss, never again on a later one. It names the recording the call is.
func TestAMissByKeysTakesUpOnlyTheRecordingsFiledUnderItsKeys(t *testing.T) {
	const misses = 5
	perMiss := map[int]uint64{}
	for _, n := range []int{100, 2000} {
		session := make([]*models.Mock, 0, n)
		for i := range n {
			session = append(session, httpCall(fmt.Sprintf("r-%d", i), time.Duration(i)*time.Millisecond, fmt.Sprintf("/p-%d", i)))
		}
		mm := stagedManager(t, nil, session)
		mm.ServeExactPool("k", nil)
		tried0, builds0 := notOwnedTried.Load(), keyIndexBuilds.Load()
		var first uint64
		for j := range misses {
			at := (j * 37) % n
			path := fmt.Sprintf("/p-%d", at)
			got := stagedNotOwnedByKeysNow(mm, models.HTTP, httpint.RequestKeysOf("GET", path, "", false), func(m *models.Mock) bool {
				return m.Spec.HTTPReq.URL == path
			})
			if want := fmt.Sprintf("r-%d", at); got != want {
				t.Fatalf("n=%d: a miss for %s named %q, want %q", n, path, got, want)
			}
			if j == 0 {
				first = keyIndexBuilds.Load() - builds0
			}
		}
		if builds := keyIndexBuilds.Load() - builds0; builds != first || builds > 3 {
			t.Errorf("n=%d: the staged trees' key indexes were built %d times over %d misses, %d of them by the first; want once per tree, by the first miss", n, builds, misses, first)
		}
		perMiss[n] = (notOwnedTried.Load() - tried0) / misses
	}
	if perMiss[100] != perMiss[2000] || perMiss[2000] > 3 {
		t.Errorf("a miss took up %d staged recordings with 100 staged and %d with 2000; want the same few, whatever the set holds", perMiss[100], perMiss[2000])
	}
}

// A kind whose parser declared no request keys files no recording under any
// key: a miss of it asked by keys still looks at every staged recording of
// the kind, as before.
func TestAMissByKeysOfAKindWithNoRequestKeysLooksAtEveryRecording(t *testing.T) {
	if models.HasRequestKeys(models.GENERIC) {
		t.Fatal("precondition: the generic parser declares no request keys")
	}
	m := exactMock("generic-other", time.Second, models.LifetimeSession)
	m.Kind = models.GENERIC
	mm := stagedManager(t, nil, []*models.Mock{m})
	mm.ServeExactPool("k", nil)
	got := stagedNotOwnedByKeysNow(mm, models.GENERIC, []string{"any"}, func(mk *models.Mock) bool { return mk.Name == "generic-other" })
	if got != "generic-other" {
		t.Fatalf("a miss of a kind with no request keys, asked by keys, named %q; want generic-other", got)
	}
}

// A tree's lookup by keys (keyedValues) hands the mocks of the kind filed
// under any of the keys, each once, in tree order, and none the tree no longer
// holds; a removal does not rebuild the index.
func TestATreesLookupByKeysIsInTreeOrderOnceEachAndLive(t *testing.T) {
	ix := &integrations.MockIndex{Keys: func(m *models.Mock) []string { return keysOfMeta(m) }}
	db := NewTreeDb(customComparator)
	add := func(i int, kind models.Kind, keys ...string) *models.Mock {
		m := &models.Mock{Name: fmt.Sprintf("m%d", i), Kind: kind, TestModeInfo: models.TestModeInfo{ID: i, SortOrder: int64(i + 1)}}
		m.Spec.Metadata = map[string]string{"k": keys[0]}
		if len(keys) > 1 {
			m.Spec.Metadata["k2"] = keys[1]
		}
		db.insert(m.TestModeInfo, m)
		return m
	}
	add(0, models.HTTP, "a")
	m1 := add(1, models.HTTP, "b")
	add(2, models.HTTP, "a", "b")
	add(3, models.HTTP, "c")
	add(4, models.HTTP, "b")
	add(5, models.Mongo, "a")
	look := func(kind models.Kind, keys ...string) []string {
		return exactNames(db.keyedValues(ix, keys, kind))
	}
	if got, want := look(models.HTTP, "k:a", "k:b"), []string{"m0", "m1", "m2", "m4"}; !slices.Equal(got, want) {
		t.Errorf("HTTP under a or b: %v, want %v", got, want)
	}
	if got, want := look("", "k:b", "k:a", "k:b"), []string{"m0", "m1", "m2", "m4", "m5"}; !slices.Equal(got, want) {
		t.Errorf("any kind under b, a, b: %v, want %v", got, want)
	}
	builds := keyIndexBuilds.Load()
	if !db.deleteMock(m1.TestModeInfo, *m1) {
		t.Fatal("precondition: m1 was not removed")
	}
	if got, want := look(models.HTTP, "k:a", "k:b"), []string{"m0", "m2", "m4"}; !slices.Equal(got, want) {
		t.Errorf("HTTP under a or b once m1 is removed: %v, want %v", got, want)
	}
	if keyIndexBuilds.Load() != builds {
		t.Error("a removal rebuilt the index")
	}
	// The verify mode (on in these rows) catches a stale index: a list that
	// lost an entry the tree still holds panics the lookup.
	if !exactViewVerify.Load() {
		t.Fatal("precondition: the exact view's verify mode is on in these rows")
	}
	db.mu.Lock()
	db.keyed[ix].lists["k:a"] = db.keyed[ix].lists["k:a"][1:]
	db.mu.Unlock()
	func() {
		defer func() {
			if recover() == nil {
				t.Error("the verify mode let a lookup through a stale index")
			}
		}()
		look(models.HTTP, "k:a")
	}()
}

// keysOfMeta is a mock's test keys: "k:" and its "k" metadata, and "k:" and
// its "k2" metadata when it has one.
func keysOfMeta(m *models.Mock) []string {
	keys := []string{"k:" + m.Spec.Metadata["k"]}
	if k2 := m.Spec.Metadata["k2"]; k2 != "" {
		keys = append(keys, "k:"+k2)
	}
	return keys
}

// genericKeyedSet is a seeded set of generic mocks for the differential row: five
// tests' per-test recordings and a session pool, each under one or two of
// four keys, in shuffled recorded order. Each call makes new mocks, the same
// for the same seed.
func genericKeyedSet(seed int64) (perTest, session []*models.Mock, owned map[string][]string) {
	rng := rand.New(rand.NewSource(seed))
	ks := []string{"a", "b", "c", "d"}
	ops := []string{"x", "y", "z"}
	at := rng.Perm(60)
	owned = map[string][]string{}
	for i := range 60 {
		lt := models.LifetimeSession
		if i < 40 {
			lt = models.LifetimePerTest
		}
		m := exactMock(fmt.Sprintf("g%d", i), time.Duration(at[i])*time.Millisecond, lt)
		m.Kind = models.GENERIC
		m.Spec.Metadata = map[string]string{"k": ks[rng.Intn(4)], "op": ops[rng.Intn(3)]}
		if rng.Intn(3) == 0 {
			m.Spec.Metadata["k2"] = ks[rng.Intn(4)]
		}
		if i < 40 {
			test := fmt.Sprintf("T%d", i%5)
			owned[test] = append(owned[test], m.Name)
			perTest = append(perTest, m)
		} else {
			session = append(session, m)
		}
	}
	return perTest, session, owned
}

// A miss asked by keys names the very recording the walk of every staged
// recording names, miss after miss, over random test opens, pool views and
// consumes: two managers staged with the same set, one asked by keys
// (FindStagedNotOwnedByKeys), the other without (FindStagedNotOwned), whose
// match accepts only a recording filed under one of the call's keys, as a
// parser's must (models.RegisterRequestKeys).
func TestAMissByKeysNamesWhatTheWalkNames(t *testing.T) {
	t.Cleanup(models.RegisterRequestKeys(models.GENERIC, keysOfMeta))
	for seed := int64(1); seed <= 4; seed++ {
		pt1, s1, owned := genericKeyedSet(seed)
		pt2, s2, _ := genericKeyedSet(seed)
		byKeys, walk := stagedManager(t, pt1, s1), stagedManager(t, pt2, s2)
		mocks := func(set []*models.Mock, names []string) []*models.Mock {
			var out []*models.Mock
			for _, mk := range set {
				if slices.Contains(names, mk.Name) {
					out = append(out, mk)
				}
			}
			return out
		}
		rng := rand.New(rand.NewSource(seed * 7919))
		named := 0
		for step := range 300 {
			switch r := rng.Intn(100); {
			case r < 15:
				test := fmt.Sprintf("T%d", rng.Intn(5))
				byKeys.ServeExactTest(test, mocks(pt1, owned[test]))
				walk.ServeExactTest(test, mocks(pt2, owned[test]))
			case r < 22:
				byKeys.ServeExactPool("k", nil)
				walk.ServeExactPool("k", nil)
			case r < 35:
				i := rng.Intn(len(pt1))
				if a, b := byKeys.DeleteFilteredMock(*pt1[i]), walk.DeleteFilteredMock(*pt2[i]); a != b {
					t.Fatalf("seed %d step %d: consuming %s gave %v asked by keys and %v by the walk", seed, step, pt1[i].Name, a, b)
				}
			default:
				keys := []string{"k:" + string(rune('a'+rng.Intn(4)))}
				if rng.Intn(2) == 0 {
					keys = append(keys, "k:"+string(rune('a'+rng.Intn(4))))
				}
				op := []string{"x", "y", "z"}[rng.Intn(3)]
				match := func(m *models.Mock) bool {
					in := false
					for _, k := range keysOfMeta(m) {
						in = in || slices.Contains(keys, k)
					}
					return in && m.Spec.Metadata["op"] == op
				}
				a := stagedNotOwnedByKeysNow(byKeys, models.GENERIC, keys, match)
				b := stagedNotOwnedNow(walk, models.GENERIC, match)
				if a != b {
					t.Fatalf("seed %d step %d: a miss under %v op %s named %q asked by keys, %q by the walk", seed, step, keys, op, a, b)
				}
				if a != "" {
					named++
				}
			}
		}
		if named < 20 {
			t.Fatalf("seed %d: only %d misses named a recording; the row checks too little", seed, named)
		}
	}
}
