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
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

const strictIxHost = "localhost:50002"

// strictVisitsOf is how many recordings strict calls looked at while f ran.
func strictVisitsOf(f func()) int64 {
	before := strictVisits.Load()
	f()
	return strictVisits.Load() - before
}

// strictItems are size per-test recordings of GET /item/<i>?size=small.
func strictItems(prefix string, size int) []*models.Mock {
	mocks := make([]*models.Mock, size)
	for i := range mocks {
		m := onHost(fmt.Sprintf("%s-%d", prefix, i), "/item/"+strconv.Itoa(i)+"?size=small", strictIxHost)
		m.TestModeInfo.Lifetime = models.LifetimePerTest
		mocks[i] = answered(m, time.Duration(i)*time.Millisecond, 200, `{"id":`+strconv.Itoa(i)+`}`)
	}
	return mocks
}

// strictCall is the live GET of /item/<i>?size=small.
func strictCall(i int) *req {
	r := getTo("/item/"+strconv.Itoa(i), strictIxHost)
	r.url.RawQuery = "size=small"
	return r
}

// ownGenDb is genDb that tells whose a recording is (integrations.TestOwner),
// as every store that serves under the switch does: the open test's own are
// its per-test recordings, as the exact view's mapping names them.
type ownGenDb struct{ *genDb }

func (d ownGenDb) OwnedByOpenTest(m *models.Mock) bool {
	return m.TestModeInfo.Lifetime == models.LifetimePerTest
}

// newOwnGenDb is newGenDb that tells whose a recording is.
func newOwnGenDb(perTest, session []*models.Mock) *ownGenDb {
	return &ownGenDb{newGenDb(perTest, session)}
}

// strictMatchOf matches r against db as decode does under the switch: held
// from base.
func strictMatchOf(t *testing.T, h *HTTP, r *req, db, base integrations.MockMemDb, urlNoise []string) *models.Mock {
	t.Helper()
	ok, stub, _, err := h.matchOf(context.Background(), r, db, base, nil, nil, urlNoise, true, false, false, true, true)
	if err != nil {
		t.Fatalf("match: %v", err)
	}
	if !ok {
		return nil
	}
	return stub
}

// Under a staging generation a strict call compares only the recordings
// filed under its method and path: at 100 recordings and at 2000 alike, after
// the generation's second call built the index, a call looks at one
// recording and reads nothing of the store (each call read the store and
// walked every recording of the staged set). The recording a call took is
// spent, and the next call of its path is a miss.
func TestAStrictCallUnderAStagingGenerationLooksOnlyAtItsPath(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	verifyOff(t)
	for _, size := range []int{100, 2000} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			h := newHTTP()
			db := newOwnGenDb(strictItems("mock", size), nil)
			strictMatchOf(t, h, strictCall(0), db, db, nil)
			strictMatchOf(t, h, strictCall(1), db, db, nil)
			builds, reads := strictIndexBuilds.Load(), db.reads
			var served *models.Mock
			visits := strictVisitsOf(func() { served = strictMatchOf(t, h, strictCall(size/2), db, db, nil) })
			if served == nil || served.Name != fmt.Sprintf("mock-%d", size/2) {
				t.Fatalf("served %v; want mock-%d", served, size/2)
			}
			if visits != 1 || db.reads != reads {
				t.Fatalf("a call that is one recording of %d looked at %d and read the store %d times; want 1 and none", size, visits, db.reads-reads)
			}
			if strictIndexBuilds.Load() != builds {
				t.Fatal("a call of the same generation built another index")
			}
			if again := strictMatchOf(t, h, strictCall(size/2), db, db, nil); again != nil || db.refused != 0 {
				t.Fatalf("a spent recording answered again (%v) or was claimed (%d refused); want neither", again, db.refused)
			}
		})
	}
}

