package proxy

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	httpint "go.keploy.io/server/v3/pkg/agent/proxy/integrations/http"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// asideNames is the names of the mocks in mks that are set-aside recordings,
// each with its test.
func asideNames(mks []*models.Mock) []string {
	var out []string
	for _, mk := range mks {
		if mk.TestModeInfo.SetAsideFrom != "" {
			out = append(out, mk.Name+"<"+mk.TestModeInfo.SetAsideFrom)
		}
	}
	return out
}

// A set-aside test's recordings are offered to every test view installed
// after it, in no read but RangeSetAside, by kind, marked with their test;
// none is required of the test, carried into it, or counted as its own
// consume; one that answered is named for the test it answered in, and is
// offered to no later test; the round's snapshot holds it as no test's own
// (never learned against).
func TestASetAsideRecordingIsServedAfterItsTestUnchecked(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	own := exactMock("own", 0, models.LifetimeSession)
	aside := exactMock("aside", time.Second, models.LifetimeSession)
	mm := stagedManager(t, nil, []*models.Mock{own, aside})
	mm.ServeExactAside("Q", []*models.Mock{aside})
	mm.ServeExactTest("T", []*models.Mock{own})
	pt, _ := mm.GetPerTestMocksInWindow()
	equalNames(t, "per-test: a set-aside recording is in no read", pt, "own")
	sa := setAsideOf(mm, models.HTTP)
	if got := asideNames(sa); !slices.Equal(got, []string{"aside<Q"}) {
		t.Fatalf("set-aside copies %v", got)
	}
	if len(setAsideOf(mm, models.Mongo)) != 0 {
		t.Fatal("SetAsideMocks gave a recording of another kind")
	}
	snap, _ := mm.RoundSnapshot(integrations.RoundQuery{})
	for _, mk := range sa {
		if mk.Name == "aside" {
			if snap.Owner(mk) != integrations.OwnerSetAside || snap.OwnedByOpenTest(mk) {
				t.Fatalf("the round holds a set-aside recording as %v", snap.Owner(mk))
			}
			if !mm.MarkMockAsUsed(*mk) {
				t.Fatal("the set-aside recording did not answer the call that is it")
			}
		}
	}
	test, consumed, unconsumed, ok := mm.ExactTestOutcome()
	if !ok || test != "T" || len(consumed) != 0 || !slices.Equal(unconsumed, []string{"own"}) {
		t.Fatalf("outcome %q consumed %v unconsumed %v: only the test's own count", test, consumed, unconsumed)
	}
	if st, served, ok := mm.ExactTestServedAside(); !ok || st != "T" || !slices.Equal(served, []string{"aside served from Q (set aside)"}) {
		t.Fatalf("served aside %q %v %v", st, served, ok)
	}
	if len(setAsideOf(mm, "")) != 0 {
		t.Fatal("a set-aside recording that answered is still offered")
	}
	// Answered once: the next test is not offered it.
	mm.ServeExactTest("U", nil)
	if got := setAsideOf(mm, ""); len(got) != 0 {
		t.Fatalf("a set-aside recording another test answered with is offered again: %v", asideNames(got))
	}
	if _, served, _ := mm.ExactTestServedAside(); len(served) != 0 {
		t.Fatalf("a test that was served nothing aside names %v", served)
	}
}

