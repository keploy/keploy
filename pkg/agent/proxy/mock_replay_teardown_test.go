package proxy

import (
	"fmt"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// keploy mock replay sends, with the pool's names (SetPoolNames), the
// recorded window of every test of its set, as it derives them from the
// set's mappings (pkg/service/mock recordedTestWindows): from the test's
// recorded begin to the last place (models.PlaceOf) of the recordings its
// mapping lists. The pool's names still decide the set's teardown bound
// (teardownAfter): after the place of every staged data-plane recording
// outside the pool; the windows serve the learn step. Of a set of one test process keploy
// test's bound (every window's end, WindowSchedule.After) tells the same
// teardown, since under keploy mock replay every staged recording is a
// test's or the pool's (poolSlices): after the last test a per-test read
// still shows the pool's recording made after every test's own (a Go
// mongo-driver's endSessions at client.Disconnect), and it answers; the same
// call made once more misses (fail closed), and neither a pool recording
// made before the first test or in a gap between two tests nor a test's own
// recording is shown. A pool recording sent after T2's last call, before
// that call's answer was stamped, is the teardown too: record charged it to
// the pool, by its place, after T2's scope.
func TestUnderMockReplayTheRecordedWindowsKeepTheSetsTeardown(t *testing.T) {
	boot := exactMongo("boot", 200*time.Millisecond, models.LifetimePerTest)
	owned := exactMongo("owned", time.Second, models.LifetimePerTest)
	gap := exactMongo("gap", 1600*time.Millisecond, models.LifetimePerTest)
	owned2 := exactMongo("owned-2", 2*time.Second, models.LifetimePerTest)
	// T2's own, sent in its window and answered later: its window ends at
	// its place, its request, so a pool recording sent after it is the
	// teardown though sent before that answer's stamp.
	late := exactMongo("owned-2-late", 2200*time.Millisecond, models.LifetimePerTest)
	late.Spec.ResTimestampMock = exactT0.Add(2600 * time.Millisecond)
	inFlight := exactMongo("pool-before-the-answer", 2400*time.Millisecond, models.LifetimePerTest)
	end := exactMongo("end-sessions", 3*time.Second, models.LifetimePerTest)
	// A protocol-phase recording made later is no test's, and no bound's.
	hs := configMock("handshake", 3500*time.Millisecond)
	slice := []string{"boot", "gap", "pool-before-the-answer", "end-sessions"}
	// What keploy mock replay derives: T began at +0.5s and T2 at +1.7s;
	// each ends at the last place of the recordings it owns.
	windows := []models.TestWindow{
		{TestCase: "T", Start: exactT0.Add(500 * time.Millisecond), End: owned.Spec.ReqTimestampMock},
		{TestCase: "T2", Start: exactT0.Add(1700 * time.Millisecond), End: late.Spec.ReqTimestampMock},
	}
	for _, c := range []struct {
		name    string
		windows []models.TestWindow
	}{
		{"the pool's names alone (keploy mock replay before it sent windows)", nil},
		{"the recorded windows with the pool's names", windows},
	} {
		t.Run(c.name, func(t *testing.T) {
			mm := stagedManager(t, []*models.Mock{boot, owned, gap, owned2, late, inFlight, end}, []*models.Mock{hs})
			// The scope table's order: the windows, then the pool slices.
			mm.SeedRecordedWindows(c.windows)
			mm.SetPoolNames(slice)
			if got := mm.testWindows.Load().Len(); got != len(c.windows) {
				t.Fatalf("precondition: %d recorded windows installed, want %d", got, len(c.windows))
			}
			after := mm.teardownAfter("k", "")
			if after == nil || !after(end.Spec.ReqTimestampMock) || !after(inFlight.Spec.ReqTimestampMock) {
				t.Fatalf("end-sessions or pool-before-the-answer, sent after every test's recording, is not the set's teardown")
			}
			for _, mk := range []*models.Mock{boot, owned, gap, owned2, late} {
				if after(mk.Spec.ReqTimestampMock) {
					t.Errorf("FALSE GREEN? %s, sent before a test's last recording, reads as the set's teardown", mk.Name)
				}
			}
			mm.ServeExactPool("k", slice)
			perTestReads(t, mm, "before the first test")
			mm.ServeExactTest("T", []*models.Mock{owned})
			if !mm.DeleteFilteredMock(*owned) {
				t.Fatal("precondition: the test's own recording did not answer it")
			}
			mm.ServeExactPool("k", slice)
			perTestReads(t, mm, "between two tests", "pool-before-the-answer", "end-sessions")
			mm.ServeExactTest("T2", []*models.Mock{owned2, late})
			if !mm.DeleteFilteredMock(*owned2) || !mm.DeleteFilteredMock(*late) {
				t.Fatal("precondition: the second test's own recordings did not answer it")
			}
			mm.ServeExactPool("k", slice)
			perTestReads(t, mm, "after the last test", "pool-before-the-answer", "end-sessions")
			if !mm.DeleteFilteredMock(*end) {
				t.Fatal("the pool's recording of the call after the last test (endSessions) did not answer it")
			}
			perTestReads(t, mm, "after it answered", "pool-before-the-answer")
			again := *end
			if mm.DeleteFilteredMock(*end) || mm.DeleteStartupMock(*end) || mm.UpdateUnFilteredMock(end, &again) || mm.MarkMockAsUsed(*end) {
				t.Fatal("FALSE GREEN: the call after the last test, made once more, was answered by the recording that already answered it")
			}
		})
	}
}

// keploy mock replay of a suite of several test processes run one after
// another (go test ./..., one test binary per package) sends, with the pool's
// names and owners, the recorded window of every test of the set. Each
// binary's call after its own last test (a Go mongo-driver's endSessions at
// client.Disconnect) is still its teardown, though another binary's tests
// were recorded later: the recorded windows do not widen a process's
// teardown bound to every test of the set.
func TestUnderMockReplayTheRecordedWindowsKeepEachTestProcessesTeardown(t *testing.T) {
	aBoot := exactMongo("a-boot", 100*time.Millisecond, models.LifetimePerTest)
	a1 := exactMongo("a1", time.Second, models.LifetimePerTest)
	a2 := exactMongo("a2", 1500*time.Millisecond, models.LifetimePerTest)
	aEnd := exactMongo("a-end", 2*time.Second, models.LifetimePerTest)
	bBoot := exactMongo("b-boot", 2500*time.Millisecond, models.LifetimePerTest)
	b1 := exactMongo("b1", 3*time.Second, models.LifetimePerTest)
	bEnd := exactMongo("b-end", 3500*time.Millisecond, models.LifetimePerTest)
	aSlice, bSlice := []string{"a-boot", "a-end"}, []string{"b-boot", "b-end"}
	mm := stagedManager(t, []*models.Mock{aBoot, a1, a2, aEnd, bBoot, b1, bEnd}, nil)
	mm.SeedRecordedWindows([]models.TestWindow{
		{TestCase: "a::TestA1", Start: exactT0.Add(900 * time.Millisecond), End: a1.Spec.ResTimestampMock},
		{TestCase: "a::TestA2", Start: exactT0.Add(1400 * time.Millisecond), End: a2.Spec.ResTimestampMock},
		{TestCase: "b::TestB", Start: exactT0.Add(2900 * time.Millisecond), End: b1.Spec.ResTimestampMock},
	})
	mm.SetPoolOwners(map[string]string{"a1": "a", "a2": "a", "b1": "b"})
	mm.SetPoolNames(append(append([]string{}, aSlice...), bSlice...))
	if got := mm.testWindows.Load().Len(); got != 3 {
		t.Fatalf("precondition: %d recorded windows installed, want 3", got)
	}
	mm.ServeExactPool("a", aSlice)
	perTestReads(t, mm, "a's start-up")
	mm.ServeExactTest("a::TestA1", []*models.Mock{a1})
	if !mm.DeleteFilteredMock(*a1) {
		t.Fatal("precondition: a1 did not answer TestA1")
	}
	mm.ServeExactPool("a", aSlice)
	mm.ServeExactTest("a::TestA2", []*models.Mock{a2})
	if !mm.DeleteFilteredMock(*a2) {
		t.Fatal("precondition: a2 did not answer TestA2")
	}
	mm.ServeExactPool("a", aSlice)
	perTestReads(t, mm, "after a's last test", "a-end")
	if !mm.DeleteFilteredMock(*aEnd) {
		t.Fatal("a's call after its last test was not answered by its own recording of it")
	}
	mm.ServeExactPool("b", bSlice)
	mm.ServeExactTest("b::TestB", []*models.Mock{b1})
	if !mm.DeleteFilteredMock(*b1) {
		t.Fatal("precondition: b1 did not answer TestB")
	}
	mm.ServeExactPool("b", bSlice)
	perTestReads(t, mm, "after b's last test", "b-end")
	if !mm.DeleteFilteredMock(*bEnd) {
		t.Fatal("b's call after its last test was not answered by its own recording of it")
	}
}

// keploy mock replay stages, of the agent's on-disk store, what was recorded
// before the set's first test and the pool, never a test's own: the trees
// then hold no recording of a test that has not opened, and a teardown bound
// read from them alone falls at the last test's recording they hold, or is
// none (api-server's full round: the endSessions after its last test
// missed). Here the trees hold a test's early call, made before the set's
// first test, so the bound from them alone would make the gap's recording
// the teardown. The bound reads every recording on disk instead, by its
// times, decoding none: the pool's recording made after every test's own is
// the teardown; one before a test's own, and a protocol-phase recording made
// later, are not. Each set's store is read again, and a store that cannot be
// read gives no teardown (fail closed), never the trees' early bound.
func TestUnderMockReplayTheTeardownBoundReadsTheRecordingOnDisk(t *testing.T) {
	// The store notes protocol-phase recordings under the proven switch,
	// keploy mock replay's.
	t.Setenv(provenswitch.Env, "1")
	early := exactMongo("owned-early", 100*time.Millisecond, models.LifetimePerTest)
	boot := exactMongo("boot", 200*time.Millisecond, models.LifetimePerTest)
	owned := exactMongo("owned", time.Second, models.LifetimePerTest)
	gap := exactMongo("gap", 1600*time.Millisecond, models.LifetimePerTest)
	owned2 := exactMongo("owned-2", 2*time.Second, models.LifetimePerTest)
	end := exactMongo("end-sessions", 3*time.Second, models.LifetimePerTest)
	dns := exactMock("dns-late", 3500*time.Millisecond, models.LifetimePerTest)
	dns.Kind = models.DNS
	slice := []string{"boot", "gap", "end-sessions"}

	disk, err := NewDiskMocks(zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = disk.Close() })
	for _, mk := range []*models.Mock{early, boot, owned, gap, owned2, end, dns} {
		if err := disk.Add(mk); err != nil {
			t.Fatal(err)
		}
	}
	disk.Finalize()

	// What the staging reads: the startup band (the test's early call, the
	// app's start) and the pool, never a test's own made in its window.
	mm := stagedManager(t, []*models.Mock{early, boot, gap, end}, nil)
	mm.SetPoolOwners(map[string]string{"owned-early": "k", "owned": "k", "owned-2": "k"})
	mm.SetPoolNames(slice)
	if after := mm.teardownAfter("k", ""); after == nil || !after(gap.Spec.ReqTimestampMock) {
		t.Fatal("precondition: the staged trees alone did not put the bound at the test's early call")
	}
	// The set as the agent stages one it keeps on disk: its recordings'
	// times handed before the staging, which installs them.
	restage := func(times func(func(string, time.Time, bool, string) bool) error) {
		mm.SetRecordingTimes(times)
		mm.SetMocksWithWindow([]*models.Mock{early, boot, gap, end}, nil, models.BaseTime, time.Now())
	}
	restage(disk.RangeTimes)
	after := mm.teardownAfter("k", "")
	if after == nil || !after(end.Spec.ReqTimestampMock) {
		t.Fatal("end-sessions, sent after every test's recording on disk, is not the set's teardown")
	}
	for _, mk := range []*models.Mock{boot, gap} {
		if after(mk.Spec.ReqTimestampMock) {
			t.Errorf("FALSE GREEN? %s, sent before a test's recording on disk, reads as the set's teardown", mk.Name)
		}
	}
	mm.ServeExactPool("k", slice)
	mm.ServeExactTest("T", []*models.Mock{early, owned})
	if !mm.DeleteFilteredMock(*early) || !mm.DeleteFilteredMock(*owned) {
		t.Fatal("precondition: the test's own recording did not answer it")
	}
	mm.ServeExactTest("T2", []*models.Mock{owned2})
	if !mm.DeleteFilteredMock(*owned2) {
		t.Fatal("precondition: the second test's own recording did not answer it")
	}
	mm.ServeExactPool("k", slice)
	perTestReads(t, mm, "after the last test", "end-sessions")

	// Another set's store, without the second test's recording: read again,
	// not the bound cached for the first.
	other, err := NewDiskMocks(zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = other.Close() })
	for _, mk := range []*models.Mock{early, boot, owned, gap, end} {
		if err := other.Add(mk); err != nil {
			t.Fatal(err)
		}
	}
	other.Finalize()
	restage(other.RangeTimes)
	if after := mm.teardownAfter("k", ""); after == nil || !after(gap.Spec.ReqTimestampMock) {
		t.Fatal("a new set's store was not read again: its bound is the first set's")
	}

	// A store that cannot be read tells no bound: the call after the last
	// test misses rather than pairing with a recording made between tests.
	if err := other.Close(); err != nil {
		t.Fatal(err)
	}
	restage(other.RangeTimes)
	if mm.teardownAfter("k", "") != nil {
		t.Fatal("FALSE GREEN? a store that cannot be read told a teardown bound from the trees alone")
	}
}