// A generation that moves (a staging, a test's open or close) is never read
// through the index of the one before, nor one of other url noise: the next
// set's recordings are found. A store held to a staging that moved
// (integrations.RestagedGeneration) keeps and uses no index, and neither does
// a call with no base store to keep one for.
func TestAStrictCallReadsTheIndexOfItsOwnGenerationAndURLNoise(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	verifyOff(t)
	h := newHTTP()
	db := newOwnGenDb(strictItems("a", 10), nil)
	for i := 0; i < 3; i++ {
		if s := strictMatchOf(t, h, strictCall(i), db, db, nil); s == nil || s.Name != "a-"+strconv.Itoa(i) {
			t.Fatalf("served %v; want a-%d", s, i)
		}
	}
	if !h.strictIx.has || h.strictIx.last.gen != 1 {
		t.Fatal("no index was kept for generation 1")
	}
	db.learnDb.perTest, db.gen = strictItems("b", 10), 2
	for i := 0; i < 3; i++ {
		if s := strictMatchOf(t, h, strictCall(i), db, db, nil); s == nil || s.Name != "b-"+strconv.Itoa(i) {
			t.Fatalf("after a staging: served %v; want b-%d", s, i)
		}
	}
	// url noise that masks the item's id files every item under one path:
	// an index of the noise before would find none of them.
	noise := []string{`/item/[0-9]+`}
	for i := 3; i < 6; i++ {
		if s := strictMatchOf(t, h, strictCall(i), db, db, noise); s == nil || s.Name != "b-3" && s.Name != "b-"+strconv.Itoa(i) {
			t.Fatalf("under url noise: served %v", s)
		}
	}
	if h.strictIx.last == nil || h.strictIx.key.url != urlNoiseKey(compileURLNoise(nil, noise)) {
		t.Fatal("no index was kept for the call's url noise")
	}
	db.gen = integrations.RestagedGeneration
	builds, reads := strictIndexBuilds.Load(), db.reads
	for i := 6; i < 9; i++ {
		strictMatchOf(t, h, strictCall(i), db, db, nil)
	}
	if strictIndexBuilds.Load() != builds || db.reads-reads != 3 {
		t.Fatalf("a store held to a staging that moved built %d indexes and read the store %d times in 3 calls; want none and 3", strictIndexBuilds.Load()-builds, db.reads-reads)
	}
	db.gen = 3
	other := newHTTP()
	for i := 6; i < 9; i++ {
		strictMatchOf(t, other, strictCall(i), db, nil, nil)
	}
	if other.strictIx.has {
		t.Fatal("a call with no base store kept an index")
	}
}

// strictPoolOf is a random pool of seed: per-test and session recordings of
// GET and POST on a few paths, some recorded twice, some that learned a
// place of their URL, with distinct answers.
func strictPoolOf(seed int64, size int) (perTest, session []*models.Mock) {
	rnd := rand.New(rand.NewSource(seed))
	for i := 0; i < size; i++ {
		path := fmt.Sprintf("/p/%d/q/%d", rnd.Intn(6), rnd.Intn(4))
		m := onHost(fmt.Sprintf("m-%d", i), path, strictIxHost)
		if rnd.Intn(3) == 0 {
			m.Spec.HTTPReq.Method = "POST"
		}
		if rnd.Intn(8) == 0 {
			m.Spec.ReqBodyNoise = map[string][]string{"url.path.1": {}}
		}
		answered(m, time.Duration(i)*time.Millisecond, 200, `{"n":`+strconv.Itoa(i)+`}`)
		if rnd.Intn(4) == 0 {
			m.TestModeInfo.Lifetime = models.LifetimeSession
			session = append(session, m)
			continue
		}
		m.TestModeInfo.Lifetime = models.LifetimePerTest
		perTest = append(perTest, m)
	}
	return perTest, session
}

// exactGenDb is genDb whose reusable recordings answer as the exact view's
// do: a claim keeps the recording the reads give, not the copy the parser
// reports (MockManager.updateUnFilteredMock).
type exactGenDb struct{ *ownGenDb }

func (d *exactGenDb) UpdateUnFilteredMock(old, _ *models.Mock) bool {
	for _, m := range d.mocks {
		if m == old {
			return true
		}
	}
	return false
}