// A kept test's recordings handed aside before it began (keploy mock replay:
// a test recorded after it began first) answer once in the staging, the
// test's own copy included: served aside to a later test, the owner's own
// read no longer holds it and its consume is refused; consumed by the owner
// first, no later test is offered it. One the replay never handed takes no
// claim, as before.
func TestAHandedAsideRecordingAnswersOnceWithItsOwnTestsCopy(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	pat := exactMock("pat", 0, models.LifetimeSession)
	// Served aside first: the owner is refused its own.
	mm := stagedManager(t, nil, []*models.Mock{pat})
	mm.ServeExactAside("Q", []*models.Mock{pat})
	mm.ServeExactTest("K", nil)
	sa := setAsideOf(mm, "")
	if got := asideNames(sa); !slices.Equal(got, []string{"pat<Q"}) {
		t.Fatalf("set-aside copies %v", got)
	}
	if !mm.MarkMockAsUsed(*sa[0]) {
		t.Fatal("the handed recording did not answer the later test's call")
	}
	mm.ServeExactTest("Q", []*models.Mock{pat})
	pt, _ := mm.GetPerTestMocksInWindow()
	equalNames(t, "per-test: the owner's copy answered elsewhere", pt)
	if mm.MarkMockAsUsed(*pat) {
		t.Fatal("the owner's own copy answered again after another test was served it aside")
	}
	// Consumed by its owner first: offered to no later test.
	mm = stagedManager(t, nil, []*models.Mock{pat})
	mm.ServeExactAside("Q", []*models.Mock{pat})
	mm.ServeExactTest("K", nil)
	mm.ServeExactTest("Q", []*models.Mock{pat})
	pt, _ = mm.GetPerTestMocksInWindow()
	equalNames(t, "per-test", pt, "pat")
	if !mm.MarkMockAsUsed(*pt[0]) {
		t.Fatal("the owner's own copy did not answer")
	}
	mm.ServeExactTest("Z", nil)
	if got := setAsideOf(mm, ""); len(got) != 0 {
		t.Fatalf("a recording its owner answered is offered aside: %v", asideNames(got))
	}
	// Never handed: the owner's consume takes no claim.
	mm = stagedManager(t, nil, []*models.Mock{pat})
	mm.ServeExactTest("Q", []*models.Mock{pat})
	pt, _ = mm.GetPerTestMocksInWindow()
	if len(pt) != 1 || !mm.MarkMockAsUsed(*pt[0]) {
		t.Fatal("the test's own recording did not answer")
	}
	if who, ok := mm.exact.asideClaims.claimant(mm.stagingEpoch.Load(), "pat"); ok {
		t.Fatalf("a recording never handed aside was claimed for %q", who)
	}
}

// A handed recording answers once in the staging for a call in flight before
// its test's begin too (a pool view's ahead), whichever lands first: the
// claim is taken as the copy answers, under the owner's name. The
// interleavings are forced in order, one call after another.
func TestAHandedRecordingAnswersOnceForACallInFlightBeforeItsTestBegins(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	pat := exactMock("pat", 0, models.LifetimeSession)
	claimant := func(mm *MockManager) string {
		who, _ := mm.exact.asideClaims.claimant(mm.stagingEpoch.Load(), "pat")
		return who
	}
	// Served aside first: the owner's in-flight call is refused it.
	mm := stagedManager(t, nil, []*models.Mock{pat})
	mm.ServeExactAside("Q", []*models.Mock{pat})
	mm.ServeExactTest("K", nil)
	sa := setAsideOf(mm, "")
	if len(sa) != 1 || !mm.MarkMockAsUsed(*sa[0]) {
		t.Fatal("the handed recording did not answer the later test's call")
	}
	mm.ServeExactPool(models.PoolKeyNone, nil, pat)
	pt, _ := mm.GetPerTestMocksInWindow()
	equalNames(t, "in flight: a copy another test was served aside", pt)
	if mm.MarkMockAsUsed(*pat) {
		t.Fatal("a call in flight before the owner's begin was answered a recording another test was served aside")
	}
	if who := claimant(mm); who != "K" {
		t.Fatalf("claimed for %q, want K", who)
	}
	// In flight first: the claim is the owner's, kept as it opens, so no
	// later test is offered it, nor answered with it.
	mm = stagedManager(t, nil, []*models.Mock{pat})
	mm.ServeExactAside("Q", []*models.Mock{pat})
	mm.ServeExactTest("K", nil)
	mm.ServeExactPool(models.PoolKeyNone, nil, pat)
	pt, _ = mm.GetPerTestMocksInWindow()
	equalNames(t, "in flight", pt, "pat")
	if !mm.MarkMockAsUsed(*pt[0]) {
		t.Fatal("the owner's call in flight was not answered its own recording")
	}
	if who := claimant(mm); who != "Q" {
		t.Fatalf("an in-flight answer claimed for %q, want Q", who)
	}
	mm.ServeExactTest("Q", []*models.Mock{pat})
	if who := claimant(mm); who != "Q" {
		t.Fatalf("the owner's begin let go of its in-flight answer's claim (now %q)", who)
	}
	mm.ServeExactTest("Z", nil)
	if got := setAsideOf(mm, ""); len(got) != 0 {
		t.Fatalf("a recording answered in flight is offered aside: %v", asideNames(got))
	}
	if mm.MarkMockAsUsed(*pat) {
		t.Fatal("a recording answered in flight answered a later test aside")
	}
	// Its owner opened again (a retry): its own call in flight before the
	// second begin is its own to answer, and carried in.
	mm = stagedManager(t, nil, []*models.Mock{pat})
	mm.ServeExactAside("Q", []*models.Mock{pat})
	mm.ServeExactTest("K", nil)
	mm.ServeExactTest("Q", []*models.Mock{pat})
	pt, _ = mm.GetPerTestMocksInWindow()
	if len(pt) != 1 || !mm.MarkMockAsUsed(*pt[0]) {
		t.Fatal("the owner's own copy did not answer")
	}
	mm.ServeExactPool(models.PoolKeyNone, nil, pat)
	pt, _ = mm.GetPerTestMocksInWindow()
	equalNames(t, "in flight into the retry", pt, "pat")
	if !mm.MarkMockAsUsed(*pt[0]) {
		t.Fatal("the retry's call in flight was refused the owner's own recording")
	}
	mm.ServeExactTest("Q", []*models.Mock{pat})
	if who := claimant(mm); who != "Q" {
		t.Fatalf("the retry's carried answer is claimed for %q, want Q", who)
	}
	// Never handed: an in-flight answer takes no claim, as before.
	mm = stagedManager(t, nil, []*models.Mock{pat})
	mm.ServeExactPool(models.PoolKeyNone, nil, pat)
	pt, _ = mm.GetPerTestMocksInWindow()
	if len(pt) != 1 || !mm.MarkMockAsUsed(*pt[0]) {
		t.Fatal("a call in flight was not answered")
	}
	if who, ok := mm.exact.asideClaims.claimant(mm.stagingEpoch.Load(), "pat"); ok {
		t.Fatalf("a recording never handed aside was claimed for %q", who)
	}
}