// onDisk is an on-disk store holding ms, finalized, as the agent keeps a set
// for keploy mock replay.
func onDisk(t *testing.T, ms ...*models.Mock) *DiskMocks {
	t.Helper()
	disk, err := NewDiskMocks(zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = disk.Close() })
	for _, mk := range ms {
		if err := disk.Add(mk); err != nil {
			t.Fatal(err)
		}
	}
	disk.Finalize()
	return disk
}

// stagedManagerOnDisk is stagedManager of a set the agent keeps on disk, of
// per-test tier perTest: the agent hands the times of the set's recordings
// (times, nil for none) before it stages the set (Agent.handUnstagedSources),
// and the staging installs them with the trees it builds.
func stagedManagerOnDisk(t *testing.T, perTest []*models.Mock, times func(visit func(name string, place time.Time, protocolPhase bool, clock string) bool) error) *MockManager {
	t.Helper()
	mm := NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(mm.Close)
	if times != nil {
		mm.SetRecordingTimes(times)
	}
	mm.SetMocksWithWindow(perTest, nil, models.BaseTime, time.Now())
	return mm
}

// Under keploy mock replay each test process's teardown is bounded by its
// own tests' recordings on disk, as from the trees: the staging holds no
// test's own, so the bound reads them from the store, and a recording only
// another key's tests list is no bound of this key there either. So a's
// teardown, recorded before b's tests, is a's, and b's is b's; neither is
// the other's.
func TestUnderMockReplayEachProcessesTeardownIsBoundedByItsOwnTestsOnDisk(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	aBoot := exactMongo("a-boot", 100*time.Millisecond, models.LifetimePerTest)
	a1 := exactMongo("a1", time.Second, models.LifetimePerTest)
	aEnd := exactMongo("a-end", 2*time.Second, models.LifetimePerTest)
	bBoot := exactMongo("b-boot", 2500*time.Millisecond, models.LifetimePerTest)
	b1 := exactMongo("b1", 3*time.Second, models.LifetimePerTest)
	bEnd := exactMongo("b-end", 3500*time.Millisecond, models.LifetimePerTest)
	disk := onDisk(t, aBoot, a1, aEnd, bBoot, b1, bEnd)
	// The staging: the pool's slices, never a test's own.
	mm := stagedManagerOnDisk(t, []*models.Mock{aBoot, aEnd, bBoot, bEnd}, disk.RangeTimes)
	mm.SetPoolOwners(map[string]string{"a1": "a", "b1": "b"})
	mm.SetPoolNames([]string{"a-boot", "a-end", "b-boot", "b-end"})
	for _, tc := range []struct {
		key  string
		mk   *models.Mock
		want bool
	}{{"a", aEnd, true}, {"b", bEnd, true}, {"b", aEnd, false}, {"a", aBoot, false}} {
		after := mm.teardownAfter(tc.key, "")
		if got := after != nil && after(tc.mk.Spec.ReqTimestampMock); got != tc.want {
			t.Errorf("key %s: %s is the teardown = %v, want %v", tc.key, tc.mk.Name, got, tc.want)
		}
	}
}

