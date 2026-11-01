package http

import (
	"context"
	"fmt"
	"math/rand"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
)

// genDb is learnDb under exact serving with a staging generation: a consume
// drops the mock from the per-test read and reports it spent, and leaves the
// generation where it was (integrations.StagingGenerations). reads counts the
// per-test reads.
type genDb struct {
	*learnDb
	gen   uint64
	spent map[string]bool
	reads int
	// refused counts the claims refused (DeleteFilteredMock of a mock the
	// per-test read no longer holds).
	refused int
	// unplaced: the store tells of no recorded windows, so the tier
	// that holds a recording says whose it is (ownership).
	unplaced bool
}

func (d *genDb) InRecordedTestWindow(t time.Time) (bool, bool) {
	if d.unplaced {
		return false, false
	}
	return d.learnDb.InRecordedTestWindow(t)
}

func newGenDb(perTest, session []*models.Mock) *genDb {
	return &genDb{learnDb: &learnDb{perTest: perTest, mockMemDb: mockMemDb{mocks: session}}, gen: 1, spent: map[string]bool{}}
}

func (d *genDb) ExactServing() bool                         { return true }
func (d *genDb) StagingGeneration() uint64                  { return d.gen }
func (d *genDb) StagingGenerationByKind(models.Kind) uint64 { return d.gen }
func (d *genDb) MockSpent(name string) bool                 { return d.spent[name] }
func (d *genDb) GetPerTestMocksInWindow() ([]*models.Mock, error) {
	d.reads++
	return d.learnDb.GetPerTestMocksInWindow()
}
func (d *genDb) DeleteFilteredMock(m models.Mock) bool {
	if !d.learnDb.DeleteFilteredMock(m) {
		// A claim refused: counted, and from the fifth on the mock is
		// reported spent, so a call that would choose it forever ends.
		d.refused++
		if d.refused >= 5 {
			d.spent[m.Name] = true
		}
		return false
	}
	d.spent[m.Name] = true
	return true
}

// learnVisitsOf is how many recordings a learned call's scope placed while f
// ran.
func learnVisitsOf(f func()) int64 {
	before := learnVisits.Load()
	f()
	return learnVisits.Load() - before
}

// Under a staging generation a learned call reads only the recordings filed
// under its path: at 100 recordings and at 2000 alike, after the generation's
// second call built the index, a call places one recording and reads nothing
// of the store (each call placed every recording of the staged set, read
// whole twice). Each recording answers once: the one a call took is spent,
// and the next call of its path is a miss.
func TestALearnedCallUnderAStagingGenerationPlacesOnlyItsPath(t *testing.T) {
	h, n, _ := learnOn(t)
	verifyOff(t)
	for _, size := range []int{100, 2000} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			mocks := make([]*models.Mock, size)
			for i := range mocks {
				m := learnRecorded(fmt.Sprintf("mock-%d", i), time.Duration(i), "GET", "/item/"+strconv.Itoa(i)+"?size=small", "", map[string]string{"X-Trace": "t" + strconv.Itoa(i)}, `{"id":`+strconv.Itoa(i)+`}`)
				m.Spec.ReqBodyNoise = map[string][]string{"header.x-trace": {}}
				mocks[i] = m
			}
			db := newGenDb(mocks, nil)
			call := func(i int) *models.Mock {
				t.Helper()
				served, err := h.learnedMatch(context.Background(), live("GET", "/item/"+strconv.Itoa(i)+"?size=small", "", map[string]string{"X-Trace": "live"}), db, n, models.OutgoingOptions{})
				if err != nil {
					t.Fatalf("call %d: %v", i, err)
				}
				return served
			}
			call(0)
			call(1)
			builds := learnIndexBuilds.Load()
			reads := db.reads
			var served *models.Mock
			visits := learnVisitsOf(func() { served = call(size / 2) })
			if served == nil || served.Name != fmt.Sprintf("mock-%d", size/2) {
				t.Fatalf("served %v; want mock-%d", served, size/2)
			}
			if visits != 1 || db.reads != reads {
				t.Fatalf("a call that is one recording of %d placed %d and read the store %d times; want 1 and none", size, visits, db.reads-reads)
			}
			if learnIndexBuilds.Load() != builds {
				t.Fatalf("a call of the same generation built another index")
			}
			if again := call(size / 2); again != nil {
				t.Fatalf("a spent recording answered again: %s", again.Name)
			}
		})
	}
}

