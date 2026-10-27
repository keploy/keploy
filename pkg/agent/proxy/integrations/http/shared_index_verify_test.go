package http

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// tierDb is a store offering the shared part of its reads
// (integrations.SharedReads) as the manager does in a test, with a per-test
// tier that holds shared recordings as well as the test's own: the per-test
// read is the open test's unanswered recordings (own, which lead it) then the
// tier's shared part (as the reached worker scope is), the session read the
// open test's unanswered recordings then the session's shared part.
type tierDb struct {
	*genVarDb
	own []*models.Mock
}

// newTierDb is a tierDb whose open test owns own, with the shared part
// sharedPerTest of its per-test tier and session of its session tier.
func newTierDb(own, sharedPerTest, session []*models.Mock) *tierDb {
	return &tierDb{genVarDb: &genVarDb{varDb: &varDb{
		perTest: append(slices.Clone(own), sharedPerTest...),
		session: session,
	}, gen: 1}, own: own}
}

func (d *tierDb) OwnedByOpenTest(m *models.Mock) bool { return slices.Contains(d.own, m) }

func (d *tierDb) SharedGeneration(models.Kind) (integrations.SharedGen, bool) {
	return integrations.SharedGen{View: 1, Store: d.gen}, true
}

func (d *tierDb) SharedMocks() (perTest, session []*models.Mock, ok bool) {
	for _, m := range d.perTest {
		if !slices.Contains(d.own, m) {
			perTest = append(perTest, m)
		}
	}
	return perTest, slices.Clone(d.session), true
}

func (d *tierDb) OwnedUnanswered() ([]*models.Mock, bool) {
	var out []*models.Mock
	for _, m := range d.perTest {
		if slices.Contains(d.own, m) {
			out = append(out, m)
		}
	}
	return out, true
}

func (d *tierDb) GetSessionMocks() ([]*models.Mock, error) {
	owned, _ := d.OwnedUnanswered()
	return append(owned, d.session...), nil
}

// poolGets are n reusable recordings of GET http://store/things/<i>, the
// shared part's.
func poolGets(n int) []*models.Mock {
	out := make([]*models.Mock, n)
	for i := range out {
		out[i] = reusable(at(varMock("pool-"+strconv.Itoa(i), "GET", "http://store/things/"+strconv.Itoa(i), "", 200, `{"id":`+strconv.Itoa(i)+`}`, nil), 10+i))
	}
	return out
}

// verifyOn turns the exact view's verify mode on for t.
func verifyOn(t *testing.T) {
	t.Helper()
	was := integrations.ExactViewVerify.Load()
	integrations.ExactViewVerify.Store(true)
	t.Cleanup(func() { integrations.ExactViewVerify.Store(was) })
}

// verifyPanicOf is what call panicked with in verify mode, or "" where it
// did not panic.
func verifyPanicOf(call func()) (got string) {
	defer func() {
		if r := recover(); r != nil {
			got = fmt.Sprint(r)
		}
	}()
	call()
	return ""
}

// In the exact view's verify mode a strict call through the index of the
// shared part of a test's reads is checked against a read, as one through
// the index of a staging is: an index that lost a shared recording the call
// is panics.
func TestASharedStrictIndexThatLostAMatchPanicsInVerifyMode(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	verifyOff(t)
	db := newTierDb(nil, nil, poolGets(10))
	h := newHTTP()
	for _, i := range []string{"0", "1"} {
		if got := strictMatchOf(t, h, liveReq("GET", "/things/"+i, ""), db, db, nil); got == nil || got.Name != "pool-"+i {
			t.Fatalf("served %v; want pool-%s", got, i)
		}
	}
	if !h.strictIx.shared.has {
		t.Fatal("no index of the shared part was built")
	}
	lost := 0
	for k := range h.strictIx.shared.last.byPath {
		if strings.HasSuffix(k, "things/5") {
			delete(h.strictIx.shared.last.byPath, k)
			lost++
		}
	}
	if lost == 0 {
		t.Fatal("the shared index files no bucket of /things/5")
	}
	verifyOn(t)
	if got := verifyPanicOf(func() { strictMatchOf(t, h, liveReq("GET", "/things/5", ""), db, db, nil) }); !strings.Contains(got, "indexed strict fits") {
		t.Fatalf("panicked with %q; want the verify mode's panic of the indexed strict fits", got)
	}
}