// The bound a test's recording on disk sets is its place (models.PlaceOf),
// where record charged it to the test: its request, or its answer for one
// placed at its answer (a consumer's poll). A pool recording sent after the
// test's last call is the teardown though that call's answer was stamped
// after it (record charged the pool recording outside the test's scope, by
// its place); one sent before the test's last call is not, and neither is
// one sent before a poll placed at its answer was answered. Read from the
// store on disk and from the trees alike.
func TestUnderMockReplayTheTeardownIsAfterTheTestsLastPlaceOnDisk(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	t.Cleanup(models.RegisterAnswerPlaced(models.Mongo, func(m *models.Mock) bool { return m.Name == "poll" }))
	before := exactMongo("before", 500*time.Millisecond, models.LifetimePerTest)
	owned := exactMongo("owned", time.Second, models.LifetimePerTest)
	owned.Spec.ResTimestampMock = exactT0.Add(2 * time.Second)
	poll := exactMongo("poll", time.Second, models.LifetimePerTest)
	poll.Spec.ResTimestampMock = exactT0.Add(2 * time.Second)
	mid := exactMongo("mid", 1500*time.Millisecond, models.LifetimePerTest)
	end := exactMongo("end", 3*time.Second, models.LifetimePerTest)
	for _, c := range []struct {
		name    string
		last    *models.Mock
		midIsTD bool
	}{
		{"a call placed at its request", owned, true},
		{"a poll placed at its answer", poll, false},
	} {
		for _, onDiskOnly := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s, on disk only %v", c.name, onDiskOnly), func(t *testing.T) {
				staged := []*models.Mock{before, mid, end}
				if !onDiskOnly {
					staged = append(staged, c.last)
				}
				var times func(func(string, time.Time, bool, string) bool) error
				if onDiskOnly {
					times = onDisk(t, before, c.last, mid, end).RangeTimes
				}
				mm := stagedManagerOnDisk(t, staged, times)
				mm.SetPoolOwners(map[string]string{c.last.Name: "k"})
				mm.SetPoolNames([]string{"before", "mid", "end"})
				after := mm.teardownAfter("k", "")
				if after == nil || !after(end.Spec.ReqTimestampMock) {
					t.Fatal("end, sent after the test's last answer, is not the teardown")
				}
				if after(before.Spec.ReqTimestampMock) {
					t.Fatal("FALSE GREEN? before, sent before the test's last call, reads as the teardown")
				}
				if got := after(mid.Spec.ReqTimestampMock); got != c.midIsTD {
					t.Fatalf("mid, sent after the test's last call was sent and before its answer, is the teardown = %v, want %v", got, c.midIsTD)
				}
			})
		}
	}
}