// What is not served aside: a recording that places or binds variables (its
// values are its own test's), one the open test owns (its own, required), and
// anything after a session reset.
func TestWhatASetAsideTestDoesNotServe(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	vars := exactMock("vars", 0, models.LifetimeSession)
	vars.Spec.Variables = models.MockVariables{"v": models.MockVariable{Recorded: "kiwi", Bind: "body.item"}}
	shared := exactMock("shared", time.Second, models.LifetimeSession)
	mm := stagedManager(t, nil, []*models.Mock{vars, shared})
	mm.ServeExactAside("Q", []*models.Mock{vars, shared})
	mm.ServeExactTest("T", []*models.Mock{shared})
	pt, _ := mm.GetPerTestMocksInWindow()
	equalNames(t, "per-test", pt, "shared")
	if got := asideNames(append(pt, setAsideOf(mm, "")...)); len(got) != 0 {
		t.Fatalf("set-aside copies %v", got)
	}
	if _, _, unconsumed, _ := mm.ExactTestOutcome(); !slices.Equal(unconsumed, []string{"shared"}) {
		t.Fatalf("the test's own recording is required whoever else recorded it: %v", unconsumed)
	}
	mm.ResetForReplaySession()
	if got := mm.exact.aside.snapshot(); len(got) != 0 {
		t.Fatalf("a session reset kept %d set-aside recordings", len(got))
	}
}

// What a test handed aside on disk (ServeExactAsideOnDisk) does not serve,
// as with its resident recordings: one that places or binds variables, a
// protocol-phase one, and one whose destination keploy now reaches for real
// (keep). And a test handed both resident and on disk offers its recordings
// in recorded order, wherever each is kept: of two exact copies of one call
// recorded with different answers, the earlier answers first.
func TestWhatATestHandedAsideOnDiskDoesNotServe(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	disk, err := newDiskMocks(zap.NewNop(), 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = disk.Close() })
	item := func(name string, at time.Duration, body string) *models.Mock {
		mk := &models.Mock{Version: models.GetVersion(), Name: name, Kind: models.HTTP,
			Spec: models.MockSpec{
				HTTPReq:          &models.HTTPReq{Method: "GET", URL: "http://api/item"},
				HTTPResp:         &models.HTTPResp{StatusCode: 200, Body: body},
				ReqTimestampMock: exactT0.Add(at), ResTimestampMock: exactT0.Add(at + time.Millisecond),
			}}
		mk.TestModeInfo.Lifetime = models.LifetimePerTest
		return mk
	}
	vars := item("vars", 1*time.Second, "v")
	vars.Spec.Variables = models.MockVariables{"v": models.MockVariable{Recorded: "kiwi", Bind: "body.item"}}
	phase := item("phase", 2*time.Second, "p")
	phase.Spec.Metadata = map[string]string{"type": "config"}
	if !phase.IsProtocolPhase() {
		t.Fatal("the fixture's phase recording is not protocol-phase")
	}
	toReal := item("real", 3*time.Second, "r")
	early := item("early", 4*time.Second, "first")
	late := item("late", 5*time.Second, "second")
	for _, mk := range []*models.Mock{vars, phase, toReal, early} {
		if err := disk.Add(mk); err != nil {
			t.Fatal(err)
		}
	}
	disk.Finalize()
	own := exactMock("own", time.Hour, models.LifetimeSession)
	mm := stagedManager(t, nil, []*models.Mock{own})
	mm.SetRecordingSource(disk.RangeKeys)
	// As the agent hands a test: its resident recordings, then those on disk.
	mm.ServeExactAside("Q", []*models.Mock{late})
	mm.ServeExactAsideOnDisk("Q", []string{"vars", "phase", "real", "early"}, func(mk *models.Mock) bool { return mk.Name != "real" })
	mm.ServeExactTest("T", []*models.Mock{own})
	var got []string
	mm.RangeSetAside(models.HTTP, httpint.RequestKeysOf("GET", "/item", "", true), func(b []*models.Mock) bool {
		got = append(got, asideNames(b)...)
		return true
	})
	if !slices.Equal(got, []string{"early<Q", "late<Q"}) {
		t.Fatalf("offered %v; want early<Q then late<Q: no variable, protocol-phase or now-real recording, and in recorded order", got)
	}
}