// Over random pools and calls, under url noise or none, a call answered
// through the index is answered with the recording a read answers it with,
// and misses where a read misses; verify mode, on for the indexed side,
// compares every indexed call's fits with a read's.
func TestAnIndexedStrictMatchIsTheOneAReadGives(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	was := integrations.ExactViewVerify.Load()
	t.Cleanup(func() { integrations.ExactViewVerify.Store(was) })
	for seed := int64(1); seed <= 6; seed++ {
		var noise []string
		if seed%2 == 0 {
			noise = []string{`/q/[0-9]+`}
		}
		indexed, read := newHTTP(), newHTTP()
		ixDb := &exactGenDb{newOwnGenDb(strictPoolOf(seed, 400))}
		readDb := &exactGenDb{newOwnGenDb(strictPoolOf(seed, 400))}
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
		}
		if !indexed.strictIx.has || ixDb.refused != 0 {
			t.Fatalf("seed %d: no index was built, or it offered %d recordings the store refused", seed, ixDb.refused)
		}
	}
}

// An index that lost a recording a call is (here, its path's bucket) is
// caught in verify mode: the call's indexed fits differ from a read's, and it
// panics.
func TestAStrictIndexThatLostAMatchPanicsInVerifyMode(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	verifyOff(t)
	h := newHTTP()
	db := newOwnGenDb(strictItems("mock", 10), nil)
	strictMatchOf(t, h, strictCall(0), db, db, nil)
	strictMatchOf(t, h, strictCall(1), db, db, nil)
	for k := range h.strictIx.last.byPath {
		if strings.HasSuffix(k, "item/5") {
			delete(h.strictIx.last.byPath, k)
		}
	}
	integrations.ExactViewVerify.Store(true)
	defer func() {
		if recover() == nil {
			t.Fatal("verify mode did not catch an index that lost a match")
		}
	}()
	strictMatchOf(t, h, strictCall(5), db, db, nil)
}

// A recording the index lists that the store no longer hands out (its claim
// refused) is not chosen again: the call reads the store, which holds it no
// more, and misses after one refusal.
func TestARefusedClaimOfAnIndexedStrictRecordingRereadsTheStore(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	verifyOff(t)
	h := newHTTP()
	db := newOwnGenDb(strictItems("mock", 10), nil)
	strictMatchOf(t, h, strictCall(0), db, db, nil)
	strictMatchOf(t, h, strictCall(1), db, db, nil)
	// Another call took mock-5 without the store reporting it spent.
	for i, m := range db.learnDb.perTest {
		if m.Name == "mock-5" {
			db.learnDb.perTest = append(db.learnDb.perTest[:i:i], db.learnDb.perTest[i+1:]...)
			break
		}
	}
	reads := db.reads
	if s := strictMatchOf(t, h, strictCall(5), db, db, nil); s != nil {
		t.Fatalf("served %s, which the store no longer holds", s.Name)
	}
	if db.refused != 1 || db.reads == reads {
		t.Fatalf("refused %d claims and read the store %d times; want 1 refusal, then a read", db.refused, db.reads-reads)
	}
}

// spentGenDb is genDb that tells which per-test recordings a test kept for
// its repeats and is over with (integrations.SpentReader): still staged,
// never served.
type spentGenDb struct {
	*ownGenDb
	over map[string]bool
}

func (d *spentGenDb) Spent(m *models.Mock) bool { return d.over[m.Name] }

// A recording a test kept for its repeats and is over with is not served
// through the index, as a read drops it (integrations.WithoutSpent).
func TestASpentRecordingIsNotServedThroughTheStrictIndex(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	verifyOff(t)
	h := newHTTP()
	db := &spentGenDb{ownGenDb: newOwnGenDb(strictItems("mock", 10), nil), over: map[string]bool{}}
	strictMatchOf(t, h, strictCall(0), db, db, nil)
	strictMatchOf(t, h, strictCall(1), db, db, nil)
	db.over["mock-5"] = true
	if s := strictMatchOf(t, h, strictCall(5), db, db, nil); s != nil {
		t.Fatalf("served %s, which its test is over with", s.Name)
	}
	if s := strictMatchOf(t, h, strictCall(6), db, db, nil); s == nil || s.Name != "mock-6" {
		t.Fatalf("served %v; want mock-6", s)
	}
}