// onClock is mk on clock (models.Origin.Clock), "" for none.
func onClock(clock string, mks ...*models.Mock) {
	for _, mk := range mks {
		mk.Origin = &models.Origin{Clock: clock}
	}
}

// A partial record keeps the tests it did not record anew, with the start-up
// and pool of the run that recorded them (run S, on its clock), beside the
// test it did (run L, on another clock, recorded later). Under keploy mock
// replay the call S's application makes after its last test (a Go
// mongo-driver's endSessions at client.Disconnect, made before L's test was
// ever recorded) is still the teardown: L's recording, on another clock, is no
// bound of it (fakeconn.SetClockID: stamps on two clocks are never ordered).
// Before, the bound was the latest recording of every clock, L's test, so
// the endSessions after the whole replay's last test missed and the replay
// failed while the partial record had judged it green. A recording on no
// clock still bounds every one (an untrusted time fails closed), and a pool
// call on no clock is still bounded by every recording.
func TestUnderMockReplayAnOlderRunsTeardownIsBoundedByItsOwnClock(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	const s, l = "proxy:5555", "proxy:1111"
	sBoot := exactMongo("s-boot", 100*time.Millisecond, models.LifetimePerTest)
	s1 := exactMongo("s1", time.Second, models.LifetimePerTest)
	sGap := exactMongo("s-gap", 1500*time.Millisecond, models.LifetimePerTest)
	s2 := exactMongo("s2", 2*time.Second, models.LifetimePerTest)
	sEnd := exactMongo("s-end", 3*time.Second, models.LifetimePerTest)
	onClock(s, sBoot, s1, sGap, s2, sEnd)
	// L's test, recorded anew 42 s later by another agent run.
	l1 := exactMongo("l1", 45*time.Second, models.LifetimePerTest)
	onClock(l, l1)
	slice := []string{"s-boot", "s-gap", "s-end"}
	owners := map[string]string{"s1": "k", "s2": "k", "l1": "k"}

	mm := stagedManagerOnDisk(t, []*models.Mock{sBoot, sGap, sEnd}, onDisk(t, sBoot, s1, sGap, s2, sEnd, l1).RangeTimes)
	mm.SetPoolOwners(owners)
	mm.SetPoolNames(slice)
	after := mm.teardownAfter("k", s)
	if after == nil || !after(sEnd.Spec.ReqTimestampMock) {
		t.Fatal("s-end, sent after every test S recorded, is not the teardown of S's clock: a newer run's test, on another clock, bounded it")
	}
	for _, mk := range []*models.Mock{sBoot, sGap} {
		if after(mk.Spec.ReqTimestampMock) {
			t.Errorf("FALSE GREEN? %s, sent before a test of its own clock, reads as the teardown", mk.Name)
		}
	}
	// The replay: T1 and T2 (S's), then T3 (L's), then the client's
	// disconnect.
	replay := func(t *testing.T, mm *MockManager) {
		t.Helper()
		mm.ServeExactPool("k", slice)
		perTestReads(t, mm, "before the first test")
		for _, tc := range []struct {
			name string
			own  *models.Mock
		}{{"T1", s1}, {"T2", s2}, {"T3", l1}} {
			mm.ServeExactTest(tc.name, []*models.Mock{tc.own})
			if !mm.DeleteFilteredMock(*tc.own) {
				t.Fatalf("precondition: %s's own recording did not answer it", tc.name)
			}
			mm.ServeExactPool("k", slice)
		}
		perTestReads(t, mm, "after the last test", "s-end")
		if !mm.DeleteFilteredMock(*sEnd) {
			t.Fatal("the call after the last test (endSessions) was not answered by S's recording of it")
		}
	}
	replay(t, mm)

	// The same set with every test's recordings staged in the trees and
	// none read from disk: the trees' recordings bound each clock apart
	// too.
	t.Run("the tests' recordings in the trees", func(t *testing.T) {
		mm := stagedManager(t, []*models.Mock{sBoot, s1, sGap, s2, sEnd, l1}, nil)
		mm.SetPoolOwners(owners)
		mm.SetPoolNames(slice)
		after := mm.teardownAfter("k", s)
		if after == nil || !after(sEnd.Spec.ReqTimestampMock) {
			t.Fatal("s-end, sent after every test S recorded, is not the teardown of S's clock: a newer run's test in the trees, on another clock, bounded it")
		}
		for _, mk := range []*models.Mock{sBoot, sGap} {
			if after(mk.Spec.ReqTimestampMock) {
				t.Errorf("FALSE GREEN? %s, sent before a test of its own clock in the trees, reads as the teardown", mk.Name)
			}
		}
		replay(t, mm)
	})
	t.Run("a recording on no clock bounds every clock", func(t *testing.T) {
		lu := exactMongo("l-untrusted", 44*time.Second, models.LifetimePerTest)
		mm := stagedManagerOnDisk(t, []*models.Mock{sBoot, sGap, sEnd}, onDisk(t, sBoot, s1, sGap, s2, sEnd, l1, lu).RangeTimes)
		mm.SetPoolOwners(map[string]string{"s1": "k", "s2": "k", "l1": "k", "l-untrusted": "k"})
		mm.SetPoolNames(slice)
		if after := mm.teardownAfter("k", s); after != nil && after(sEnd.Spec.ReqTimestampMock) {
			t.Fatal("FALSE GREEN? a recording on no clock, made after s-end by its time, did not bound S's clock")
		}
	})
	t.Run("a pool call on no clock is bounded by every clock", func(t *testing.T) {
		if after := mm.teardownAfter("k", ""); after == nil || after(sEnd.Spec.ReqTimestampMock) {
			t.Fatal("FALSE GREEN? a pool call on no clock, sent before L's test by its time, reads as the teardown")
		}
	})
	// A stated limit: where the test recorded anew (T2, s2 gone from the
	// set) was its run's last, that run's pool call between its previous
	// test and it is after every recording left on its clock, so it is the
	// teardown too. A clock orders only its own run's stamps, and no
	// recording of the set tells where S's T2 was.
	t.Run("the stated limit: the gap before a run's last test recorded anew", func(t *testing.T) {
		mm := stagedManagerOnDisk(t, []*models.Mock{sBoot, sGap, sEnd}, onDisk(t, sBoot, s1, sGap, sEnd, l1).RangeTimes)
		mm.SetPoolOwners(owners)
		mm.SetPoolNames(slice)
		after := mm.teardownAfter("k", s)
		if after == nil || !after(sGap.Spec.ReqTimestampMock) || after(sBoot.Spec.ReqTimestampMock) {
			t.Fatal("the stated limit changed: s-gap, after S's last test left in the set, is not S's teardown, or s-boot is")
		}
	})
	t.Run("a clock none of whose tests remain tells no teardown", func(t *testing.T) {
		if mm.teardownAfter("k", "proxy:9999") != nil {
			t.Fatal("FALSE GREEN? a clock no remaining test is on told a teardown bound")
		}
	})
}

