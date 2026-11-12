package proxy

import (
	"errors"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// named accepts the recording called name alone.
func named(name string) func(*models.Mock) bool {
	return func(m *models.Mock) bool { return m.Name == name }
}

// diskOf is an on-disk store holding ms, closed with the test.
func diskOf(t *testing.T, ms ...*models.Mock) *DiskMocks {
	t.Helper()
	disk, err := NewDiskMocks(zap.NewNop())
	if err != nil {
		t.Fatalf("on-disk store: %v", err)
	}
	t.Cleanup(func() { _ = disk.Close() })
	for _, m := range ms {
		if err := disk.Add(m); err != nil {
			t.Fatalf("adding %s: %v", m.Name, err)
		}
	}
	disk.Finalize()
	return disk
}

// hasUnserved is HasUnservedRecording of kind for the recording called name,
// failing the test on an error.
func hasUnserved(t *testing.T, mm *MockManager, kind models.Kind, name string) bool {
	t.Helper()
	got, err := mm.HasUnservedRecording(kind, named(name))
	if err != nil {
		t.Fatalf("HasUnservedRecording(%s): %v", name, err)
	}
	return got
}

// Without exact serving, under either switch: a later test's recording the agent keeps on disk is
// unserved until a call takes it in its own test's window, and is not once
// it answered, though the disk still holds it. Another recording of the set,
// or one of another kind, is never it.
func TestALaterTestsRecordingOnDiskIsUnservedUntilItAnswers(t *testing.T) {
	for _, sw := range []string{"0", "1"} {
		t.Run("KEPLOY_PROVEN="+sw, func(t *testing.T) {
			t.Setenv(provenswitch.Env, sw)
			aLaterTestsRecordingOnDiskIsUnservedUntilItAnswers(t)
		})
	}
}

func aLaterTestsRecordingOnDiskIsUnservedUntilItAnswers(t *testing.T) {
	// Without the switch the disk lists by kind only the kinds a parser
	// holds calls of (models.AnswerPlacedKind).
	t.Cleanup(models.RegisterAnswerPlaced(models.HTTP, func(*models.Mock) bool { return false }))
	t0 := time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC)
	first := keyedRecording("mock-1", models.HTTP, 1, "/a", t0)
	later := keyedRecording("mock-2", models.HTTP, 2, "/b", t0.Add(10*time.Second))
	disk := diskOf(t, first, later)
	mm := NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(mm.Close)
	window, err := disk.LoadWindow(t0.Add(-time.Second), t0.Add(time.Second))
	if err != nil || len(window) != 1 {
		t.Fatalf("the first window loads %d (%v), want 1", len(window), err)
	}
	mm.SetMocksWithWindow(window, nil, t0.Add(-time.Second), t0.Add(time.Second))

	if hasUnserved(t, mm, models.HTTP, later.Name) {
		t.Fatal("with no disk reader the trees hold no later test's recording, yet one was found")
	}
	mm.SetRecordedOfKind(disk.RangeKind)
	if !hasUnserved(t, mm, models.HTTP, later.Name) {
		t.Fatal("the later test's recording on disk was not found")
	}
	// A parser reads the store through the worker-scope wrap (app-start
	// marks, a scoped worker), which must forward the check.
	var wrapped integrations.MockMemDb = &scopedMockDb{MockMemDb: mm}
	if f, ok := wrapped.(integrations.UnservedRecordingFinder); !ok {
		t.Fatal("the worker-scope wrap is no UnservedRecordingFinder")
	} else if got, err := f.HasUnservedRecording(models.HTTP, named(later.Name)); err != nil || !got {
		t.Fatalf("through the worker-scope wrap found %v (%v), want the later test's recording as the store found it", got, err)
	}
	if hasUnserved(t, mm, models.GENERIC, later.Name) {
		t.Fatal("a recording of another kind was found")
	}
	if !hasUnserved(t, mm, models.HTTP, first.Name) {
		t.Fatal("the open test's recording, not answered yet, was not found")
	}
	staged, _ := mm.GetFilteredMocks()
	if len(staged) != 1 || !mm.DeleteFilteredMock(*staged[0]) {
		t.Fatal("the open test's recording did not answer")
	}
	if hasUnserved(t, mm, models.HTTP, first.Name) {
		t.Fatal("a recording that answered, kept on disk still, was found unserved")
	}

	next, err := disk.LoadWindow(t0.Add(9*time.Second), t0.Add(11*time.Second))
	if err != nil || len(next) != 1 {
		t.Fatalf("the later window loads %d (%v), want 1", len(next), err)
	}
	mm.SetMocksWithWindow(next, nil, t0.Add(9*time.Second), t0.Add(11*time.Second))
	staged, _ = mm.GetFilteredMocks()
	if len(staged) != 1 || !mm.MarkMockAsUsed(*staged[0]) {
		t.Fatal("the later test's recording did not answer")
	}
	if hasUnserved(t, mm, models.HTTP, later.Name) {
		t.Fatal("the later test's recording answered and was found unserved")
	}

	// A new replay session, its set staged again, has served nothing.
	mm.ResetForReplaySession()
	mm.SetRecordedOfKind(disk.RangeKind)
	if !hasUnserved(t, mm, models.HTTP, first.Name) {
		t.Fatal("a new session counts the previous session's answers")
	}
}

