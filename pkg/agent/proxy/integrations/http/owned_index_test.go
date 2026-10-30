package http

import (
	"context"
	"fmt"
	"math/rand"
	"slices"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// viewDb is a store that offers the shared part of its reads
// (integrations.SharedReads) as the manager does through one test: the open
// test's own recordings are one view whose list never changes while the test
// lasts (owned: the per-test read at the test's open, in its order), what is
// left of a position is what the store has not spent, and the shared part is
// the session read's rest. Its claims are exactGenDb's.
type viewDb struct {
	*exactGenDb
	owned  []*models.Mock
	shared []*models.Mock
}

func newViewDb(perTest, session []*models.Mock) *viewDb {
	return &viewDb{
		exactGenDb: &exactGenDb{newOwnGenDb(slices.Clone(perTest), slices.Clone(session))},
		owned:      slices.Clone(perTest),
		shared:     slices.Clone(session),
	}
}

func (d *viewDb) SharedGeneration(models.Kind) (integrations.SharedGen, bool) {
	return integrations.SharedGen{View: 1, Store: d.gen}, true
}

func (d *viewDb) SharedMocks() (perTest, session []*models.Mock, ok bool) {
	return nil, slices.Clone(d.shared), true
}

func (d *viewDb) OwnedUnanswered() ([]*models.Mock, bool) { return slices.Clone(d.perTest), true }

func (d *viewDb) OwnedRecordings() (uint64, []*models.Mock, bool) { return 1, d.owned, true }

func (d *viewDb) OwnedUnansweredAt(view uint64, at []int) ([]int, bool) {
	if view != 1 {
		return nil, false
	}
	return slices.DeleteFunc(slices.Clone(at), func(p int) bool { return d.spent[d.owned[p].Name] }), true
}

func (d *viewDb) GetSessionMocks() ([]*models.Mock, error) {
	return append(slices.Clone(d.perTest), d.shared...), nil
}

// Over random pools whose open test owns most of the recordings, among them
// recordings that learned a place of their URL (filed apart, under their
// method alone), a strict call answered through the index of the test's own
// recordings and of the shared part is answered with the recording a read of
// the store answers it with, and misses where a read misses; verify mode, on
// for the indexed side, compares every call's fits with a read's, in the
// read's order, where a call's own bucket and the URL learners interleave.
func TestAnOwnedIndexedStrictMatchIsTheOneAReadGives(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	was := integrations.ExactViewVerify.Load()
	t.Cleanup(func() { integrations.ExactViewVerify.Store(was) })
	interleaved := 0
	for seed := int64(1); seed <= randomStrictSeeds(); seed++ {
		var noise []string
		if seed%2 == 0 {
			noise = []string{`/q/[0-9]+`}
		}
		perTest, session := strictPoolOf(seed, 400)
		indexed, read := newHTTP(), newHTTP()
		ixDb := newViewDb(perTest, session)
		readDb := &exactGenDb{newOwnGenDb(slices.Clone(perTest), slices.Clone(session))}
		rnd := rand.New(rand.NewSource(seed * 7))
		for c := 0; c < 300; c++ {
			r := getTo(fmt.Sprintf("/p/%d/q/%d", rnd.Intn(7), rnd.Intn(4)), strictIxHost)
			if rnd.Intn(3) == 0 {
				r.method = "POST"
			}
			integrations.ExactViewVerify.Store(true)
			got := strictMatchOf(t, indexed, r, ixDb, ixDb, noise)
			integrations.ExactViewVerify.Store(false)
			want := strictMatchOf(t, read, r, readDb, nil, noise)
			if (got == nil) != (want == nil) || got != nil && got.Name != want.Name {
				t.Fatalf("seed %d call %d %s %s: indexed %v, read %v", seed, c, r.method, r.url.Path, got, want)
			}
			if own, has := indexed.strictIx.owned.last, indexed.strictIx.owned.has; has {
				s := newStrictSieve(r, indexed.newStrictNoise(nil, nil, noise))
				if exact, wide := own.byKey[s.method+" "+s.masked], own.byURL[s.method]; len(exact) > 0 && len(wide) > 0 && wide[0] < exact[len(exact)-1] {
					interleaved++
				}
			}
		}
		if !indexed.strictIx.owned.has || ixDb.refused != 0 {
			t.Fatalf("seed %d: no index of the test's own was built, or it offered %d recordings the store refused", seed, ixDb.refused)
		}
	}
	if want := int(100 * randomStrictSeeds() / 6); interleaved < want {
		t.Fatalf("only %d calls had their own bucket and a URL learner out of the list's order, want %d: the row compares too little", interleaved, want)
	}
}

// The group and the owner a learned call finds through the index of the
// open test's own recordings and of the shared part are those a read of the
// store gives, over random pools whose test owns most of them: recordings
// that learned a place of their URL or carry obfuscation noise (filed under
// their segment count too), recordings with variables (never placed), and
// recorded order that is not the list's (ties included), with consumes
// between calls.
func TestAnOwnedIndexedLearnedGroupIsTheOneAReadGives(t *testing.T) {
	h, _, _ := learnOn(t)
	verifyOff(t)
	rng := rand.New(rand.NewSource(23))
	paths := []string{"/v1/item/1", "/v2/item/1", "/v1/item/2", "/v1/item", "/v1/user/1", "/v3/item/7"}
	compared, grouped, wide, reordered := 0, 0, 0, 0
	for round := 0; round < 400; round++ {
		urlNoise := []string(nil)
		if rng.Intn(2) == 0 {
			urlNoise = []string{`v[0-9]+`}
		}
		n := h.newLearnNoise(models.OutgoingOptions{Learn: true}, nil, urlNoise)
		var perTest, session []*models.Mock
		for i := 0; i < 2+rng.Intn(14); i++ {
			method := []string{"GET", "POST"}[rng.Intn(2)]
			m := learnRecorded(fmt.Sprintf("r%d-%d", round, i), time.Duration(rng.Intn(4)), method, paths[rng.Intn(len(paths))], "", nil, `{}`)
			switch rng.Intn(6) {
			case 0:
				m.Spec.ReqBodyNoise = map[string][]string{"url.path.2": {}}
			case 1:
				m.Noise = []string{`^[0-9]$`}
			case 2:
				m.Spec.Variables = models.MockVariables{"x": {}}
			}
			if rng.Intn(4) > 0 {
				perTest = append(perTest, m)
			} else {
				m.TestModeInfo.Lifetime = models.LifetimeSession
				session = append(session, m)
			}
		}
		db := newViewDb(perTest, session)
		db.unplaced = rng.Intn(2) == 0
		ctx := context.Background()
		if rng.Intn(3) > 0 {
			tr := learn.NewTracker()
			tr.Begin()
			ctx = learn.WithTracker(ctx, tr)
		}
		for call := 0; call < 6; call++ {
			if len(db.perTest) > 0 && rng.Intn(3) == 0 {
				_ = db.DeleteFilteredMock(*db.perTest[rng.Intn(len(db.perTest))])
			}
			in := live([]string{"GET", "POST"}[rng.Intn(2)], paths[rng.Intn(len(paths))], "", nil)
			lp := liveParts(ctx, in)
			ls := n.shape(lp, sameDest)
			r, err := h.learnReads(db, learn.TrackerFrom(ctx))
			if err != nil {
				t.Fatal(err)
			}
			sc := r.scope(lp, ls, n, true)
			want, wantOwner := exactGroup(ls, sc, false), sc.owner()
			// Twice: a staging's second read builds the shared part's index.
			_, _, _ = h.learnedGroupOf(db, db, learn.TrackerFrom(ctx), lp, ls, n, false)
			got, gotOwner, err := h.learnedGroupOf(db, db, learn.TrackerFrom(ctx), lp, ls, n, false)
			if err != nil {
				t.Fatal(err)
			}
			if !h.learnIx.owned.has {
				t.Fatalf("round %d: no index of the test's own was built", round)
			}
			if !slices.Equal(got, want) {
				t.Fatalf("round %d call %s %s (url noise %v): indexed %v, read %v", round, in.method, in.url.Path, urlNoise, strictNames(got), strictNames(want))
			}
			for _, m := range got {
				if gotOwner(m) != wantOwner(m) {
					t.Fatalf("round %d: %s own %v through the index, %v from a read", round, m.Name, gotOwner(m), wantOwner(m))
				}
			}
			compared++
			if len(want) > 0 {
				grouped++
			}
			for _, m := range want {
				if learnsWide(m) && n.maskedPath(recordedParts(m).path) != n.learnSieveOf(lp).masked {
					wide++
					break
				}
			}
			if outOfListOrder(db.owned, want) {
				reordered++
			}
		}
	}
	if grouped < compared/5 || wide < 20 || reordered < 20 {
		t.Fatalf("%d of %d calls had a group, %d an own recording of another path whose segment may be excused, %d own recordings out of the list's order: the row compares too little", grouped, compared, wide, reordered)
	}
}

// outOfListOrder reports whether two of group, both of owned, come in the
// other order in owned.
func outOfListOrder(owned, group []*models.Mock) bool {
	last := -1
	for _, m := range group {
		p := slices.Index(owned, m)
		if p < 0 {
			continue
		}
		if p < last {
			return true
		}
		last = p
	}
	return false
}
