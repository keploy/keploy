package http

import (
	"context"
	"fmt"
	"math/rand"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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
	db.perTest, db.gen = strictItems("b", 10), 2
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

// randomStrictSeeds is how many random pools the strict-index rows draw:
// six, or two under -race (one under url noise, one without), where the rows
// test no concurrency and six take most of a minute of the race step; the
// plain run (go test ./...) draws all six.
func randomStrictSeeds() int64 {
	if raceOn {
		return 2
	}
	return 6
}

// Over random pools and calls, under url noise or none, a call answered
// through the index is answered with the recording a read answers it with,
// and misses where a read misses; verify mode, on for the indexed side,
// compares every indexed call's fits with a read's.
func TestAnIndexedStrictMatchIsTheOneAReadGives(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	was := integrations.ExactViewVerify.Load()
	t.Cleanup(func() { integrations.ExactViewVerify.Store(was) })
	for seed := int64(1); seed <= randomStrictSeeds(); seed++ {
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
	for i, m := range db.perTest {
		if m.Name == "mock-5" {
			db.perTest = append(db.perTest[:i:i], db.perTest[i+1:]...)
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

// A generation that stages a recording declaring variables keeps it aside
// in its strict index, and the variable pass serves it before the strict
// pass, as from a read. Here a reusable recording is the very create too,
// and the create, the generation's second call (the one that builds its
// index), is still answered by the recording that binds its variable, with
// the verify mode comparing what the variable pass read with a read's.
func TestAGenerationsRecordingWithVariablesIsServedBeforeItsStrictIndex(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
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
	if !h.strictIx.has || len(h.strictIx.last.vars) != 1 || h.strictIx.last.vars[0].Name != "bind" {
		t.Fatal("the generation's index was not built, or does not keep its recording with variables aside")
	}
}

// exactVarDb is genVarDb under exact serving's sequence view: the open
// test's view (SequenceViewer), every recording the store stages, spent ones
// included (StagedReader), and the background's cursor (BackgroundCursor),
// as the manager offers them in a test.
type exactVarDb struct {
	*genVarDb
	view   integrations.SequenceView
	staged []*models.Mock
	bg     *mockMemDb
	reads  int
}

func (d *exactVarDb) GetPerTestMocksInWindow() ([]*models.Mock, error) {
	d.reads++
	return d.genVarDb.GetPerTestMocksInWindow()
}
func (d *exactVarDb) SequenceView() (integrations.SequenceView, bool) { return d.view, true }
func (d *exactVarDb) StagedMocks() []*models.Mock                     { return d.staged }
func (d *exactVarDb) BackgroundCursor() (integrations.MockCursor, bool) {
	return d.bg, true
}

// A background poll whose recordings declare variables walks its recorded
// sequence through the staging generation's index as through a read: each
// call the next recording, in record order. Once the last recording can
// answer no more (here another call spent it), the poll's next call is a
// miss that names it, as a read reports it: the index serves no answer for
// an answered walk, and never an earlier recording again. With the verify
// mode on and off; the verify mode reads the store on every indexed call, so
// the reads are counted with it off.
func TestAnAnsweredWalkOfRecordingsWithVariablesIsAMissThroughTheIndex(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	for _, verify := range []bool{true, false} {
		t.Run(fmt.Sprintf("verify=%v", verify), func(t *testing.T) {
			if !verify {
				verifyOff(t)
			}
			answeredWalkThroughTheIndex(t, !verify)
		})
	}
}

func answeredWalkThroughTheIndex(t *testing.T, countReads bool) {
	h := newHTTP()
	polls := []*models.Mock{
		pollAt("poll-1", 1, "pending", models.MockVariables{"token": bindV("T", "body.token")}),
		pollAt("poll-2", 2, "pending", models.MockVariables{"token": useV("T", []string{"body.token"})}),
		pollAt("poll-3", 3, "running", models.MockVariables{"token": useV("T", []string{"body.token"})}),
		pollAt("poll-4", 4, "done", models.MockVariables{"token": useV("T", []string{"body.token"})}),
	}
	db := &exactVarDb{
		genVarDb: &genVarDb{varDb: &varDb{session: slices.Clone(polls)}, gen: 1},
		view:     integrations.SequenceView{Cursor: &mockMemDb{}, Test: "test-1", Start: recordedAt.Add(10 * time.Second)},
		staged:   slices.Clone(polls),
		bg:       &mockMemDb{},
	}
	call := func() (bool, *models.Mock, *matchDiag) {
		t.Helper()
		ok, stub, diag, err := h.matchOf(context.Background(), liveReq("POST", "http://svc/poll", `{"token":"L"}`), db, db, nil, nil, nil, true, false, false, true, true)
		if err != nil {
			t.Fatalf("match: %v", err)
		}
		return ok, stub, diag
	}
	for i, want := range []string{"poll-1", "poll-2", "poll-3"} {
		reads := db.reads
		if ok, stub, diag := call(); !ok || stub.Name != want {
			t.Fatalf("poll %d: served %v (ok %v, diag %+v); want %s", i+1, stub, ok, diag, want)
		}
		// The generation's second call builds its index; the third reads it.
		if countReads && i == 2 && db.reads != reads {
			t.Fatalf("poll 3 read the store %d times; want it served through the index", db.reads-reads)
		}
	}
	if !h.strictIx.has || len(h.strictIx.last.vars) != len(polls) {
		t.Fatal("the generation's index was not built, or does not keep the polls with variables aside")
	}
	// Another call spent poll-4: the store no longer offers it, but stages it.
	db.session = db.session[:3]
	db.consumed = map[string]bool{"poll-4": true}
	reads := db.reads
	ok, stub, diag := call()
	if ok {
		t.Fatalf("once poll-4 is spent the poll was answered %s; want a miss, never an earlier recording again", stub.Name)
	}
	if diag == nil || diag.answered == nil || diag.answered.last != "poll-4" {
		t.Fatalf("the miss %+v does not report the answered walk ending at poll-4", diag)
	}
	if countReads && db.reads == reads {
		t.Fatal("the answered walk was reported without a read; the read path reports it")
	}
}

// sharedVarDb is genVarDb offering the shared part of its reads
// (integrations.SharedReads) as the manager does in a test: its per-test
// read is the open test's unanswered recordings, its session read those then
// the shared session part, each once, at one SharedGen.
type sharedVarDb struct{ *genVarDb }

func (d *sharedVarDb) SharedGeneration(models.Kind) (integrations.SharedGen, bool) {
	return integrations.SharedGen{View: 1, Store: d.gen}, true
}

func (d *sharedVarDb) SharedMocks() (perTest, session []*models.Mock, ok bool) {
	return nil, slices.Clone(d.session), true
}

func (d *sharedVarDb) OwnedUnanswered() ([]*models.Mock, bool) { return slices.Clone(d.perTest), true }

func (d *sharedVarDb) OwnedRecordings() (uint64, []*models.Mock, bool) {
	return fakeOwnedView(d.OwnedUnanswered())
}

func (d *sharedVarDb) OwnedUnansweredAt(view uint64, at []int) ([]int, bool) {
	return fakeOwnedAt(view, at)
}

// fakeViews numbers the views of the fake stores' owned reads.
var fakeViews atomic.Uint64

// fakeOwnedView is a fake store's OwnedRecordings (integrations.SharedReads)
// from what its OwnedUnanswered reads now: each read is a view of its own,
// holding only unanswered recordings, so a parser files the test's own
// afresh on each call, and reads it at once (fakeOwnedAt).
func fakeOwnedView(owned []*models.Mock, ok bool) (uint64, []*models.Mock, bool) {
	if !ok {
		return 0, nil, false
	}
	return fakeViews.Add(1), owned, true
}

// fakeOwnedAt is a fake store's OwnedUnansweredAt of a view fakeOwnedView
// gave: every position of it, which the call read just before.
func fakeOwnedAt(view uint64, at []int) ([]int, bool) {
	if view == 0 {
		return nil, false
	}
	return slices.Clone(at), true
}

func (d *sharedVarDb) GetSessionMocks() ([]*models.Mock, error) {
	out := slices.Clone(d.perTest)
	for _, m := range d.session {
		if !slices.Contains(out, m) {
			out = append(out, m)
		}
	}
	return out, nil
}

// An open test's own recording that declares variables is served before the
// strict index of the shared part of its reads, as one in the staged set is
// before its generation's: the variable pass reads it first (here the
// create, which the shared part's plain recording would answer too). The
// learn step's index never places it: a call that is it exactly finds the
// group a read gives (the verify mode panics otherwise).
func TestAnOwnedRecordingWithVariablesIsServedBeforeTheSharedIndex(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	body, recorded := `{"id":"ord-live-9","item":"pen"}`, `{"id":"ord-rec-1","item":"pen"}`
	newDb := func() *sharedVarDb {
		return &sharedVarDb{&genVarDb{varDb: &varDb{
			perTest: []*models.Mock{
				at(varMock("ping", "GET", "http://store/ping", "", 200, `{}`, nil), 1),
				at(varMock("bind", "POST", "http://store/orders", recorded, 201, `{"id":"ord-rec-1"}`,
					models.MockVariables{"orderId": bindV("ord-rec-1", "body.id", "body.id")}), 2),
			},
			session: []*models.Mock{reusable(at(varMock("plain", "POST", "http://store/orders", body, 200, `{}`, nil), 3))},
		}, gen: 1}}
	}
	h, db := newHTTP(), newDb()
	for _, c := range []struct{ method, path, body, want string }{{"GET", "/ping", "", "ping"}, {"POST", "/orders", body, "bind"}} {
		ok, stub, _, err := h.matchOf(context.Background(), liveReq(c.method, c.path, c.body), db, db, nil, nil, nil, true, false, false, true, true)
		if err != nil || !ok || stub.Name != c.want {
			t.Fatalf("%s %s: served %v (ok %v, err %v); want %s", c.method, c.path, stub, ok, err, c.want)
		}
	}
	if !h.strictIx.shared.has {
		t.Fatal("no index of the shared part was built")
	}
	integrations.ExactViewVerify.Store(true)
	hl, n, _ := learnOn(t)
	if _, err := hl.learnedMatch(context.Background(), liveReq("POST", "/orders", recorded), newDb(), n, models.OutgoingOptions{}); err != nil {
		t.Fatal(err)
	}
	if !hl.learnIx.shared.has {
		t.Fatal("no learn index of the shared part was built")
	}
}

// readCountedVarDb is genVarDb that counts its per-test reads.
type readCountedVarDb struct {
	*genVarDb
	reads int
}

func (d *readCountedVarDb) GetPerTestMocksInWindow() ([]*models.Mock, error) {
	d.reads++
	return d.genVarDb.GetPerTestMocksInWindow()
}

// orderBind is a per-test recording of POST /orders on the strict index's
// host that binds variable orderId at its body's id.
func orderBind() *models.Mock {
	v := at(varMock("bind", "POST", "http://"+strictIxHost+"/orders", `{"id":"ord-rec-1"}`, 201, `{"id":"ord-rec-1"}`,
		models.MockVariables{"orderId": bindV("ord-rec-1", "body.id", "body.id")}), 1)
	v.TestModeInfo.Lifetime = models.LifetimePerTest
	return v
}

// One recording that declares variables, staged or the open test's own,
// keeps every other call on the strict index: the variable pass reads the
// index's recordings with variables (here that one), and a call none of them
// fits compares only its bucket and reads no store, at 100 recordings as at
// 2000 (its own path in each read through the shared part's index).
func TestARecordingWithVariablesKeepsTheOtherCallsOnTheStrictIndex(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	verifyOff(t)
	for _, size := range []int{100, 2000} {
		for _, shared := range []bool{false, true} {
			t.Run(fmt.Sprintf("%d recordings, shared %v", size, shared), func(t *testing.T) {
				h := newHTTP()
				own := newOwnGenDb(append(strictItems("mock", size), orderBind()), nil)
				var db integrations.MockMemDb = own
				want := int64(1)
				if shared {
					db, want = &sharedOwnDb{ownGenDb: own}, 2
				}
				strictMatchOf(t, h, strictCall(0), db, db, nil)
				strictMatchOf(t, h, strictCall(1), db, db, nil)
				reads := own.reads
				visits := strictVisitsOf(func() {
					if m := strictMatchOf(t, h, strictCall(2), db, db, nil); m == nil || m.Name != "mock-2" {
						t.Fatalf("served %v; want mock-2", m)
					}
				})
				if visits != want || own.reads != reads {
					t.Fatalf("a strict call compared %d recordings and read the store %d times; want %d and 0", visits, own.reads-reads, want)
				}
			})
		}
	}
}

// A call a generation's recording with variables answers is served through
// the index too: the variable pass reads the index's recordings with
// variables, and the store is not read.
func TestARecordingWithVariablesIsServedThroughTheIndexWithoutARead(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	verifyOff(t)
	h := newHTTP()
	db := &readCountedVarDb{genVarDb: &genVarDb{varDb: &varDb{perTest: append(strictItems("mock", 50), orderBind())}, gen: 1}}
	strictMatchOf(t, h, strictCall(0), db, db, nil)
	strictMatchOf(t, h, strictCall(1), db, db, nil) // the generation's second call builds its index
	reads := db.reads
	ok, stub, _, err := h.matchOf(context.Background(), liveReq("POST", "http://"+strictIxHost+"/orders", `{"id":"ord-live-9"}`), db, db, nil, nil, nil, true, false, false, true, true)
	if err != nil || !ok || stub.Name != "bind" {
		t.Fatalf("served %v (ok %v, err %v); want bind", stub, ok, err)
	}
	if db.reads != reads {
		t.Fatalf("the call read the store %d times; want 0", db.reads-reads)
	}
	if v, _ := db.table.Lookup("orderId"); v.Value != "ord-live-9" {
		t.Fatalf("orderId bound to %q; want ord-live-9", v.Value)
	}
}

// An index that lost a recording with variables (here the generation's one)
// is caught in verify mode: what the call's variable pass reads through the
// index differs from a read's, and it panics.
func TestAStrictIndexThatLostAVariableRecordingPanicsInVerifyMode(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	verifyOff(t)
	h := newHTTP()
	db := newOwnGenDb(append(strictItems("mock", 10), orderBind()), nil)
	strictMatchOf(t, h, strictCall(0), db, db, nil)
	strictMatchOf(t, h, strictCall(1), db, db, nil) // the generation's second call builds its index
	h.strictIx.last.vars, h.strictIx.last.varSplit = nil, 0
	integrations.ExactViewVerify.Store(true)
	defer func() {
		if r, _ := recover().(string); !strings.Contains(r, "exact view verify: indexed variable recordings") {
			t.Fatalf("verify mode did not catch an index that lost a recording with variables (recovered %q)", r)
		}
	}()
	strictMatchOf(t, h, strictCall(5), db, db, nil)
}

// spentVarDb is genVarDb that tells which recordings a test kept for its
// repeats and is over with (integrations.SpentReader): still staged, never
// served.
type spentVarDb struct {
	*genVarDb
	over map[string]bool
}

func (d *spentVarDb) Spent(m *models.Mock) bool { return d.over[m.Name] }

// A recording with variables a test kept for its repeats and is over with is
// not served through the index, as a read drops it (integrations.WithoutSpent):
// the call it would answer is a miss, and the calls the index serves go on.
func TestASpentRecordingWithVariablesIsNotServedThroughTheStrictIndex(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	verifyOff(t)
	h := newHTTP()
	db := &spentVarDb{genVarDb: &genVarDb{varDb: &varDb{perTest: append(strictItems("mock", 10), orderBind())}, gen: 1}, over: map[string]bool{}}
	strictMatchOf(t, h, strictCall(0), db, db, nil)
	strictMatchOf(t, h, strictCall(1), db, db, nil) // the generation's second call builds its index
	db.over["bind"] = true
	if s := strictMatchOf(t, h, liveReq("POST", "http://"+strictIxHost+"/orders", `{"id":"ord-live-9"}`), db, db, nil); s != nil {
		t.Fatalf("served %s, which its test is over with", s.Name)
	}
	if s := strictMatchOf(t, h, strictCall(6), db, db, nil); s == nil || s.Name != "mock-6" {
		t.Fatalf("served %v; want mock-6", s)
	}
}

// claimCountedVarDb is genVarDb that counts the claims of its per-test
// recordings.
type claimCountedVarDb struct {
	*genVarDb
	claims int
}

func (d *claimCountedVarDb) DeleteFilteredMock(m models.Mock) bool {
	d.claims++
	return d.genVarDb.DeleteFilteredMock(m)
}

// A recording with variables the store handed out is not claimed again
// through the index, which still lists it while its generation holds: the
// call it fitted, made again, misses with no claim.
func TestARecordingWithVariablesTheStoreHandedOutIsNotClaimedAgainThroughTheIndex(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	verifyOff(t)
	h := newHTTP()
	db := &claimCountedVarDb{genVarDb: &genVarDb{varDb: &varDb{perTest: append(strictItems("mock", 10), orderBind())}, gen: 1}}
	strictMatchOf(t, h, strictCall(0), db, db, nil)
	strictMatchOf(t, h, strictCall(1), db, db, nil) // the generation's second call builds its index
	create := func() *req { return liveReq("POST", "http://"+strictIxHost+"/orders", `{"id":"ord-live-9"}`) }
	if s := strictMatchOf(t, h, create(), db, db, nil); s == nil || s.Name != "bind" {
		t.Fatalf("served %v; want bind", s)
	}
	claims := db.claims
	if s := strictMatchOf(t, h, create(), db, db, nil); s != nil {
		t.Fatalf("served %s again", s.Name)
	}
	if db.claims != claims {
		t.Fatalf("the call made again claimed %d recordings; want none", db.claims-claims)
	}
}

// beginDb is genVarDb on which the next test begins once, armed, as the
// strict pass reads its index (strictIndexes.of): that read is of the
// previous test's pool and gives its generation, and then the next test's
// pool is swapped in, its variables reset and the generation moved.
type beginDb struct {
	*genVarDb
	armed bool
	next  []*models.Mock
}

func (d *beginDb) StagingGenerationByKind(k models.Kind) uint64 {
	gen := d.genVarDb.StagingGenerationByKind(k)
	if d.armed && calledFrom("(*strictIndexes).of") {
		d.armed = false
		d.perTest, d.gen = d.next, d.gen+1
		d.table.Reset()
	}
	return gen
}

// calledFrom reports whether a function whose name ends in fn is on the
// caller's stack.
func calledFrom(fn string) bool {
	pcs := make([]uintptr, 32)
	frames := runtime.CallersFrames(pcs[:runtime.Callers(2, pcs)])
	for {
		f, more := frames.Next()
		if strings.HasSuffix(f.Function, fn) {
			return true
		}
		if !more {
			return false
		}
	}
}

// A call the variable pass checks through the index of the previous test's
// pool binds nothing in the test that began as the index was read: the
// variables' scope is read before the index, so the bind is stale and the
// call reads the store again, which holds the next test's pool (none here).
func TestACallThroughTheIndexOfThePreviousPoolBindsNothingInTheNextTest(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	h := newHTTP()
	db := &beginDb{genVarDb: &genVarDb{varDb: &varDb{perTest: append(strictItems("mock", 10), orderBind())}, gen: 1}}
	strictMatchOf(t, h, strictCall(0), db, db, nil)
	strictMatchOf(t, h, strictCall(1), db, db, nil) // the generation's second call builds its index
	db.armed = true
	if s := strictMatchOf(t, h, liveReq("POST", "http://"+strictIxHost+"/orders", `{"id":"straggler"}`), db, db, nil); s != nil {
		t.Fatalf("the straggler was served %s, the previous test's", s.Name)
	}
	if db.armed {
		t.Fatal("the next test did not begin as the index was read")
	}
	if got := db.table.Snapshot(); len(got) != 0 {
		t.Fatalf("the straggler bound %+v in the next test", got)
	}
}

// racedSharedDb is sharedVarDb whose owned read meets a test's close (no test
// open: false) or a staging (the shared part moves, its session read
// replaced) once, between the call's sample of the shared generation and its
// read of the open test's recordings.
type racedSharedDb struct {
	*sharedVarDb
	closeOnce  bool
	restage    []*models.Mock
	ownedReads int
}

func (d *racedSharedDb) OwnedRecordings() (uint64, []*models.Mock, bool) {
	d.ownedReads++
	if d.ownedReads == 1 && d.closeOnce {
		return 0, nil, false
	}
	view, owned, ok := d.sharedVarDb.OwnedRecordings()
	if d.ownedReads == 1 && d.restage != nil {
		d.session, d.gen = d.restage, d.gen+1
	}
	return view, owned, ok
}

// A call whose read of the open test's recordings meets the test's close, or
// a staging that moves the shared part, reads the store: the index of the
// shared part is not read beside what no one test owns, nor beside a staging
// it is not of. Here a create the test recorded and the shared part holds
// alike is the test's own from a read; through the index without the test's
// recordings it would be the shared one, and through the index of the old
// staging the replaced one.
func TestACallWhoseOwnedReadMeetsACloseOrAStagingReadsTheStore(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	verifyOff(t)
	body := `{"item":"pen"}`
	for _, c := range []struct {
		name string
		race func(*racedSharedDb)
		want string
	}{
		{"a close", func(d *racedSharedDb) { d.closeOnce = true }, "own"},
		{"a staging", func(d *racedSharedDb) {
			d.perTest = nil
			d.restage = []*models.Mock{reusable(at(varMock("plain-2", "POST", "http://store/orders", body, 200, `{}`, nil), 4))}
		}, "plain-2"},
	} {
		t.Run(c.name, func(t *testing.T) {
			db := &racedSharedDb{sharedVarDb: &sharedVarDb{&genVarDb{varDb: &varDb{
				perTest: []*models.Mock{at(varMock("own", "POST", "http://store/orders", body, 201, `{}`, nil), 1)},
				session: []*models.Mock{reusable(at(varMock("plain", "POST", "http://store/orders", body, 200, `{}`, nil), 3))},
			}, gen: 1}}}
			c.race(db)
			h := newHTTP()
			ok, stub, _, err := h.matchOf(context.Background(), liveReq("POST", "/orders", body), db, db, nil, nil, nil, true, false, false, true, true)
			if err != nil || !ok || stub.Name != c.want {
				t.Fatalf("served %v (ok %v, err %v); want %s", stub, ok, err, c.want)
			}
		})
	}
}

// sharedOwnDb is ownGenDb offering the shared part of its reads
// (integrations.SharedReads) as the manager does in a test: the open test's
// own are its per-test read, the shared part (shared) its session read's
// rest.
type sharedOwnDb struct {
	*ownGenDb
	shared []*models.Mock
}

func (d *sharedOwnDb) SharedGeneration(models.Kind) (integrations.SharedGen, bool) {
	return integrations.SharedGen{View: 1, Store: d.gen}, true
}

func (d *sharedOwnDb) SharedMocks() (perTest, session []*models.Mock, ok bool) {
	return nil, slices.Clone(d.shared), true
}

func (d *sharedOwnDb) OwnedUnanswered() ([]*models.Mock, bool) { return slices.Clone(d.perTest), true }

func (d *sharedOwnDb) OwnedRecordings() (uint64, []*models.Mock, bool) {
	return fakeOwnedView(d.OwnedUnanswered())
}

func (d *sharedOwnDb) OwnedUnansweredAt(view uint64, at []int) ([]int, bool) {
	return fakeOwnedAt(view, at)
}

func (d *sharedOwnDb) GetSessionMocks() ([]*models.Mock, error) {
	return append(slices.Clone(d.perTest), d.shared...), nil
}

// Through the index of the shared part of a test's reads a call compares,
// of the test's own recordings, those of its path only, in each read that
// holds them (the per-test read and the session read): a test that owns 300
// recordings costs a call two strict comparisons and two learn placements,
// not one of each per recording it owns.
func TestACallThroughTheSharedIndexComparesOnlyTheOwnRecordingsOfItsPath(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	verifyOff(t)
	db := &sharedOwnDb{ownGenDb: newOwnGenDb(strictItems("own", 300), nil)}
	h := newHTTP()
	if v := strictVisitsOf(func() {
		if got := strictMatchOf(t, h, strictCall(150), db, db, nil); got == nil || got.Name != "own-150" {
			t.Fatalf("served %v; want own-150", got)
		}
	}); v != 2 {
		t.Fatalf("a strict call compared %d recordings; want 2 (own-150 in each read)", v)
	}
	if !h.strictIx.shared.has {
		t.Fatal("no index of the shared part was built")
	}
	hl, n, _ := learnOn(t)
	if v := learnVisitsOf(func() {
		if _, err := hl.learnedMatch(context.Background(), strictCall(151), db, n, models.OutgoingOptions{}); err != nil {
			t.Fatal(err)
		}
	}); v != 2 {
		t.Fatalf("a learned call placed %d recordings; want 2 (own-151 in each read)", v)
	}
}

// landings is what other calls do while the call under test runs, keyed by
// the call's reads of what the store reports spent (MockSpent and
// SpentReader.Spent, each one point): the change at point p lands just after
// the p-th read answered, as another call's consume, an ended test's Spend,
// or a test's end, lands between two reads of a call that runs beside it.
// took is the twin the landing consumed or spent, fired how many of at ran,
// and ended what ended tests spent, as SpentReader reports it.
type landings struct {
	at     map[int]func()
	points int
	fired  int
	took   string
	ended  map[string]bool
}

func (l *landings) point() {
	l.points++
	if f := l.at[l.points]; f != nil {
		l.fired++
		f()
	}
}

// sweep is the store's landings (landingDb).
func (l *landings) sweep() *landings { return l }

// spend is an ended test's Spend of the first of names it has not spent:
// SpentReader reports it spent from then on, and, as MockManager.Spend, it
// moves no generation and consumes nothing.
func (l *landings) spend(names ...string) func() {
	return func() {
		for _, name := range names {
			if !l.ended[name] {
				if l.ended == nil {
					l.ended = map[string]bool{}
				}
				l.ended[name], l.took = true, name
				return
			}
		}
	}
}

// spentByAnEndedTest is SpentReader.Spent of what ended tests spent, one
// point.
func (l *landings) spentByAnEndedTest(m *models.Mock) bool {
	spent := l.ended[m.Name]
	l.point()
	return spent
}

// landingDb is a store under landings: take is another call's consume of
// the first of names the store still holds, end a test's end, which moves
// the staging generation and installs a view that reports nothing of the
// last one spent.
type landingDb interface {
	integrations.MockMemDb
	sweep() *landings
	take(names ...string) func()
	end()
}

// landingGenDb is ownGenDb under landings.
type landingGenDb struct {
	*ownGenDb
	landings
}

func (d *landingGenDb) MockSpent(name string) bool {
	spent := d.ownGenDb.MockSpent(name)
	d.point()
	return spent
}

func (d *landingGenDb) Spent(m *models.Mock) bool { return d.spentByAnEndedTest(m) }

func (d *landingGenDb) take(names ...string) func() {
	return func() {
		for _, name := range names {
			for _, m := range d.perTest {
				if m.Name == name && d.DeleteFilteredMock(*m) {
					d.took = name
					return
				}
			}
		}
	}
}

func (d *landingGenDb) end() { d.gen, d.spent = d.gen+1, map[string]bool{} }

// landingVarDb is genVarDb under landings.
type landingVarDb struct {
	*genVarDb
	landings
}

func (d *landingVarDb) MockSpent(name string) bool {
	spent := d.genVarDb.MockSpent(name)
	d.point()
	return spent
}

func (d *landingVarDb) Spent(m *models.Mock) bool { return d.spentByAnEndedTest(m) }

func (d *landingVarDb) take(names ...string) func() {
	return func() {
		for _, name := range names {
			for _, m := range d.perTest {
				if m.Name == name && d.DeleteFilteredMock(*m) {
					d.took = name
					return
				}
			}
		}
	}
}

func (d *landingVarDb) end() { d.gen, d.consumed = d.gen+1, map[string]bool{} }

// strictTwins are two per-test recordings of one GET /item/77?size=small,
// the call recorded twice, beside strictItems' ten.
func strictTwins() []*models.Mock {
	mocks := strictItems("mock", 10)
	for i := 0; i < 2; i++ {
		m := onHost(fmt.Sprintf("twin-%d", i), "/item/77?size=small", strictIxHost)
		m.TestModeInfo.Lifetime = models.LifetimePerTest
		mocks = append(mocks, answered(m, time.Duration(100+i)*time.Millisecond, 200, `{"id":77}`))
	}
	return mocks
}

// bindTwins are two per-test recordings of one POST /orders that binds
// orderId (orderBind), the call recorded twice, beside strictItems' ten.
func bindTwins() []*models.Mock {
	mocks := strictItems("mock", 10)
	for i := 0; i < 2; i++ {
		m := orderBind()
		m.Name = fmt.Sprintf("twin-%d", i)
		mocks = append(mocks, m)
	}
	return mocks
}

// landingRun is call in verify mode, with what verify mode panicked with, if
// it did.
func landingRun(call func() *models.Mock) (served *models.Mock, panicked any) {
	integrations.ExactViewVerify.Store(true)
	defer func() {
		integrations.ExactViewVerify.Store(false)
		panicked = recover()
	}()
	return call(), nil
}

// strictLanding is a parser whose strict index the generation's first two
// calls of db built, and its call of the strict twins' request.
func strictLanding(t *testing.T) func(db landingDb) func() *models.Mock {
	return func(db landingDb) func() *models.Mock {
		h := newHTTP()
		strictMatchOf(t, h, strictCall(0), db, db, nil)
		strictMatchOf(t, h, strictCall(1), db, db, nil)
		return func() *models.Mock { return strictMatchOf(t, h, strictCall(77), db, db, nil) }
	}
}

// varLanding is strictLanding of the bind twins' request.
func varLanding(t *testing.T) func(db landingDb) func() *models.Mock {
	return func(db landingDb) func() *models.Mock {
		h := newHTTP()
		strictMatchOf(t, h, strictCall(0), db, db, nil)
		strictMatchOf(t, h, strictCall(1), db, db, nil)
		return func() *models.Mock {
			return strictMatchOf(t, h, liveReq("POST", "http://"+strictIxHost+"/orders", `{"id":"ord-live-9"}`), db, db, nil)
		}
	}
}

// newStrictLandingDb and newVarLandingDb are the twins' stores.
func newStrictLandingDb() landingDb {
	return &landingGenDb{ownGenDb: newOwnGenDb(strictTwins(), nil)}
}

func newVarLandingDb() landingDb {
	return &landingVarDb{genVarDb: &genVarDb{varDb: &varDb{perTest: bindTwins()}, gen: 1}}
}

// consumeLanding and spendLanding are what a sweep lands at a point of a
// call's reads: another call's consume of the first twin of order the store
// still holds (take), or an ended test's Spend of the first it has not spent
// (landings.spend).
func consumeLanding(db landingDb, order ...string) func() { return db.take(order...) }

func spendLanding(db landingDb, order ...string) func() { return db.sweep().spend(order...) }

// sweepLandings runs, on a fresh store of newDb, the call prepare readies
// (after the generation's first calls built its index), once for each of the
// call's own reads of what is spent, with land (of one of the twins, the
// first it can of either order) landing just after that read, and with end,
// once for each later read with the test's end landing just after it too.
// Only runs whose landings all fired count, and the sweep fails unless least
// of them did, least being what the call's reads made when its row was
// written: a call that reads what is spent at fewer points, or reads it off
// the instrumented methods, sweeps fewer. check, when there is one, is
// handed each run's answer, with the twin land took, when no test ended.
func sweepLandings(t *testing.T, newDb func() landingDb, prepare func(landingDb) func() *models.Mock, land func(db landingDb, order ...string) func(), withEnd bool, least int, check func(served *models.Mock, took string)) {
	t.Helper()
	runs := 0
	for _, order := range [][]string{{"twin-0", "twin-1"}, {"twin-1", "twin-0"}} {
		for at := 1; ; at++ {
			landed := false
			for end := at + 1; ; end++ {
				db := newDb()
				call := prepare(db)
				l := db.sweep()
				// Points count the call's own reads, not prepare's.
				l.points = 0
				l.at = map[int]func(){at: land(db, order...)}
				if withEnd {
					l.at[end] = db.end
				}
				served, panicked := landingRun(call)
				if panicked != nil {
					t.Fatalf("%v: a landing at point %d (test end at %d: %v) of %d made verify mode panic: %v", order, at, end, withEnd, l.points, panicked)
				}
				if l.fired == 0 {
					// at is past the call's last read.
					break
				}
				if l.took == "" {
					t.Fatalf("%v: the landing at point %d of %d took no twin", order, at, l.points)
				}
				landed = true
				if !withEnd {
					runs++
					if check != nil {
						check(served, l.took)
					}
					break
				}
				if l.fired < 2 {
					// end is past the call's last read.
					break
				}
				runs++
			}
			if !landed {
				break
			}
		}
	}
	if runs < least {
		t.Fatalf("%d runs had their landings fire; want %d at least, as many as the call's reads of what is spent made", runs, least)
	}
}

// otherTwin fails t unless served is the twin the other call did not take.
func otherTwin(t *testing.T) func(served *models.Mock, took string) {
	return func(served *models.Mock, took string) {
		t.Helper()
		if served == nil || !strings.HasPrefix(served.Name, "twin-") || served.Name == took {
			t.Fatalf("served %v while another call took %s; want the other twin", served, took)
		}
	}
}

// aTwin fails t unless the call was answered with one of the twins.
func aTwin(t *testing.T) func(served *models.Mock, took string) {
	return func(served *models.Mock, took string) {
		t.Helper()
		if served == nil || !strings.HasPrefix(served.Name, "twin-") {
			t.Fatalf("served %v (an ended test spent %s); want a twin", served, took)
		}
	}
}

// Two calls of one request the test recorded twice run at once, under
// verify mode: the other call's consume of a twin may land between any two
// of this call's reads of what the store reports spent, among them the
// verify's own reads after its read of the store. The verify drops a twin
// it reads spent from both what the index found and what the read found, or
// from neither, so a consume that lands between its reads is no difference
// of the index, and no panic; the call is answered with the other twin.
func TestAConsumeLandingInAStrictCallsVerifyIsNoDifference(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	verifyOff(t)
	sweepLandings(t, newStrictLandingDb, strictLanding(t), consumeLanding, false, 56, otherTwin(t))
}

// So too of the variable pass's verify: the other call's consume of a
// recording that binds a variable, landing between any two of a call's
// reads of what is spent, is no difference of the index.
func TestAConsumeLandingInAVariableCallsVerifyIsNoDifference(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	verifyOff(t)
	sweepLandings(t, newVarLandingDb, varLanding(t), consumeLanding, false, 36, otherTwin(t))
}

// An ended test's Spend of a twin (SpentReader reports it spent; the
// generation does not move) may land between any two of a strict call's
// reads of what is spent too. The strict verify reads that account once per
// twin as well, so the Spend is no difference of the index, and no panic.
// Which twin the call is answered with is not this row's: it asserts only
// that the call is answered with one.
func TestASpendLandingInAStrictCallsVerifyIsNoDifference(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	verifyOff(t)
	sweepLandings(t, newStrictLandingDb, strictLanding(t), spendLanding, false, 56, aTwin(t))
}

// So too of the variable pass's verify under an ended test's Spend.
func TestASpendLandingInAVariableCallsVerifyIsNoDifference(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	verifyOff(t)
	sweepLandings(t, newVarLandingDb, varLanding(t), spendLanding, false, 36, aTwin(t))
}

// A test that ends while a call verifies (its staging generation moved, and
// the view that reads what is spent is the next one's) leaves nothing to
// compare: the verify compares only what it read while the call's
// generation held, from its read of the store to its last read of what is
// spent, whatever another call consumed before.
func TestATestEndLandingInACallsVerifyIsNotCompared(t *testing.T) {
	_, _, _ = learnOn(t)
	verifyOff(t)
	sweepLandings(t, newStrictLandingDb, strictLanding(t), consumeLanding, true, 796, nil)
	sweepLandings(t, newVarLandingDb, varLanding(t), consumeLanding, true, 426, nil)
	sweepLandings(t, newLearnLandingDb, learnLanding(t), consumeLanding, true, 11, nil)
}

// lockedGenDb is ownGenDb that calls may read and consume at once, as they
// do the manager: each read and consume holds one lock.
type lockedGenDb struct {
	*ownGenDb
	mu sync.Mutex
}

func (d *lockedGenDb) GetPerTestMocksInWindow() ([]*models.Mock, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.ownGenDb.GetPerTestMocksInWindow()
}

func (d *lockedGenDb) GetSessionMocks() ([]*models.Mock, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.ownGenDb.GetSessionMocks()
}

func (d *lockedGenDb) DeleteFilteredMock(m models.Mock) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.ownGenDb.DeleteFilteredMock(m)
}

func (d *lockedGenDb) MockSpent(name string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.ownGenDb.MockSpent(name)
}

// Two calls of one request the test recorded twice, made at once through
// one parser's strict index in verify mode, many times over: each call is
// answered with a twin of its own, and verify mode never panics. (It
// panicked in about one run of a hundred, more under -race: its two reads of
// a twin's account straddled the other call's consume.) Under -race the
// row is also the verify path's race check.
func TestTwoIdenticalCallsAtOnceAreAnsweredInVerifyMode(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	verifyOff(t)
	integrations.ExactViewVerify.Store(true)
	runs := 2000
	if raceOn {
		runs = 500
	}
	for run := 0; run < runs; run++ {
		h, db := newHTTP(), &lockedGenDb{ownGenDb: newOwnGenDb(strictTwins(), nil)}
		strictMatchOf(t, h, strictCall(0), db, db, nil)
		strictMatchOf(t, h, strictCall(1), db, db, nil)
		start := make(chan struct{})
		var wg sync.WaitGroup
		served, failed := make([]*models.Mock, 2), make([]any, 2)
		for i := range served {
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer func() {
					if p := recover(); p != nil {
						failed[i] = p
					}
				}()
				<-start
				ok, stub, _, err := h.matchOf(context.Background(), strictCall(77), db, db, nil, nil, nil, true, false, false, true, true)
				if err != nil {
					failed[i] = err
				} else if ok {
					served[i] = stub
				}
			}()
		}
		close(start)
		wg.Wait()
		for i := range served {
			if failed[i] != nil {
				t.Fatalf("run %d: call %d failed: %v", run, i, failed[i])
			}
		}
		if served[0] == nil || served[1] == nil || served[0].Name == served[1].Name {
			t.Fatalf("run %d: served %v and %v; want a twin each", run, served[0], served[1])
		}
	}
}