// A generation that moves (a staging, a test's open or close) is never read
// through the index of the one before: the next set's recordings are found,
// and a store held to a staging that moved (integrations.RestagedGeneration)
// keeps and uses no index.
func TestALearnedCallReadsTheIndexOfItsOwnGeneration(t *testing.T) {
	h, n, _ := learnOn(t)
	verifyOff(t)
	rec := func(name, path string) *models.Mock {
		m := learnRecorded(name, 0, "GET", path, "", map[string]string{"X-Trace": "t"}, `{}`)
		m.Spec.ReqBodyNoise = map[string][]string{"header.x-trace": {}}
		return m
	}
	db := newGenDb([]*models.Mock{rec("a-0", "/a"), rec("a-1", "/b")}, nil)
	get := func(path string) *models.Mock {
		served, err := h.learnedMatch(context.Background(), live("GET", path, "", map[string]string{"X-Trace": "live"}), db, n, models.OutgoingOptions{})
		if err != nil {
			t.Fatal(err)
		}
		return served
	}
	if get("/a") == nil || get("/b") == nil {
		t.Fatal("the first set's recordings were not served")
	}
	db.perTest, db.gen = []*models.Mock{rec("b-0", "/a"), rec("b-1", "/c")}, 2
	if s := get("/c"); s == nil || s.Name != "b-1" {
		t.Fatalf("after a staging: served %v; want b-1", s)
	}
	if s := get("/a"); s == nil || s.Name != "b-0" {
		t.Fatalf("after a staging: served %v; want b-0", s)
	}
	// A read the generation moved under is of no one generation: kept for
	// neither.
	db.gen = 3
	for i := 0; i < 2; i++ {
		_, ok := h.learnIx.of(db, db, n, "GET", "a", func() ([]*models.Mock, []*models.Mock, error) {
			db.gen = 4
			return db.perTest, nil, nil
		})
		if ok || h.learnIx.last != nil && h.learnIx.last.gen != 2 {
			t.Fatal("an index was kept from a read the generation moved under")
		}
		db.gen = 3
	}
	db.gen = integrations.RestagedGeneration
	if _, ok := h.learnIx.of(db, db, n, "GET", "a", func() ([]*models.Mock, []*models.Mock, error) { return db.perTest, nil, nil }); ok {
		t.Fatal("a store held to a staging that moved was read through an index")
	}
	if _, ok := h.learnIx.of(db, db, n, "GET", "a", func() ([]*models.Mock, []*models.Mock, error) { return db.perTest, nil, nil }); ok {
		t.Fatal("a store held to a staging that moved built an index")
	}
}

// The group and the owner a learned call finds through the index are those a
// read of the store gives, over random pools of both tiers (own and the
// pool's), paths of a few lengths and methods, keploy.yml's url noise or
// none, recordings that learned a place of their URL or carry obfuscation
// noise (filed under their segment count), recordings with variables (never
// placed), ties in the recorded order, and consumes between calls.
func TestAnIndexedLearnedGroupIsTheOneAReadGives(t *testing.T) {
	h, _, _ := learnOn(t)
	verifyOff(t)
	rng := rand.New(rand.NewSource(11))
	paths := []string{"/v1/item/1", "/v2/item/1", "/v1/item/2", "/v1/item", "/v1/user/1", "/v3/item/7"}
	compared, grouped := 0, 0
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
			if rng.Intn(2) == 0 {
				perTest = append(perTest, m)
			} else {
				m.TestModeInfo.Lifetime = models.LifetimeSession
				session = append(session, m)
			}
		}
		db := newGenDb(perTest, session)
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
			// Twice: the generation's second read builds the index.
			_, _, _ = h.learnedGroupOf(db, db, learn.TrackerFrom(ctx), lp, ls, n, false)
			got, gotOwner, err := h.learnedGroupOf(db, db, learn.TrackerFrom(ctx), lp, ls, n, false)
			if err != nil {
				t.Fatal(err)
			}
			if h.learnIx.last == nil || h.learnIx.last.store != integrations.MockMemDb(db) {
				t.Fatalf("round %d: no index was built", round)
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
		}
	}
	if grouped < compared/5 {
		t.Fatalf("only %d of %d calls had a group: the row compares too little", grouped, compared)
	}
}

// In the exact view's verify mode an index that lost a recording a read
// finds the call exactly panics, and one that did not passes.
func TestAnIndexThatLostAMatchPanicsInVerifyMode(t *testing.T) {
	h, n, _ := learnOn(t)
	was := integrations.ExactViewVerify.Load()
	integrations.ExactViewVerify.Store(true)
	t.Cleanup(func() { integrations.ExactViewVerify.Store(was) })
	m := learnRecorded("mock-0", 0, "GET", "/item/1", "", nil, `{}`)
	db := newGenDb([]*models.Mock{m}, nil)
	in := live("GET", "/item/1", "", nil)
	lp := liveParts(context.Background(), in)
	ls := n.shape(lp, sameDest)
	for i := 0; i < 2; i++ {
		if g, _, err := h.learnedGroupOf(db, db, nil, lp, ls, n, false); err != nil || len(g) != 1 {
			t.Fatalf("group %v err %v; want mock-0", strictNames(g), err)
		}
	}
	for k := range h.learnIx.last.byPath {
		if strings.HasSuffix(k, "item/1") {
			delete(h.learnIx.last.byPath, k)
		}
	}
	defer func() {
		if r := recover(); r == nil || !strings.Contains(fmt.Sprint(r), "indexed learned group") {
			t.Fatalf("recovered %v; want the verify mode's panic", r)
		}
	}()
	_, _, _ = h.learnedGroupOf(db, db, nil, lp, ls, n, false)
}

