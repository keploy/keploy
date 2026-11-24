package proxy

import (
	"reflect"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// Under the proven switch a test's window has a recording made before the
// first test as its background only when a declared rule (a startup: true
// rule, placed in worker:) or a learned one (worker:, recorded between
// tests) covers it. A plain startup: recording (BackgroundRecording.Startup,
// "pool" here) is the test's own call in a test's window: the window does
// not show it, a call marked in a test reads it as no background recording
// (so its miss counts), and a test's identical call after it answered
// outside every test is told it was made outside every test, not one more
// run of the set's background. Between tests, and with the switch off, it is
// the background's as before. The worker: recording ("w-boot") stays the
// background's in a test's window.
// Mutation: show a plain startup recording in a test's window, read it as
// background for a call in a test, keep its pool scope on a test's repeat,
// or apply any of it between tests or with the switch off.
func TestAPlainStartupRecordingIsATestsOwnCallInItsWindow(t *testing.T) {
	byURL(t)
	mm, ms := s12Set(t)
	mm.SeedBackgroundRecordings([]models.BackgroundRecording{
		{Mock: ms["pool"], Startup: true},
		{Mock: ms["w-boot"]},
	})
	mm.SetMocksWithWindow(nil, []*models.Mock{ms["own"], ms["other"], ms["w-boot"], ms["w-later"], ms["pool"]}, models.BaseTime, time.Now())
	mm.ServeExactBackground(models.RecordedWithS12Routing, []string{"pool"}, []string{"w-boot", "w-later"})
	mm.ServeExactWorker([]string{"w-boot"})
	inTest := func() integrations.CallRound {
		return integrations.NewCallRound(mm.StagingEpoch(), nil).InView(true, true)
	}

	// In a test, unspent, it is not shown, and a call there reads it as no
	// background recording.
	mm.ServeExactTest("T0", []*models.Mock{ms["other"]})
	if got := sessionNames(mm); got != "other,w-boot" {
		t.Fatalf("in a test the window shows %s; want other,w-boot: the plain startup recording is no background of a test's window", got)
	}
	equalNames(t, "a test window's background", mm.Background(models.HTTP), "w-boot")
	if got := pollFirst(t, mm, "pool"); got != "a miss" {
		t.Fatalf("a test was served the plain startup recording: %s", got)
	}
	if got := bgNames(mm.BackgroundRecordingsIn(inTest(), models.HTTP)); !reflect.DeepEqual(got, []string{"w-boot"}) {
		t.Fatalf("a call in a test reads %v as background recordings; want [w-boot]", got)
	}

	// Between tests the pool answers, and its spend is the background's.
	mm.ServeExactPool(models.PoolKeyNone, []string{"pool"})
	between := integrations.NewCallRound(mm.StagingEpoch(), nil)
	if got := bgNames(mm.BackgroundRecordingsIn(between, models.HTTP)); !reflect.DeepEqual(got, []string{"w-boot", "pool"}) {
		t.Fatalf("a call between tests reads %v; want [w-boot pool]", got)
	}
	if got := pollFirst(t, mm, "pool"); got != "pool" {
		t.Fatalf("between tests the plain startup recording did not answer: %s", got)
	}
	if a, ok := mm.FindAnsweredEarlier(models.HTTP, []string{"url:/pool"}, func(*models.Mock) bool { return true }); !ok || a.Background != models.RepeatBackgroundPool {
		t.Fatalf("between tests its spend reads %+v (%v); want the pool's", a, ok)
	}

	// A test's repeat of it after it answered is the test's own call.
	mm.ServeExactTest("T1", []*models.Mock{ms["own"]})
	if a, ok := mm.FindAnsweredEarlier(models.HTTP, []string{"url:/pool"}, func(*models.Mock) bool { return true }); !ok || a.Background != "" || a.Mock.Name != "pool" {
		t.Fatalf("a test's repeat of the plain startup recording reads %+v (%v); want it with no background scope", a, ok)
	}

	// With the switch off nothing of this applies.
	t.Setenv(provenswitch.Env, "")
	if got := bgNames(mm.BackgroundRecordingsIn(inTest(), models.HTTP)); !reflect.DeepEqual(got, []string{"w-boot", "pool"}) {
		t.Fatalf("with the switch off a call in a test reads %v; want [w-boot pool]", got)
	}
}

// A background recording seeded without the plain startup mark (keploy
// test's staging, a learned worker: one, a declared one) is the background's
// in a test's window too, as before.
func TestABackgroundRecordingWithoutTheStartupMarkStaysTheBackgroundsInATest(t *testing.T) {
	mm, ms := s12Set(t)
	mm.SeedBackgroundRecordings([]models.BackgroundRecording{{Mock: ms["pool"]}, {Mock: ms["w-boot"]}})
	mm.SetMocksWithWindow(nil, []*models.Mock{ms["own"], ms["other"], ms["w-boot"], ms["w-later"], ms["pool"]}, models.BaseTime, time.Now())
	mm.ServeExactBackground(models.RecordedWithS12Routing, []string{"pool"}, []string{"w-boot", "w-later"})
	mm.ServeExactWorker([]string{"w-boot"})
	mm.ServeExactTest("T1", []*models.Mock{ms["own"]})
	if got := sessionNames(mm); got != "own,w-boot,pool" {
		t.Fatalf("in a test the window shows %s; want own,w-boot,pool", got)
	}
	inTest := integrations.NewCallRound(mm.StagingEpoch(), nil).InView(true, true)
	if got := bgNames(mm.BackgroundRecordingsIn(inTest, models.HTTP)); !reflect.DeepEqual(got, []string{"w-boot", "pool"}) {
		t.Fatalf("a call in a test reads %v; want [w-boot pool]", got)
	}
}

// A by-kind answer (Mongo's background replies, every one of a kind equal)
// reads and spends a test's window's background as the consume doors do: to
// a call read in a test's window a plain startup: recording is none of it
// (KindBackground, SpentBackground, the round snapshot's BackgroundUnused
// and BackgroundSpent, SpendByKind), unspent or spent between tests; to one
// read between tests it is the pool's, though the next test opened before
// the call settled; so too for its background read (Background). The
// worker: recording stays the background's everywhere.
// Mutation: read or spend the plain startup recording by kind for a call in
// a test's window (manager, snapshot, held store), or decide a held call's
// by-kind or background read by the view in force, not the window it was
// read in.
func TestAByKindAnswerInATestsWindowLeavesOutThePlainStartupRecordings(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	mongo := func(name string, at time.Duration) *models.Mock {
		m := exactMock(name, at, models.LifetimeSession)
		m.Kind = models.Mongo
		return m
	}
	ms := map[string]*models.Mock{
		"own": mongo("own", 0), "boot": mongo("boot", time.Second), "boot2": mongo("boot2", 2*time.Second),
		"w-boot": mongo("w-boot", 3*time.Second), "own2": mongo("own2", 4*time.Second),
	}
	all := []*models.Mock{ms["own"], ms["boot"], ms["boot2"], ms["w-boot"], ms["own2"]}
	mm := stagedManager(t, nil, all)
	mm.SeedBackgroundRecordings([]models.BackgroundRecording{
		{Mock: ms["boot"], Startup: true}, {Mock: ms["boot2"], Startup: true}, {Mock: ms["w-boot"]},
	})
	mm.SetMocksWithWindow(nil, all, models.BaseTime, time.Now())
	mm.ServeExactBackground(models.RecordedWithS12Routing, []string{"boot", "boot2"}, []string{"w-boot"})
	mm.ServeExactWorker([]string{"w-boot"})
	snapshot := func() (unused, spent []*models.Mock) {
		t.Helper()
		s, ok := integrations.SnapshotRound(mm, integrations.RoundQuery{Kind: models.Mongo})
		if !ok {
			t.Fatal("no round snapshot")
		}
		return s.BackgroundUnused, s.BackgroundSpent
	}

	// Between tests the plain startup recordings are the pool's, by kind.
	mm.ServeExactPool(models.PoolKeyNone, []string{"boot", "boot2"})
	unused, spent := mm.KindBackground(models.Mongo)
	equalNames(t, "between tests the kind's unused background", unused, "boot", "boot2", "w-boot")
	equalNames(t, "between tests the kind's spent background", spent)
	unused, _ = snapshot()
	equalNames(t, "between tests the snapshot's unused background", unused, "boot", "boot2", "w-boot")
	if !mm.SpendByKind(ms["boot"]) {
		t.Fatal("between tests a by-kind answer could not spend the plain startup recording")
	}
	equalNames(t, "between tests the spent background", mm.SpentBackground(models.Mongo), "boot")

	// In a test's window none of them is, spent or not.
	mm.ServeExactTest("T1", []*models.Mock{ms["own"]})
	unused, spent = mm.KindBackground(models.Mongo)
	equalNames(t, "a test window's unused background by kind", unused, "w-boot")
	equalNames(t, "a test window's spent background by kind", spent)
	unused, spent = snapshot()
	equalNames(t, "a test window's snapshot's unused background", unused, "w-boot")
	equalNames(t, "a test window's snapshot's spent background", spent)
	equalNames(t, "a test window's spent background", mm.SpentBackground(models.Mongo))
	if mm.SpendByKind(ms["boot2"]) {
		t.Fatal("a test's call spent the plain startup recording by kind")
	}
	var sv integrations.MockMemDb = &scopedMockDb{MockMemDb: mm}
	if u, _ := integrations.KindBackground(sv, models.Mongo); len(u) != 1 || u[0].Name != "w-boot" {
		t.Fatalf("a test window's scoped view reads %v by kind; want [w-boot]", exactNames(u))
	}

	// A call read between tests, settled after the next test opened, reads
	// and spends them as the pool's; one read in a test, settled after its
	// end, does not.
	mm.ServeExactPool(models.PoolKeyNone, []string{"boot", "boot2"})
	between, _ := mm.HoldToRound(mm.MarkCallRound())
	mm.ServeExactTest("T2", []*models.Mock{ms["own2"]})
	unused, spent = integrations.KindBackground(between, models.Mongo)
	equalNames(t, "a call read between tests, after the test opened, reads unused", unused, "boot2", "w-boot")
	equalNames(t, "a call read between tests, after the test opened, reads spent", spent, "boot")
	equalNames(t, "a call read between tests, after the test opened, reads spent background", integrations.SpentBackground(between, models.Mongo), "boot")
	equalNames(t, "a call read between tests, after the test opened, reads the background", integrations.Background(between, models.Mongo), "w-boot", "boot2")
	equalNames(t, "a test window's background", mm.Background(models.Mongo), "w-boot")
	inTest, _ := mm.HoldToRound(mm.MarkCallRound())
	mm.ServeExactPool(models.PoolKeyNone, []string{"boot", "boot2"})
	equalNames(t, "a call read in a test, after its end, reads the background", integrations.Background(inTest, models.Mongo), "w-boot")
	if u, s := integrations.KindBackground(inTest, models.Mongo); len(u) != 1 || u[0].Name != "w-boot" || len(s) != 0 {
		t.Fatalf("a call read in a test, after its end, reads %v unused, %v spent by kind; want [w-boot], []", exactNames(u), exactNames(s))
	}
	if integrations.SpendByKind(inTest, ms["boot2"]) {
		t.Fatal("a call read in a test spent the plain startup recording by kind after its end")
	}
	if !integrations.SpendByKind(between, ms["boot2"]) {
		t.Fatal("a call read between tests could not spend the plain startup recording by kind")
	}
}

// A consume door settles a held call by the window it was read in, not the
// view in force (roundGuard.windowed): a call read between tests that
// settles after the next test opened is answered the plain startup
// recording it read, as the pool's, never refused (a 502 the app sees for
// a call no test made); one read in a test's window that settles after its
// end is never answered it. Its repeat note follows the same window
// (findAnsweredEarlierIn).
// Mutation: decide what the view shows a held call, or its repeat's
// background scope, by the view in force.
func TestAHeldCallIsShownThePlainStartupRecordingsByTheWindowItWasReadIn(t *testing.T) {
	byURL(t)
	mm, ms := s12Set(t)
	mm.SeedBackgroundRecordings([]models.BackgroundRecording{{Mock: ms["pool"], Startup: true}, {Mock: ms["w-boot"]}})
	mm.SetMocksWithWindow(nil, []*models.Mock{ms["own"], ms["other"], ms["w-boot"], ms["w-later"], ms["pool"]}, models.BaseTime, time.Now())
	mm.ServeExactBackground(models.RecordedWithS12Routing, []string{"pool"}, []string{"w-boot", "w-later"})
	mm.ServeExactWorker([]string{"w-boot"})
	all := func(*models.Mock) bool { return true }

	// Read in a test, settled after its end: refused.
	mm.ServeExactTest("T0", []*models.Mock{ms["other"]})
	inTest, _ := mm.HoldToRound(mm.MarkCallRound())
	mm.ServeExactPool(models.PoolKeyNone, []string{"pool"})
	if inTest.DeleteUnFilteredMock(*ms["pool"]) {
		t.Fatal("a call read in a test's window was answered the plain startup recording after the test ended")
	}

	// Read between tests, settled after the next test opened: answered.
	between, _ := mm.HoldToRound(mm.MarkCallRound())
	mm.ServeExactTest("T1", []*models.Mock{ms["own"]})
	if !between.DeleteUnFilteredMock(*ms["pool"]) {
		t.Fatal("a call read between tests was refused the plain startup recording it read once the next test opened")
	}

	// Its repeat: a held call read between tests keeps the pool's scope,
	// one read in a test does not, whatever view is in force.
	mm.ServeExactPool(models.PoolKeyNone, []string{"pool"})
	heldBetween, _ := mm.HoldToRound(mm.MarkCallRound())
	mm.ServeExactTest("T2", []*models.Mock{ms["other"]})
	heldInTest, _ := mm.HoldToRound(mm.MarkCallRound())
	if a, ok := heldBetween.(integrations.AnsweredFinder).FindAnsweredEarlier(models.HTTP, []string{"url:/pool"}, all); !ok || a.Background != models.RepeatBackgroundPool {
		t.Fatalf("a call read between tests, decided in a test, reads its repeat %+v (%v); want the pool's", a, ok)
	}
	mm.ServeExactPool(models.PoolKeyNone, []string{"pool"})
	if a, ok := heldInTest.(integrations.AnsweredFinder).FindAnsweredEarlier(models.HTTP, []string{"url:/pool"}, all); !ok || a.Background != "" {
		t.Fatalf("a call read in a test, decided between tests, reads its repeat %+v (%v); want no background scope", a, ok)
	}
}

// The plain startup marks are the set's: recordings' names restart in each
// set, so a call marked in a test of one set, and a view of that set, read
// that set's marks after the next set's background is installed
// (backgroundInstall.at, its prev keeping its marks), never the next set's;
// and the next set's views read its own.
// Mutation: drop the marks from the install a newer one keeps, or read the
// latest install's marks for a view of an earlier epoch.
func TestThePlainStartupMarksAreReadInTheEpochOfTheCallAndTheView(t *testing.T) {
	mm, ms := s12Set(t)
	mm.SeedBackgroundRecordings([]models.BackgroundRecording{{Mock: ms["pool"], Startup: true}, {Mock: ms["w-boot"]}})
	mm.SetMocksWithWindow(nil, []*models.Mock{ms["own"], ms["other"], ms["w-boot"], ms["w-later"], ms["pool"]}, models.BaseTime, time.Now())
	mm.ServeExactBackground(models.RecordedWithS12Routing, []string{"pool"}, []string{"w-boot", "w-later"})
	mm.ServeExactWorker([]string{"w-boot"})
	mm.ServeExactTest("T1", []*models.Mock{ms["own"]})
	v1 := mm.exactView()
	call := integrations.NewCallRound(mm.StagingEpoch(), nil).InView(true, true)

	// The next set names the same recording, with no mark.
	mm.ResetForReplaySession()
	mm.SeedBackgroundRecordings([]models.BackgroundRecording{{Mock: ms["pool"]}, {Mock: ms["w-boot"]}})
	mm.SetMocksWithWindow(nil, []*models.Mock{ms["own"], ms["other"], ms["w-boot"], ms["w-later"], ms["pool"]}, models.BaseTime, time.Now())
	if got := bgNames(mm.BackgroundRecordingsIn(call, models.HTTP)); !reflect.DeepEqual(got, []string{"w-boot"}) {
		t.Fatalf("a call marked in the earlier set's test reads %v after the next set's install; want [w-boot], its set's marks", got)
	}
	if !v1.startupOwned("pool") {
		t.Fatal("a view of the earlier set reads the next set's marks")
	}
	mm.ServeExactBackground(models.RecordedWithS12Routing, []string{"pool"}, []string{"w-boot", "w-later"})
	mm.ServeExactWorker([]string{"w-boot"})
	mm.ServeExactTest("T1", []*models.Mock{ms["own"]})
	if mm.exactView().startupOwned("pool") {
		t.Fatal("a view of the next set reads the earlier set's marks")
	}
	if got := sessionNames(mm); got != "own,w-boot,pool" {
		t.Fatalf("the next set's test window shows %s; want own,w-boot,pool", got)
	}
}