// A partial record's tests recorded anew may make calls whose stamp names
// no clock (models.Origin.Clock ""): a call whose parser names none, or an
// HTTP call recorded before keploy's HTTP recorder named its clock, as in
// api-server's partial re-record of two tests from a whole record's set.
// Such a recording bounded every clock, so the newer run's calls, made after
// the older run's endSessions by their time, hid the older run's teardown:
// the call the application made at its disconnect after the last test
// missed, outside every test, and the partial record failed with every test
// passing. A recording on no clock that a test lists is of the run that
// recorded that test, whose clock the test's other recordings are on
// (runClocks): it bounds that clock alone. One that no test lists, or that
// only a test whose recordings are on no clock lists, still bounds every
// clock (fail closed), and one that tests of two runs list bounds both.
func TestUnderMockReplayATestsRecordingOnNoClockBoundsOnlyItsRunsClock(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	const s, l = "proxy:5555", "proxy:1111"
	sBoot := exactMongo("s-boot", 100*time.Millisecond, models.LifetimePerTest)
	s1 := exactMongo("s1", time.Second, models.LifetimePerTest)
	sGap := exactMongo("s-gap", 1500*time.Millisecond, models.LifetimePerTest)
	s2 := exactMongo("s2", 2*time.Second, models.LifetimePerTest)
	sEnd := exactMongo("s-end", 3*time.Second, models.LifetimePerTest)
	onClock(s, sBoot, s1, sGap, s2, sEnd)
	// T3, recorded anew 42 s later by another agent run: a Mongo call on
	// that run's clock and an HTTP call on none (recorded before the HTTP
	// recorder named its clock).
	l1 := exactMongo("l1", 45*time.Second, models.LifetimePerTest)
	onClock(l, l1)
	lHTTP := exactMock("l-http", 46*time.Second, models.LifetimePerTest)
	lHTTP.Kind = models.HTTP
	slice := []string{"s-boot", "s-gap", "s-end"}
	all := []*models.Mock{sBoot, s1, sGap, s2, sEnd, l1, lHTTP}
	tests := map[string][]string{"T1": {"s1"}, "T2": {"s2"}, "T3": {"l1", "l-http"}}
	owners := func(tests map[string][]string) map[string]string {
		out := map[string]string{}
		for _, names := range tests {
			for _, n := range names {
				out[n] = "k"
			}
		}
		return out
	}
	// As the agent hands the scope table (agent.PoolNamesSetter), through
	// the proxy; the tests' recordings on disk, or in the trees.
	setup := func(t *testing.T, tests map[string][]string, inTrees bool, ms ...*models.Mock) *MockManager {
		t.Helper()
		staged := []*models.Mock{sBoot, sGap, sEnd}
		if inTrees {
			staged = ms
		}
		var times func(func(string, time.Time, bool, string) bool) error
		if !inTrees {
			times = onDisk(t, ms...).RangeTimes
		}
		mm := stagedManagerOnDisk(t, staged, times)
		if err := (&Proxy{mockManager: mm}).SetPoolNames(slice, owners(tests), tests); err != nil {
			t.Fatalf("SetPoolNames: %v", err)
		}
		return mm
	}
	for _, inTrees := range []bool{false, true} {
		where := map[bool]string{false: "on disk", true: "in the trees"}[inTrees]
		t.Run("the tests' recordings "+where, func(t *testing.T) {
			mm := setup(t, tests, inTrees, all...)
			after := mm.teardownAfter("k", s)
			if after == nil || !after(sEnd.Spec.ReqTimestampMock) {
				t.Fatal("s-end, sent after every test S recorded, is not the teardown of S's clock: the newer run's HTTP call, on no clock, bounded it")
			}
			for _, mk := range []*models.Mock{sBoot, sGap} {
				if after(mk.Spec.ReqTimestampMock) {
					t.Errorf("FALSE GREEN? %s, sent before a test of its own clock, reads as the teardown", mk.Name)
				}
			}
			// The HTTP call still bounds its own run's clock.
			if after := mm.teardownAfter("k", l); after == nil || after(lHTTP.Spec.ReqTimestampMock) || !after(lHTTP.Spec.ReqTimestampMock.Add(time.Millisecond)) {
				t.Fatal("FALSE GREEN? the HTTP call T3 lists does not bound the clock of T3's run")
			}
			// The replay: T1 and T2 (S's), then T3 (L's), then the
			// client's disconnect.
			mm.ServeExactPool("k", slice)
			perTestReads(t, mm, "before the first test")
			for _, tc := range []struct {
				name string
				own  []*models.Mock
			}{{"T1", []*models.Mock{s1}}, {"T2", []*models.Mock{s2}}, {"T3", []*models.Mock{l1, lHTTP}}} {
				mm.ServeExactTest(tc.name, tc.own)
				for _, mk := range tc.own {
					if !mm.DeleteFilteredMock(*mk) {
						t.Fatalf("precondition: %s's own recording %s did not answer it", tc.name, mk.Name)
					}
				}
				mm.ServeExactPool("k", slice)
			}
			perTestReads(t, mm, "after the last test", "s-end")
			if !mm.DeleteFilteredMock(*sEnd) {
				t.Fatal("the call after the last test (endSessions) was not answered by S's recording of it")
			}
		})
	}
	// Each one below makes the HTTP call's run not known, or S's too, so it
	// bounds S's clock, and s-end, before it, is no teardown.
	for _, tc := range []struct {
		name  string
		tests map[string][]string
	}{
		{"no test lists it", map[string][]string{"T1": {"s1"}, "T2": {"s2"}, "T3": {"l1"}}},
		{"a test whose recordings are on no clock lists it too", map[string][]string{"T1": {"s1"}, "T2": {"s2"}, "T3": {"l1", "l-http"}, "T4": {"l-http"}}},
		{"only a test whose recordings are on no clock lists it", map[string][]string{"T1": {"s1"}, "T2": {"s2"}, "T3": {"l1"}, "T4": {"l-http"}}},
		{"tests of both runs list it", map[string][]string{"T1": {"s1"}, "T2": {"s2", "l-http"}, "T3": {"l1", "l-http"}}},
		{"its test's recordings are on both clocks", map[string][]string{"T1": {"s1"}, "T2": {"s2"}, "T3": {"l1", "s2", "l-http"}}},
	} {
		t.Run(tc.name+": it bounds every clock", func(t *testing.T) {
			for _, inTrees := range []bool{false, true} {
				mm := setup(t, tc.tests, inTrees, all...)
				if after := mm.teardownAfter("k", s); after == nil || after(sEnd.Spec.ReqTimestampMock) {
					t.Fatalf("FALSE GREEN? (in trees %v) the HTTP call, of no run known apart from S's, did not bound S's clock: s-end reads as the teardown", inTrees)
				}
				// Nor L's: whichever order the table names its tests in.
				if after := mm.teardownAfter("k", l); after == nil || after(lHTTP.Spec.ReqTimestampMock) {
					t.Fatalf("FALSE GREEN? (in trees %v) the HTTP call did not bound L's clock", inTrees)
				}
			}
		})
	}
	// A recording on a clock bounds that clock alone, whatever else its test
	// lists: T3's l1, beside S's s2, never bounds S's clock.
	t.Run("a test on both clocks: each recording on one bounds its own", func(t *testing.T) {
		for _, inTrees := range []bool{false, true} {
			mm := setup(t, map[string][]string{"T1": {"s1"}, "T2": {"s2"}, "T3": {"l1", "s2"}}, inTrees, sBoot, s1, sGap, s2, sEnd, l1)
			if after := mm.teardownAfter("k", s); after == nil || !after(sEnd.Spec.ReqTimestampMock) {
				t.Fatalf("(in trees %v) s-end is not S's teardown: l1, on L's clock, bounded S's", inTrees)
			}
		}
	})
	// The table is read as it was installed: a later write to the caller's
	// slices (T3's first recording is now S's s2) moves no bound.
	t.Run("a change to the caller's table after it was installed is not read", func(t *testing.T) {
		table := map[string][]string{"T1": {"s1"}, "T2": {"s2"}, "T3": {"l1", "l-http"}}
		mm := setup(t, table, false, all...)
		table["T3"][0] = "s2"
		if after := mm.teardownAfter("k", s); after == nil || !after(sEnd.Spec.ReqTimestampMock) {
			t.Fatal("the bound read the caller's table as written after it was installed: T3's HTTP call bounded S's clock")
		}
	})
	// Without the table (an agent handed none) every recording on no clock
	// bounds every clock, as before.
	t.Run("no table: it bounds every clock", func(t *testing.T) {
		mm := setup(t, nil, false, all...)
		if after := mm.teardownAfter("k", s); after == nil || after(sEnd.Spec.ReqTimestampMock) {
			t.Fatal("FALSE GREEN? with no scope table, the HTTP call did not bound S's clock")
		}
	})
	// A new replay session forgets the previous session's table: one that
	// names its pool and owners alone reads none.
	t.Run("a new session reads no earlier table", func(t *testing.T) {
		mm := setup(t, tests, false, all...)
		mm.ResetForReplaySession()
		mm.SetPoolOwners(owners(tests))
		mm.SetPoolNames(slice)
		if after := mm.teardownAfter("k", s); after == nil || after(sEnd.Spec.ReqTimestampMock) {
			t.Fatal("FALSE GREEN? a new session's bound read the previous session's table: the HTTP call did not bound S's clock")
		}
	})
	// The bound cached for one table is not the next one's.
	t.Run("a new table is read again", func(t *testing.T) {
		mm := setup(t, tests, false, all...)
		if after := mm.teardownAfter("k", s); after == nil || !after(sEnd.Spec.ReqTimestampMock) {
			t.Fatal("precondition: s-end is not S's teardown")
		}
		mm.SetPoolTests(map[string][]string{"T1": {"s1"}, "T2": {"s2"}, "T3": {"l1"}})
		if after := mm.teardownAfter("k", s); after == nil || after(sEnd.Spec.ReqTimestampMock) {
			t.Fatal("FALSE GREEN? the bound cached for the earlier table was read for a new one")
		}
	})
}