// Under exact serving a test's recording answers without leaving its tree:
// it is unserved until then, and not after.
func TestAnExactTestsRecordingIsUnservedUntilItAnswers(t *testing.T) {
	t0 := time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC)
	own := keyedRecording("mock-1", models.HTTP, 1, "/a", t0)
	mm := stagedManager(t, []*models.Mock{own}, nil)
	mm.ServeExactTest("T1", []*models.Mock{own})
	if !hasUnserved(t, mm, models.HTTP, own.Name) {
		t.Fatal("the open test's own recording, not answered yet, was not found")
	}
	pt, _ := mm.GetPerTestMocksInWindow()
	if len(pt) != 1 || !mm.MarkMockAsUsed(*pt[0]) {
		t.Fatalf("the test's recording did not answer (%d shown)", len(pt))
	}
	if hasUnserved(t, mm, models.HTTP, own.Name) {
		t.Fatal("a recording that answered, still in its tree, was found unserved")
	}
}

// A recording the trees hold is not decoded again from the disk, and a
// recording the disk cannot read is the finder's error, as is the disk's own.
// A source that loads no mock for a name finds none, and a nil match none.
func TestTheUnservedFinderReadsTheDiskOnceAndSaysWhyItCannot(t *testing.T) {
	t0 := time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC)
	own := keyedRecording("mock-1", models.HTTP, 1, "/a", t0)
	mm := NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(mm.Close)
	mm.SetMocksWithWindow([]*models.Mock{own}, nil, t0.Add(-time.Second), t0.Add(time.Second))
	loads := 0
	mm.SetRecordedOfKind(func(_ models.Kind, visit func(string, func() (*models.Mock, error)) bool) error {
		visit(own.Name, func() (*models.Mock, error) { loads++; return own, nil })
		visit("mock-gone", func() (*models.Mock, error) { loads++; return nil, nil })
		return nil
	})
	if hasUnserved(t, mm, models.HTTP, "nothing") || loads != 1 {
		t.Fatalf("decoded %d recordings from disk, want 1: the trees' own is not decoded again", loads)
	}
	if got, err := mm.HasUnservedRecording(models.HTTP, nil); got || err != nil {
		t.Fatalf("a nil match found %v (%v)", got, err)
	}

	broken := errors.New("the store's file is gone")
	mm.SetRecordedOfKind(func(_ models.Kind, visit func(string, func() (*models.Mock, error)) bool) error {
		visit("mock-2", func() (*models.Mock, error) { return nil, broken })
		return nil
	})
	if _, err := mm.HasUnservedRecording(models.HTTP, named("mock-2")); !errors.Is(err, broken) {
		t.Fatalf("a recording the disk cannot read gave %v, want its error", err)
	}
	mm.SetRecordedOfKind(func(models.Kind, func(string, func() (*models.Mock, error)) bool) error { return broken })
	if _, err := mm.HasUnservedRecording(models.HTTP, named("mock-2")); !errors.Is(err, broken) {
		t.Fatalf("a disk that cannot be read gave %v, want its error", err)
	}
}

// Without the switch the on-disk store lists by kind only the kinds a parser
// holds calls of (models.AnswerPlacedKind): a name per on-disk mock of every
// other kind would be kept for no reader.
func TestWithoutTheSwitchTheDiskListsOnlyAHeldKindByKind(t *testing.T) {
	t.Setenv(provenswitch.Env, "0")
	t0 := time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC)
	disk := diskOf(t, keyedRecording("mock-1", models.HTTP, 1, "/a", t0))
	visited := 0
	if err := disk.RangeKind(models.HTTP, func(string, func() (*models.Mock, error)) bool { visited++; return true }); err != nil || visited != 0 {
		t.Fatalf("a kind no parser holds calls of was listed: %d visited (%v)", visited, err)
	}
}
