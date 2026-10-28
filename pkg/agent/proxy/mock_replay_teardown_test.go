package proxy

import (
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// keploy mock replay sends, with the pool's names (SetPoolNames), the
// recorded window of every test of its set, as it derives them from the
// set's mappings (pkg/service/mock recordedTestWindows): from the test's
// recorded begin to the last request or answer of the recordings its mapping
// lists. The pool's names still decide the set's teardown bound
// (teardownAfter): after every staged data-plane recording outside the pool;
// the windows serve the learn step. Of a set of one test process keploy
// test's bound (every window's end, WindowSchedule.After) tells the same
// teardown, since under keploy mock replay every staged recording is a
// test's or the pool's (poolSlices): after the last test a per-test read
// still shows the pool's recording made after every test's own (a Go
// mongo-driver's endSessions at client.Disconnect), and it answers; the same
// call made once more misses (fail closed), and neither a pool recording
// made before the first test or in a gap between two tests nor a test's own
// recording is shown.
func TestUnderMockReplayTheRecordedWindowsKeepTheSetsTeardown(t *testing.T) {
	boot := exactMongo("boot", 200*time.Millisecond, models.LifetimePerTest)
	owned := exactMongo("owned", time.Second, models.LifetimePerTest)
	gap := exactMongo("gap", 1600*time.Millisecond, models.LifetimePerTest)
	owned2 := exactMongo("owned-2", 2*time.Second, models.LifetimePerTest)
	// T2's own, sent in its window and answered later: its window ends at
	// that answer, so a pool recording sent before it is no teardown.
	late := exactMongo("owned-2-late", 2200*time.Millisecond, models.LifetimePerTest)
	late.Spec.ResTimestampMock = exactT0.Add(2600 * time.Millisecond)
	inFlight := exactMongo("pool-while-t2-waits", 2400*time.Millisecond, models.LifetimePerTest)
	end := exactMongo("end-sessions", 3*time.Second, models.LifetimePerTest)
	// A protocol-phase recording made later is no test's, and no bound's.
	hs := configMock("handshake", 3500*time.Millisecond)
	slice := []string{"boot", "gap", "pool-while-t2-waits", "end-sessions"}
	// What keploy mock replay derives: T began at +0.5s and T2 at +1.7s;
	// each ends at the last request or answer of the recordings it owns.
	windows := []models.TestWindow{
		{TestCase: "T", Start: exactT0.Add(500 * time.Millisecond), End: owned.Spec.ResTimestampMock},
		{TestCase: "T2", Start: exactT0.Add(1700 * time.Millisecond), End: late.Spec.ResTimestampMock},
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
			after := mm.teardownAfter("k")
			if after == nil || !after(end.Spec.ReqTimestampMock) {
				t.Fatalf("end-sessions, sent after every test's recording, is not the set's teardown")
			}
			for _, mk := range []*models.Mock{boot, owned, gap, owned2, late, inFlight} {
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
			perTestReads(t, mm, "between two tests", "end-sessions")
			mm.ServeExactTest("T2", []*models.Mock{owned2, late})
			if !mm.DeleteFilteredMock(*owned2) || !mm.DeleteFilteredMock(*late) {
				t.Fatal("precondition: the second test's own recordings did not answer it")
			}
			mm.ServeExactPool("k", slice)
			perTestReads(t, mm, "after the last test", "end-sessions")
			if !mm.DeleteFilteredMock(*end) {
				t.Fatal("the pool's recording of the call after the last test (endSessions) did not answer it")
			}
			perTestReads(t, mm, "after it answered")
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
	if after := mm.teardownAfter("k"); after == nil || !after(gap.Spec.ReqTimestampMock) {
		t.Fatal("precondition: the staged trees alone did not put the bound at the test's early call")
	}
	mm.SetRecordingTimes(disk.RangeTimes)
	after := mm.teardownAfter("k")
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
	mm.SetRecordingTimes(other.RangeTimes)
	if after := mm.teardownAfter("k"); after == nil || !after(gap.Spec.ReqTimestampMock) {
		t.Fatal("a new set's store was not read again: its bound is the first set's")
	}

	// A store that cannot be read tells no bound: the call after the last
	// test misses rather than pairing with a recording made between tests.
	if err := other.Close(); err != nil {
		t.Fatal(err)
	}
	mm.SetRecordingTimes(other.RangeTimes)
	if mm.teardownAfter("k") != nil {
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
	mm := stagedManager(t, []*models.Mock{aBoot, aEnd, bBoot, bEnd}, nil)
	mm.SetPoolOwners(map[string]string{"a1": "a", "b1": "b"})
	mm.SetPoolNames([]string{"a-boot", "a-end", "b-boot", "b-end"})
	mm.SetRecordingTimes(disk.RangeTimes)
	for _, tc := range []struct {
		key  string
		mk   *models.Mock
		want bool
	}{{"a", aEnd, true}, {"b", bEnd, true}, {"b", aEnd, false}, {"a", aBoot, false}} {
		after := mm.teardownAfter(tc.key)
		if got := after != nil && after(tc.mk.Spec.ReqTimestampMock); got != tc.want {
			t.Errorf("key %s: %s is the teardown = %v, want %v", tc.key, tc.mk.Name, got, tc.want)
		}
	}
}

// The bound a test's recording on disk sets is its answer's time, not its
// request's: a pool recording sent while the test's last call waited for its
// answer is no teardown (the test was still running), one sent after the
// answer is.
func TestUnderMockReplayTheTeardownIsAfterTheTestsLastAnswerOnDisk(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	owned := exactMongo("owned", time.Second, models.LifetimePerTest)
	owned.Spec.ResTimestampMock = exactT0.Add(2 * time.Second)
	mid := exactMongo("mid", 1500*time.Millisecond, models.LifetimePerTest)
	end := exactMongo("end", 3*time.Second, models.LifetimePerTest)
	disk := onDisk(t, owned, mid, end)
	mm := stagedManager(t, []*models.Mock{mid, end}, nil)
	mm.SetPoolOwners(map[string]string{"owned": "k"})
	mm.SetPoolNames([]string{"mid", "end"})
	mm.SetRecordingTimes(disk.RangeTimes)
	after := mm.teardownAfter("k")
	if after == nil || !after(end.Spec.ReqTimestampMock) {
		t.Fatal("end, sent after the test's last answer, is not the teardown")
	}
	if after(mid.Spec.ReqTimestampMock) {
		t.Fatal("FALSE GREEN? mid, sent before the test's last call was answered, reads as the teardown")
	}
}