// One set-aside copy answers one call of a test, even when two of the
// test's calls were offered it before either was answered (two connections
// of the app making the same call at once): the claim is the test's either
// way, so what the view was served aside (exactView.asideServed) refuses the
// second, and the set's calls of that shape never exceed what it recorded.
func TestASetAsideCopyOfferedToTwoCallsOfATestAnswersOne(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	aside := exactMock("aside", 0, models.LifetimeSession)
	mm := stagedManager(t, nil, []*models.Mock{aside})
	mm.ServeExactAside("Q", []*models.Mock{aside})
	mm.ServeExactTest("T", nil)
	first, second := setAsideOf(mm, models.HTTP), setAsideOf(mm, models.HTTP)
	if a, b := asideNames(first), asideNames(second); !slices.Equal(a, []string{"aside<Q"}) || !slices.Equal(b, a) {
		t.Fatalf("offered %v, then %v: want the copy offered to both calls", a, b)
	}
	if !mm.MarkMockAsUsed(*first[0]) {
		t.Fatal("the first call was not answered by the set-aside copy")
	}
	if mm.MarkMockAsUsed(*second[0]) {
		t.Fatal("one set-aside copy answered a second call of the same test")
	}
	if _, served, _ := mm.ExactTestServedAside(); !slices.Equal(served, []string{"aside served from Q (set aside)"}) {
		t.Fatalf("served aside %v; want the copy once", served)
	}
}

// A set-aside recording whose request holds a read of the application's
// clock the learn step learned (MockSpec.Clocks) is offered to the tests
// after it, unlike one that places or binds variables: a clock read's value
// comes from the clock at the call, not from its own test's other calls, so
// the parser checks it against the later call's own window, as it does in
// its own test (the HTTP parser's row:
// TestASetAsideRecordingsClockReadIsCheckedInTheLaterCallsWindow). Not
// offering it would leave the later test's call unanswered (a false red);
// serving it unchecked would be a false green.
func TestASetAsideRecordingWithALearnedClockReadIsOffered(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	clock := exactMock("clock", 0, models.LifetimeSession)
	clock.Spec.Clocks = map[string]models.ClockOffset{"body.since": {Res: time.Second}}
	mm := stagedManager(t, nil, []*models.Mock{clock})
	mm.ServeExactAside("Q", []*models.Mock{clock})
	mm.ServeExactTest("T", nil)
	sa := setAsideOf(mm, models.HTTP)
	if got := asideNames(sa); !slices.Equal(got, []string{"clock<Q"}) {
		t.Fatalf("set-aside copies %v; want the recording with a learned clock read offered", got)
	}
	if len(sa[0].Spec.Clocks) != 1 {
		t.Fatalf("the offered copy lost its learned clock read: %v", sa[0].Spec.Clocks)
	}
}

// setAsideOf is every set-aside recording mm offers the open test of kind
// (every kind for ""), across its handed tests (RangeSetAside).
func setAsideOf(mm *MockManager, kind models.Kind) []*models.Mock {
	var out []*models.Mock
	mm.RangeSetAside(kind, nil, func(b []*models.Mock) bool {
		out = append(out, b...)
		return true
	})
	return out
}