// A set recorded in one run is on one clock (the proxy's, or proxyless
// capture's ktime) or, for an untrusted stamp, on none: its teardown is the
// same with the clocks read as without them, every recording of the set
// bounding every pool call, as before the bound was per clock.
func TestUnderMockReplayOneRunsTeardownIsTheSameWithItsClock(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	mks := func() []*models.Mock {
		return []*models.Mock{
			exactMongo("boot", 100*time.Millisecond, models.LifetimePerTest),
			exactMongo("owned", time.Second, models.LifetimePerTest),
			exactMongo("owned-2", 1200*time.Millisecond, models.LifetimePerTest),
			exactMongo("gap", 1500*time.Millisecond, models.LifetimePerTest),
			// A test's recording on no clock, after the gap: the gap is
			// before a test's recording, though not before one on its clock.
			exactMongo("owned-2-untrusted", 2500*time.Millisecond, models.LifetimePerTest),
			exactMongo("end", 3*time.Second, models.LifetimePerTest),
			// A pool call on no clock, after every test.
			exactMongo("end-untrusted", 3500*time.Millisecond, models.LifetimePerTest),
		}
	}
	teardownOf := func(clocked bool) map[string]bool {
		ms := mks()
		if clocked {
			for _, mk := range ms {
				if mk.Name != "owned-2-untrusted" && mk.Name != "end-untrusted" {
					onClock("ktime:b00t.1", mk)
				}
			}
		}
		pool := []*models.Mock{ms[0], ms[3], ms[5], ms[6]}
		mm := stagedManagerOnDisk(t, pool, onDisk(t, ms...).RangeTimes)
		mm.SetPoolOwners(map[string]string{"owned": "k", "owned-2": "k", "owned-2-untrusted": "k"})
		mm.SetPoolNames([]string{"boot", "gap", "end", "end-untrusted"})
		got := map[string]bool{}
		for _, mk := range mm.teardown(mm.stagedNow(), "k", pool) {
			got[mk.Name] = true
		}
		return got
	}
	plain, clocked := teardownOf(false), teardownOf(true)
	if !plain["end"] || !plain["end-untrusted"] || plain["boot"] || plain["gap"] {
		t.Fatalf("precondition: the teardown without clocks is %v, want end and end-untrusted", plain)
	}
	if len(clocked) != len(plain) || !clocked["end"] || !clocked["end-untrusted"] {
		t.Fatalf("one run's teardown on its clock is %v, without it %v: the clock changed it", clocked, plain)
	}
}