// An index is read with the url noise it was built with: a call of the same
// store and generation read with other url noise (keploy.yml's for another
// destination, or none) is found as a read finds it, through its own index.
func TestALearnedCallReadsTheIndexOfItsOwnURLNoise(t *testing.T) {
	h, _, _ := learnOn(t)
	verifyOff(t)
	masked := h.newLearnNoise(models.OutgoingOptions{Learn: true}, nil, []string{`v[0-9]+`})
	plain := h.newLearnNoise(models.OutgoingOptions{Learn: true}, nil, nil)
	db := newGenDb([]*models.Mock{learnRecorded("mock-0", 0, "GET", "/v1/item/1", "", nil, `{}`)}, nil)
	in := live("GET", "/v1/item/1", "", nil)
	lp := liveParts(context.Background(), in)
	for i, n := range []learnNoise{masked, masked, plain, plain, masked} {
		g, _, err := h.learnedGroupOf(db, db, nil, lp, n.shape(lp, sameDest), n, false)
		if err != nil || len(g) != 1 {
			t.Fatalf("call %d: group %v err %v; want mock-0", i, strictNames(g), err)
		}
	}
}

// A claim the store refuses on a recording the index lists (one the store no
// longer hands out, though it reported nothing spent) is not chosen again:
// the call reads the store from then on, and misses instead of looping.
func TestARefusedClaimOfAnIndexedRecordingRereadsTheStore(t *testing.T) {
	h, n, _ := learnOn(t)
	verifyOff(t)
	rec := func(name string) *models.Mock {
		m := learnRecorded(name, 0, "GET", "/item/1", "", map[string]string{"X-Trace": "t"}, `{}`)
		m.Spec.ReqBodyNoise = map[string][]string{"header.x-trace": {}}
		return m
	}
	db := newGenDb([]*models.Mock{rec("mock-0"), rec("mock-1")}, nil)
	call := func() *models.Mock {
		served, err := h.learnedMatch(context.Background(), live("GET", "/item/1", "", map[string]string{"X-Trace": "live"}), db, n, models.OutgoingOptions{})
		if err != nil {
			t.Fatal(err)
		}
		return served
	}
	if s := call(); s == nil || s.Name != "mock-0" {
		t.Fatalf("served %v; want mock-0", s)
	}
	if s := call(); s == nil || s.Name != "mock-1" {
		t.Fatalf("served %v; want mock-1 (through the index)", s)
	}
	// Both gone from the store, neither reported spent.
	db.spent = map[string]bool{}
	if s := call(); s != nil {
		t.Fatalf("served %s, which the store no longer holds", s.Name)
	}
	if db.refused != 1 {
		t.Fatalf("the store refused %d claims of the call; want 1, then a read of the store", db.refused)
	}
}

// learnTwinItem is a per-test recording of GET /item/<i>?size=small whose
// X-Trace header is learned noise.
func learnTwinItem(name string, i int) *models.Mock {
	m := learnRecorded(name, time.Duration(i), "GET", "/item/"+strconv.Itoa(i)+"?size=small", "", map[string]string{"X-Trace": "t" + strconv.Itoa(i)}, `{"id":`+strconv.Itoa(i)+`}`)
	m.Spec.ReqBodyNoise = map[string][]string{"header.x-trace": {}}
	return m
}

// newLearnLandingDb is ten learned recordings and twins of GET /item/77.
func newLearnLandingDb() landingDb {
	var mocks []*models.Mock
	for i := 0; i < 10; i++ {
		mocks = append(mocks, learnTwinItem(fmt.Sprintf("mock-%d", i), i))
	}
	for i := 0; i < 2; i++ {
		mocks = append(mocks, learnTwinItem(fmt.Sprintf("twin-%d", i), 77))
	}
	return &landingGenDb{ownGenDb: &ownGenDb{newGenDb(mocks, nil)}}
}

// learnLanding is a learning parser whose learned index the generation's
// first two calls of db built, and its call of the twins' request.
func learnLanding(t *testing.T) func(db landingDb) func() *models.Mock {
	return func(db landingDb) func() *models.Mock {
		h := newHTTP()
		opts := models.OutgoingOptions{Learn: true}
		n := h.newLearnNoise(opts, nil, nil)
		call := func(i int) *models.Mock {
			served, err := h.learnedMatch(context.Background(), live("GET", "/item/"+strconv.Itoa(i)+"?size=small", "", map[string]string{"X-Trace": "live"}), db, n, opts)
			if err != nil {
				t.Fatalf("call %d: %v", i, err)
			}
			return served
		}
		call(0)
		call(1)
		return func() *models.Mock { return call(77) }
	}
}

// So too of a learned call's verify (learnedGroupOf): the other call's
// consume of a twin, landing between any two of the call's reads of what is
// spent, is no difference of the learned index's group.
func TestAConsumeLandingInALearnedCallsVerifyIsNoDifference(t *testing.T) {
	_, _, _ = learnOn(t)
	verifyOff(t)
	sweepLandings(t, newLearnLandingDb, learnLanding(t), false, otherTwin(t))
}