// A partial replay hands, at its first begin, every earlier test it has not
// run (founder rule no-oom-components: "a row pins peak heap"). Handing aside
// 1000 tests' 2 recordings kept on disk, each with a 64 KiB request (128 MiB
// decoded), holds their names only; a call no recording answers reads them
// one handed test at a time, in hand order, under its request keys, and the
// heap held at any point of that read, and after it, stays under 16 MiB.
// Mutants: decoding at the hand, or every test's before the first visit,
// holds the 128 MiB.
func TestAHandedAsideOnDiskIsReadOneTestAtATimeWithinABoundHeap(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	const tests, per, body, bound = 1000, 2, 64 << 10, 16 << 20
	disk, err := newDiskMocks(zap.NewNop(), 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = disk.Close() })
	pad := strings.Repeat("x", body)
	type handed struct {
		from  string
		names []string
	}
	var hands []handed
	for i := 0; i < tests; i++ {
		h := handed{from: fmt.Sprintf("T%04d", i)}
		for j := 0; j < per; j++ {
			at := exactT0.Add(time.Duration(i*per+j) * time.Millisecond)
			mk := &models.Mock{Version: models.GetVersion(), Name: fmt.Sprintf("big-%d-%d", i, j), Kind: models.HTTP,
				Spec: models.MockSpec{
					HTTPReq:          &models.HTTPReq{Method: "GET", URL: fmt.Sprintf("http://api/big/%d", i), Header: map[string]string{"X-Pad": pad}},
					HTTPResp:         &models.HTTPResp{StatusCode: 200, Body: "ok"},
					ReqTimestampMock: at, ResTimestampMock: at.Add(time.Microsecond),
				}}
			mk.TestModeInfo.Lifetime = models.LifetimePerTest
			if err := disk.Add(mk); err != nil {
				t.Fatal(err)
			}
			h.names = append(h.names, mk.Name)
		}
		hands = append(hands, h)
	}
	disk.Finalize()
	own := exactMock("own", time.Hour, models.LifetimeSession)
	mm := stagedManager(t, nil, []*models.Mock{own})
	mm.SetRecordingSource(disk.RangeKeys)
	base := liveHeap()
	for _, h := range hands {
		mm.ServeExactAsideOnDisk(h.from, h.names, nil)
	}
	mm.ServeExactTest("LAST", []*models.Mock{own})
	if held := int64(liveHeap()) - int64(base); held > bound {
		t.Fatalf("handing %d tests aside holds %d bytes, over %d: the hand must hold names only", tests, held, bound)
	}
	keys := httpint.RequestKeysOf("GET", "/big/none", "", true)
	var peak int64
	var from []string
	mm.RangeSetAside(models.HTTP, keys, func(b []*models.Mock) bool {
		if len(b) != per {
			t.Fatalf("a batch of %d, want one handed test's %d", len(b), per)
		}
		from = append(from, b[0].TestModeInfo.SetAsideFrom)
		if len(from)%100 == 0 {
			if h := int64(liveHeap()) - int64(base); h > peak {
				peak = h
			}
		}
		return true
	})
	if len(from) != tests || from[0] != "T0000" || from[tests-1] != hands[tests-1].from {
		t.Fatalf("visited %d tests (%v...), want all %d in hand order", len(from), from[:min(3, len(from))], tests)
	}
	if peak > bound {
		t.Fatalf("reading the handed tests held %d bytes at its peak, over %d: one test at a time", peak, bound)
	}
	if held := int64(liveHeap()) - int64(base); held > bound {
		t.Fatalf("after the read %d bytes are held, over %d: none is kept", held, bound)
	}
	// Another shape's keys read none of them.
	n := 0
	mm.RangeSetAside(models.HTTP, httpint.RequestKeysOf("POST", "/big/1", "", true), func([]*models.Mock) bool { n++; return true })
	if n != 0 {
		t.Fatalf("a call of another shape was offered %d handed tests' recordings", n)
	}
}

// The set-aside recordings a call is offered come in recorded order, and a
// recording with no request time (an older keploy's) comes first, as the
// agent orders a test's own: the first copy the matcher reads is the one a
// test recorded first. Mutant: those with none sorted last.
func TestInRecordedOrderPutsRecordingsWithNoRequestTimeFirst(t *testing.T) {
	at := time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)
	mk := func(name string, req time.Time) *models.Mock {
		return &models.Mock{Name: name, Spec: models.MockSpec{ReqTimestampMock: req}}
	}
	mocks := []*models.Mock{mk("late", at.Add(time.Second)), mk("none-1", time.Time{}), mk("early", at), mk("none-2", time.Time{})}
	inRecordedOrder(mocks)
	var got []string
	for _, m := range mocks {
		got = append(got, m.Name)
	}
	if want := []string{"none-1", "none-2", "early", "late"}; !slices.Equal(got, want) {
		t.Fatalf("recorded order: got %v, want %v", got, want)
	}
}