// A recording with no request time has no place (models.PlaceOf): under
// keploy mock replay it bounds no teardown, read from the trees and from the
// store on disk alike, so the call after the last test misses (fail closed).
// On disk its place was kept as the zero time's UnixNano, which overflows to
// a time in 1754, and read back as a place before every pool call: every
// pool call, the application's boot among them, read as the teardown, while
// the trees told none.
func TestUnderMockReplayARecordingWithNoPlaceBoundsNoTeardown(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	boot := exactMongo("boot", 100*time.Millisecond, models.LifetimePerTest)
	end := exactMongo("end", 3*time.Second, models.LifetimePerTest)
	unplaced := exactMongo("unplaced", time.Second, models.LifetimePerTest)
	unplaced.Spec.ReqTimestampMock = time.Time{}
	if !models.PlaceOf(unplaced).IsZero() {
		t.Fatal("precondition: the recording with no request time has a place")
	}
	slice := []string{"boot", "end"}
	for _, c := range []struct {
		name   string
		staged []*models.Mock
		disk   bool
	}{
		{"in the trees", []*models.Mock{boot, unplaced, end}, false},
		{"on disk only", []*models.Mock{boot, end}, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			var times func(func(string, time.Time, bool, string) bool) error
			if c.disk {
				times = onDisk(t, boot, unplaced, end).RangeTimes
			}
			mm := stagedManagerOnDisk(t, c.staged, times)
			mm.SetPoolOwners(map[string]string{"unplaced": "k"})
			mm.SetPoolNames(slice)
			if after := mm.teardownAfter("k", ""); after != nil {
				t.Fatalf("FALSE GREEN? a recording with no place bounded the set's teardown (the boot reads as the teardown: %v)", after(boot.Spec.ReqTimestampMock))
			}
		})
	}
}