// Likewise the learn step's: a call through the index of the shared part
// that lost the recording the call is exactly panics in verify mode.
func TestASharedLearnIndexThatLostAMatchPanicsInVerifyMode(t *testing.T) {
	h, n, _ := learnOn(t)
	verifyOff(t)
	db := newTierDb(nil, nil, poolGets(10))
	for _, i := range []string{"0", "1"} {
		if _, err := h.learnedMatch(context.Background(), liveReq("GET", "/things/"+i, ""), db, n, models.OutgoingOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	if !h.learnIx.shared.has {
		t.Fatal("no learn index of the shared part was built")
	}
	lost := 0
	for k := range h.learnIx.shared.last.byPath {
		if strings.HasSuffix(k, "things/5") {
			delete(h.learnIx.shared.last.byPath, k)
			lost++
		}
	}
	if lost == 0 {
		t.Fatal("the shared learn index files no bucket of /things/5")
	}
	verifyOn(t)
	if got := verifyPanicOf(func() {
		_, _ = h.learnedMatch(context.Background(), liveReq("GET", "/things/5", ""), db, n, models.OutgoingOptions{})
	}); !strings.Contains(got, "indexed learned group") {
		t.Fatalf("panicked with %q; want the verify mode's panic of the indexed learned group", got)
	}
}

// A shared recording in the per-test tier (the reached worker scope's) is
// read through the index of the shared part in that tier, beside the test's
// own recording of its path: the strict fits and the learned group of each
// call are a read's, in a read's order (the verify mode compares them on
// every call), and the call is answered the test's own first, then the
// worker scope, then the session's. Here the worker scope's is recorded
// before the test's own, and the session's after both.
func TestTheSharedIndexReadsAPerTestTiersSharedRecordingInItsTier(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	verifyOn(t)
	newDb := func() *tierDb {
		return newTierDb(
			[]*models.Mock{at(varMock("own", "GET", "http://store/things/x", "", 200, `{"by":"own"}`, nil), 2)},
			[]*models.Mock{at(varMock("worker", "GET", "http://store/things/x", "", 200, `{"by":"worker"}`, nil), 1)},
			append(poolGets(4), reusable(at(varMock("session", "GET", "http://store/things/x", "", 200, `{"by":"session"}`, nil), 3))),
		)
	}
	db, h := newDb(), newHTTP()
	if shared, _, _ := db.SharedMocks(); len(shared) != 1 {
		t.Fatalf("the per-test tier's shared part holds %d recordings; want the worker scope's", len(shared))
	}
	var served []string
	for c := 0; c < 4; c++ {
		got := verifyPanicOf(func() {
			if s := strictMatchOf(t, h, liveReq("GET", "/things/x", ""), db, db, nil); s != nil {
				served = append(served, s.Name)
			} else {
				served = append(served, "<miss>")
			}
		})
		if got != "" {
			t.Fatalf("strict call %d: %s", c, got)
		}
	}
	if !h.strictIx.shared.has {
		t.Fatal("no index of the shared part was built")
	}
	if want := []string{"own", "worker", "session", "session"}; !slices.Equal(served, want) {
		t.Fatalf("served %v; want %v", served, want)
	}
	hl, n, _ := learnOn(t)
	verifyOn(t)
	ldb := newDb()
	for c := 0; c < 2; c++ {
		if got := verifyPanicOf(func() {
			if _, err := hl.learnedMatch(context.Background(), liveReq("GET", "/things/x", ""), ldb, n, models.OutgoingOptions{}); err != nil {
				t.Fatal(err)
			}
		}); got != "" {
			t.Fatalf("learned call %d: %s", c, got)
		}
	}
	if !hl.learnIx.shared.has {
		t.Fatal("no learn index of the shared part was built")
	}
}
