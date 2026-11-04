package proxy

import (
	"errors"
	"reflect"
	"sort"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// rangedNames is what integrations.RangeRecordedSet visits of db's HTTP
// recordings, sorted, each loaded, and whether db told the whole set.
func rangedNames(t *testing.T, db integrations.MockMemDb) ([]string, bool) {
	t.Helper()
	var names []string
	whole, err := integrations.RangeRecordedSet(db, models.HTTP, func(name string, load func() (*models.Mock, error)) bool {
		m, err := load()
		if err != nil || m == nil || m.Name != name {
			t.Fatalf("load of %q gave %v, %v", name, m, err)
		}
		names = append(names, name)
		return true
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(names)
	return names, whole
}

// The staged set as recorded is what the trees hold and what the agent keeps
// on disk, each once, whatever the open test's window shows: a test's own
// window holds one of the set's per-test recordings, and the range visits
// every one, a recording the trees and the disk both hold once. A manager
// the agent gave no on-disk source tells no whole set, and visits nothing:
// its trees may hold one window's. A connection's view forwards it.
func TestTheRecordedSetIsTheTreesAndTheDiskEachOnce(t *testing.T) {
	now := time.Now()
	early := r3Mock("t-early", now, "GET", "/a", "", `{"a":1}`)
	late := r3Mock("t-late", now.Add(time.Minute), "GET", "/b", "", `{"b":1}`)
	session := r3Mock("s-config", now.Add(-time.Minute), "GET", "/config", "", `{}`)
	mm := NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(mm.Close)
	mm.SetMocksWithWindow([]*models.Mock{early, late}, []*models.Mock{session}, now.Add(-time.Second), now.Add(time.Second))
	if got, whole := rangedNames(t, mm); whole || got != nil {
		t.Fatalf("with no on-disk source the range told whole %v and visited %v; want neither", whole, got)
	}
	disk := map[string]*models.Mock{"t-early": early, "t-late": late}
	mm.SetRecordedOfKind(func(kind models.Kind, visit func(string, func() (*models.Mock, error)) bool) error {
		// A source that lists a name twice is visited for it once.
		for _, n := range []string{"t-early", "t-late", "t-late"} {
			if m := disk[n]; m.Kind == kind && !visit(n, func() (*models.Mock, error) { return m, nil }) {
				return nil
			}
		}
		return nil
	})
	want := []string{"s-config", "t-early", "t-late"}
	for name, db := range map[string]integrations.MockMemDb{"manager": mm, "view": &scopedMockDb{MockMemDb: mm}} {
		if got, whole := rangedNames(t, db); !whole || !reflect.DeepEqual(got, want) {
			t.Fatalf("%s: the range told whole %v and visited %v; want the whole set %v", name, whole, got, want)
		}
	}
	// A visit that stops is not visited again, and a source's error is told.
	n := 0
	if whole, err := mm.RangeRecordedSet(models.HTTP, func(string, func() (*models.Mock, error)) bool { n++; return false }); !whole || err != nil || n != 1 {
		t.Fatalf("a range stopped at its first visit told whole %v, %v after %d visits; want true, nil after 1", whole, err, n)
	}
	boom := errors.New("disk read failed")
	mm.SetRecordedOfKind(func(models.Kind, func(string, func() (*models.Mock, error)) bool) error { return boom })
	if whole, err := mm.RangeRecordedSet(models.HTTP, func(string, func() (*models.Mock, error)) bool { return true }); whole || !errors.Is(err, boom) {
		t.Fatalf("a source that failed told whole %v, %v; want false and its error", whole, err)
	}
	mm.SetRecordedOfKind(nil)
	if got, whole := rangedNames(t, mm); whole || got != nil {
		t.Fatalf("a source unset told whole %v and visited %v; want neither", whole, got)
	}
	// A manager closed tells none, though its source was not unset.
	mm.SetRecordedOfKind(func(models.Kind, func(string, func() (*models.Mock, error)) bool) error { return nil })
	mm.Close()
	if got, whole := rangedNames(t, mm); whole || got != nil {
		t.Fatalf("a closed manager told whole %v and visited %v; want neither", whole, got)
	}
}

// A call held to its round (HoldToRound) is told the set it was marked in,
// and nothing once the replay staged the set again: the set staged after
// its mark never decides what its recordings are.
func TestAHeldCallIsToldNoSetStagedAfterItsMark(t *testing.T) {
	for _, view := range []bool{false, true} {
		store, stage, want := heldGenerationStore(t, view)
		mm := managerOf(store)
		held, _ := holdCall(store)
		if got, whole := rangedNames(t, held); whole || got != nil {
			t.Fatalf("view %v: a held call of a manager with no source told whole %v and visited %v; want neither", view, whole, got)
		}
		mm.SetRecordedOfKind(func(models.Kind, func(string, func() (*models.Mock, error)) bool) error { return nil })
		held, _ = holdCall(store)
		if got, whole := rangedNames(t, held); !whole || !reflect.DeepEqual(got, want) {
			t.Fatalf("view %v: a held call's range told whole %v and visited %v; want %v", view, whole, got, want)
		}
		n := 0
		if whole, err := integrations.RangeRecordedSet(held, models.HTTP, func(string, func() (*models.Mock, error)) bool { n++; return false }); !whole || err != nil || n != 1 {
			t.Fatalf("view %v: a held range stopped at its first visit told whole %v, %v after %d visits; want true, nil after 1", view, whole, err, n)
		}
		held, restaged := holdCall(store)
		stage()
		if got, whole := rangedNames(t, held); whole || got != nil {
			t.Fatalf("view %v: a held call the staging moved under told whole %v and visited %v; want neither", view, whole, got)
		}
		if !restaged() {
			t.Fatalf("view %v: a held call the staging moved under does not say the set was staged again", view)
		}
	}
}

// managerOf is the manager under db, a manager or a connection's view of one.
func managerOf(db integrations.MockMemDb) *MockManager {
	if s, ok := db.(*scopedMockDb); ok {
		return s.MockMemDb.(*MockManager)
	}
	return db.(*MockManager)
}

// With the switch off too, the agent's on-disk store reads a kind's mocks
// (DiskMocks.RangeKind), so a manager whose source it is tells the whole
// set: a set kept on disk under keploy test's default window is read as
// recorded in both switch modes.
func TestTheOnDiskSetIsReadByKindInEitherSwitchMode(t *testing.T) {
	for _, on := range []bool{false, true} {
		provenswitch.Pin(on)
		now := time.Now()
		early := r3Mock("t-early", now, "GET", "/a", "", `{"a":1}`)
		late := r3Mock("t-late", now.Add(time.Minute), "GET", "/b", "", `{"b":1}`)
		disk, err := NewDiskMocks(zap.NewNop())
		if err != nil {
			provenswitch.Unpin()
			t.Fatal(err)
		}
		for _, m := range []*models.Mock{early, late} {
			if err := disk.Add(m); err != nil {
				provenswitch.Unpin()
				t.Fatal(err)
			}
		}
		disk.Finalize()
		mm := NewMockManager(nil, nil, zap.NewNop())
		mm.SetMocksWithWindow([]*models.Mock{early}, nil, now.Add(-time.Second), now.Add(time.Second))
		mm.SetRecordedOfKind(disk.RangeKind)
		got, whole := rangedNames(t, mm)
		mm.Close()
		_ = disk.Close()
		provenswitch.Unpin()
		if want := []string{"t-early", "t-late"}; !whole || !reflect.DeepEqual(got, want) {
			t.Fatalf("switch %v: the range told whole %v and visited %v; want the whole set %v", on, whole, got, want)
		}
	}
}