// A store that does not tell whose a recording is (integrations.TestOwner)
// keeps no strict index: a read serves each call, and answers it as it
// answers with no index to try.
func TestAStoreThatDoesNotTellOwnershipIsNotServedThroughTheStrictIndex(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	verifyOff(t)
	indexed, read := newHTTP(), newHTTP()
	ixDb := newGenDb(strictItems("own", 2), strictItems("shared", 6))
	readDb := newGenDb(strictItems("own", 2), strictItems("shared", 6))
	builds := strictIndexBuilds.Load()
	for i, want := range []string{"own-0", "own-1", "shared-4", "shared-4", "shared-4"} {
		c := i
		if i >= 2 {
			c = 4
		}
		got, again := strictMatchOf(t, indexed, strictCall(c), ixDb, ixDb, nil), strictMatchOf(t, read, strictCall(c), readDb, nil, nil)
		if got == nil || got.Name != want || again == nil || again.Name != want {
			t.Fatalf("call %d: served %v held from a base and %v from a read; want %s", i, got, again, want)
		}
	}
	if indexed.strictIx.has || strictIndexBuilds.Load() != builds || ixDb.reads != readDb.reads {
		t.Fatalf("index kept %v, %d built; the store was read %d times held from a base and %d from a read", indexed.strictIx.has, strictIndexBuilds.Load()-builds, ixDb.reads, readDb.reads)
	}
}

// genVarDb is varDb under exact serving with a staging generation.
type genVarDb struct {
	*varDb
	gen uint64
}

func (d *genVarDb) ExactServing() bool                         { return true }
func (d *genVarDb) StagingGeneration() uint64                  { return d.gen }
func (d *genVarDb) StagingGenerationByKind(models.Kind) uint64 { return d.gen }
func (d *genVarDb) MockSpent(name string) bool                 { return d.consumed[name] }

// OwnedByOpenTest: the open test's own are the per-test read's.
func (d *genVarDb) OwnedByOpenTest(m *models.Mock) bool { return slices.Contains(d.perTest, m) }

// A generation that stages a recording declaring variables is never served
// through the strict index: the variable pass serves those first, from a
// read. Here a reusable recording is the very create too, and the create,
// the generation's second call (the one that builds its index), is still
// answered by the recording that binds its variable.
func TestAGenerationWithVariablesIsNotServedThroughTheStrictIndex(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	verifyOff(t)
	h := newHTTP()
	body := `{"id":"ord-live-9","item":"pen"}`
	db := &genVarDb{varDb: &varDb{
		perTest: []*models.Mock{
			at(varMock("ping", "GET", "http://store/ping", "", 200, `{}`, nil), 1),
			at(varMock("bind", "POST", "http://store/orders", `{"id":"ord-rec-1","item":"pen"}`, 201, `{"id":"ord-rec-1"}`,
				models.MockVariables{"orderId": bindV("ord-rec-1", "body.id", "body.id")}), 2),
		},
		session: []*models.Mock{reusable(at(varMock("plain", "POST", "http://store/orders", body, 200, `{}`, nil), 3))},
	}, gen: 1}
	for _, c := range []struct{ method, path, body, want string }{{"GET", "/ping", "", "ping"}, {"POST", "/orders", body, "bind"}} {
		ok, stub, _, err := h.matchOf(context.Background(), liveReq(c.method, c.path, c.body), db, db, nil, nil, nil, true, false, false, true, true)
		if err != nil || !ok || stub.Name != c.want {
			t.Fatalf("%s %s: served %v (ok %v, err %v); want %s", c.method, c.path, stub, ok, err, c.want)
		}
	}
	if !h.strictIx.has || !h.strictIx.last.vars {
		t.Fatal("the generation's index was not built, or does not say it holds variables")
	}
}