// keploy mock record stamps an answer once the relay's write of it to the
// application returns, and the application can read it, end its test and
// make its next call before that. The times are a loaded host's recording
// of a Go Mongo suite: its last test's delete was sent at .613347922 and its
// answer stamped at .615503334, 270 us after keploy read the driver's
// endSessions (.61523347), sent at client.Disconnect after that test, on the
// same connection (so after the app had the answer). Record charged the
// endSessions to the pool, by its place, after the last test's scope. Under
// keploy mock replay the set's teardown is bounded by the last place
// (models.PlaceOf) of the recordings outside the pool, as record charged
// them, read from the trees and from the store on disk alike: the
// endSessions is the teardown and answers the call after the last test.
// Bounded by the late answer instead, it was no teardown, the call missed
// outside every test, and the record failed with every test passing.
func TestUnderMockReplayAnAnswerStampedAfterTheNextCallKeepsTheTeardown(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	at := func(nsec int) time.Time { return time.Date(2026, 11, 2, 21, 56, 15, nsec, time.UTC) }
	boot := exactMongo("boot", 0, models.LifetimePerTest)
	boot.Spec.ReqTimestampMock, boot.Spec.ResTimestampMock = at(300000000), at(301000000)
	find := exactMongo("find", 0, models.LifetimePerTest)
	find.Spec.ReqTimestampMock, find.Spec.ResTimestampMock = at(612000000), at(612900000)
	del := exactMongo("delete", 0, models.LifetimePerTest)
	del.Spec.ReqTimestampMock, del.Spec.ResTimestampMock = at(613347922), at(615503334)
	end := exactMongo("end-sessions", 0, models.LifetimePerTest)
	end.Spec.ReqTimestampMock, end.Spec.ResTimestampMock = at(615233470), at(616645021)
	slice := []string{"boot", "end-sessions"}
	owners := map[string]string{"find": "items", "delete": "items"}
	for _, c := range []struct {
		name   string
		staged []*models.Mock
		disk   bool
	}{
		{"the test's recordings in the trees", []*models.Mock{boot, find, del, end}, false},
		{"the test's recordings on disk only", []*models.Mock{boot, end}, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			var times func(func(string, time.Time, bool, string) bool) error
			if c.disk {
				times = onDisk(t, boot, find, del, end).RangeTimes
			}
			mm := stagedManagerOnDisk(t, c.staged, times)
			mm.SetPoolOwners(owners)
			mm.SetPoolNames(slice)
			after := mm.teardownAfter("items", "")
			if after == nil || !after(end.Spec.ReqTimestampMock) {
				t.Fatal("endSessions, sent after the last test's delete was answered (same connection), is not the set's teardown: the delete's late answer stamp bounded it")
			}
			if after(boot.Spec.ReqTimestampMock) || after(del.Spec.ReqTimestampMock) {
				t.Fatal("FALSE GREEN? a recording sent before the last test's own reads as the set's teardown")
			}
			mm.ServeExactPool("items", slice)
			mm.ServeExactTest("items::TestCrossTestFind", []*models.Mock{find, del})
			if !mm.DeleteFilteredMock(*find) || !mm.DeleteFilteredMock(*del) {
				t.Fatal("precondition: the last test's own recordings did not answer it")
			}
			mm.ServeExactPool("items", slice)
			perTestReads(t, mm, "after the last test", "end-sessions")
			if !mm.DeleteFilteredMock(*end) {
				t.Fatal("the call after the last test (endSessions) was not answered by its recording")
			}
		})
	}
}
