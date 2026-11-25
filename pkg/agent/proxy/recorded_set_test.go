package proxy

import (
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
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

// diskListed registers kind, until t ends, as one whose parser holds a call
// while the set holds an unserved recording of it (models.RegisterAnswerPlaced,
// matching no mock, so none is placed at its answer), as the Redis parser
// registers its kind: the agent's on-disk store lists it in either switch
// mode (diskListsKind), so the set as recorded can tell its whole set.
func diskListed(t *testing.T, kind models.Kind) {
	t.Helper()
	t.Cleanup(models.RegisterAnswerPlaced(kind, func(*models.Mock) bool { return false }))
}

// The staged set as recorded is what the trees hold and what the agent keeps
// on disk, each once, whatever the open test's window shows: a test's own
// window holds one of the set's per-test recordings, and the range visits
// every one, a recording the trees and the disk both hold once. A manager
// the agent gave no on-disk source tells no whole set, and visits nothing:
// its trees may hold one window's. A connection's view forwards it.
func TestTheRecordedSetIsTheTreesAndTheDiskEachOnce(t *testing.T) {
	diskListed(t, models.HTTP)
	now := time.Now()
	early := r3Mock("t-early", now, "GET", "/a", "", `{"a":1}`)
	late := r3Mock("t-late", now.Add(time.Minute), "GET", "/b", "", `{"b":1}`)
	session := r3Mock("s-config", now.Add(-time.Minute), "GET", "/config", "", `{}`)
	mm := NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(mm.Close)
	// The agent hands a set's source, then stages it (Agent.stageMocks).
	stage := func() {
		mm.SetMocksWithWindow([]*models.Mock{early, late}, []*models.Mock{session}, now.Add(-time.Second), now.Add(time.Second))
	}
	stage()
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
	if got, whole := rangedNames(t, mm); whole || got != nil {
		t.Fatalf("a source handed and not staged yet told whole %v and visited %v; want the staging's, none", whole, got)
	}
	stage()
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
	stage()
	if whole, err := mm.RangeRecordedSet(models.HTTP, func(string, func() (*models.Mock, error)) bool { return true }); whole || !errors.Is(err, boom) {
		t.Fatalf("a source that failed told whole %v, %v; want false and its error", whole, err)
	}
	mm.SetRecordedOfKind(nil)
	stage()
	if got, whole := rangedNames(t, mm); whole || got != nil {
		t.Fatalf("a source unset told whole %v and visited %v; want neither", whole, got)
	}
	// A manager closed tells none, though its source was not unset.
	mm.SetRecordedOfKind(func(models.Kind, func(string, func() (*models.Mock, error)) bool) error { return nil })
	stage()
	mm.Close()
	if got, whole := rangedNames(t, mm); whole || got != nil {
		t.Fatalf("a closed manager told whole %v and visited %v; want neither", whole, got)
	}
}

// A call held to its round (HoldToRound) is told the set it was marked in,
// and nothing once the replay staged the set again: the set staged after
// its mark never decides what its recordings are.
func TestAHeldCallIsToldNoSetStagedAfterItsMark(t *testing.T) {
	diskListed(t, models.HTTP)
	for _, view := range []bool{false, true} {
		store, stage, want := heldGenerationStore(t, view)
		mm := managerOf(store)
		held, _ := holdCall(store)
		if got, whole := rangedNames(t, held); whole || got != nil {
			t.Fatalf("view %v: a held call of a manager with no source told whole %v and visited %v; want neither", view, whole, got)
		}
		mm.SetRecordedOfKind(func(models.Kind, func(string, func() (*models.Mock, error)) bool) error { return nil })
		stage()
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

// With the switch off too, the agent's on-disk store reads the mocks of a
// kind placed at its answer (DiskMocks.RangeKind), as Redis is, so a manager
// whose source it is tells the whole set: a set kept on disk under keploy
// test's default window is read as recorded in both switch modes.
func TestTheOnDiskSetIsReadByKindInEitherSwitchMode(t *testing.T) {
	diskListed(t, models.HTTP)
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
		mm.SetRecordedOfKind(disk.RangeKind)
		mm.SetMocksWithWindow([]*models.Mock{early}, nil, now.Add(-time.Second), now.Add(time.Second))
		got, whole := rangedNames(t, mm)
		mm.Close()
		_ = disk.Close()
		provenswitch.Unpin()
		if want := []string{"t-early", "t-late"}; !whole || !reflect.DeepEqual(got, want) {
			t.Fatalf("switch %v: the range told whole %v and visited %v; want the whole set %v", on, whole, got, want)
		}
	}
}

// Without the switch the agent's on-disk store lists only the kinds placed at
// their answer (diskListsKind), so it ranges none of another kind's
// recordings: the set as recorded tells no whole set of that kind, never one
// that lacks its on-disk recordings, and visits nothing, so its parser
// decides from its reads. Under the switch the disk lists every kind, and
// the same set is whole.
func TestAKindTheDiskDoesNotListIsToldNoWholeSet(t *testing.T) {
	for _, on := range []bool{false, true} {
		got, whole := func() ([]string, bool) {
			provenswitch.Pin(on)
			defer provenswitch.Unpin()
			now := time.Now()
			early := r3Mock("t-early", now, "GET", "/a", "", `{"a":1}`)
			late := r3Mock("t-late", now.Add(time.Minute), "GET", "/b", "", `{"b":1}`)
			disk, err := NewDiskMocks(zap.NewNop())
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = disk.Close() }()
			for _, m := range []*models.Mock{early, late} {
				if err := disk.Add(m); err != nil {
					t.Fatal(err)
				}
			}
			disk.Finalize()
			mm := NewMockManager(nil, nil, zap.NewNop())
			defer mm.Close()
			mm.SetRecordedOfKind(disk.RangeKind)
			mm.SetMocksWithWindow([]*models.Mock{early}, nil, now.Add(-time.Second), now.Add(time.Second))
			return rangedNames(t, mm)
		}()
		if on {
			if want := []string{"t-early", "t-late"}; !whole || !reflect.DeepEqual(got, want) {
				t.Fatalf("switch on: the range told whole %v and visited %v; want the whole set %v", whole, got, want)
			}
			continue
		}
		if whole || got != nil {
			t.Fatalf("switch off, a kind the disk does not list: the range told whole %v and visited %v; want neither", whole, got)
		}
	}
}

// setSource is a set's on-disk source of HTTP recordings: names, each its own
// recording.
func setSource(names ...string) func(models.Kind, func(string, func() (*models.Mock, error)) bool) error {
	now := time.Now()
	mocks := make([]*models.Mock, len(names))
	for i, n := range names {
		mocks[i] = r3Mock(n, now, "GET", "/"+n, "", `{}`)
	}
	return func(kind models.Kind, visit func(string, func() (*models.Mock, error)) bool) error {
		for _, m := range mocks {
			if m.Kind == kind && !visit(m.Name, func() (*models.Mock, error) { return m, nil }) {
				return nil
			}
		}
		return nil
	}
}

// The agent hands the next set's on-disk source before it stages that set
// (Agent.stageMocks). A read of the set as recorded between the two ranges
// the set staged, its trees and its source, never its trees with the next
// set's on-disk recordings: the staging installs the source it was handed
// with the trees it builds, and the range reads both as one staging left
// them.
func TestARangeBeforeTheNextSetIsStagedRangesTheSetStaged(t *testing.T) {
	diskListed(t, models.HTTP)
	now := time.Now()
	mm := NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(mm.Close)
	stageSet := func(set string) {
		mm.SetRecordedOfKind(setSource(set + "-disk"))
		mm.SetMocksWithWindow(nil, []*models.Mock{r3Mock(set+"-tree", now, "GET", "/"+set, "", `{}`)}, models.BaseTime, now.Add(time.Hour))
	}
	stageSet("a")
	if got, whole := rangedNames(t, mm); !whole || !reflect.DeepEqual(got, []string{"a-disk", "a-tree"}) {
		t.Fatalf("set a staged: the range told whole %v and visited %v; want set a's", whole, got)
	}
	mm.SetRecordedOfKind(setSource("b-disk"))
	if got, whole := rangedNames(t, mm); !whole || !reflect.DeepEqual(got, []string{"a-disk", "a-tree"}) {
		t.Fatalf("set b's source handed, set a still staged: the range told whole %v and visited %v; want set a's alone", whole, got)
	}
	mm.SetMocksWithWindow(nil, []*models.Mock{r3Mock("b-tree", now, "GET", "/b", "", `{}`)}, models.BaseTime, now.Add(time.Hour))
	if got, whole := rangedNames(t, mm); !whole || !reflect.DeepEqual(got, []string{"b-disk", "b-tree"}) {
		t.Fatalf("set b staged: the range told whole %v and visited %v; want set b's", whole, got)
	}
	// The bare tier setters stage too, and install what was handed.
	mm.SetRecordedOfKind(setSource("c-disk"))
	mm.SetUnFilteredMocks([]*models.Mock{r3Mock("c-tree", now, "GET", "/c", "", `{}`)})
	if got, whole := rangedNames(t, mm); !whole || !reflect.DeepEqual(got, []string{"b-tree", "c-disk", "c-tree"}) {
		t.Fatalf("a bare session setter staged set c: the range told whole %v and visited %v; want the trees it left and set c's source", whole, got)
	}
	mm.SetRecordedOfKind(setSource("d-disk"))
	mm.SetFilteredMocks(nil)
	if got, whole := rangedNames(t, mm); !whole || !reflect.DeepEqual(got, []string{"b-tree", "c-tree", "d-disk"}) {
		t.Fatalf("a bare per-test setter staged: the range told whole %v and visited %v; want the trees it left and the source it installed", whole, got)
	}
}

// Ranges made while the agent stages set after set, each set's source handed
// before its staging as the agent does, each visit one set's recordings
// alone: the staging changes the trees and installs the source under swapMu,
// and the range reads them together under it.
func TestRangesInterleavedWithStagingsEachSeeOneSet(t *testing.T) {
	diskListed(t, models.HTTP)
	now := time.Now()
	mm := NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(mm.Close)
	stageSet := func(i int) {
		set := "s" + strconv.Itoa(i)
		mm.SetRecordedOfKind(setSource(set+"-disk-1", set+"-disk-2"))
		mm.SetMocksWithWindow(nil, []*models.Mock{
			r3Mock(set+"-tree-1", now, "GET", "/"+set+"/1", "", `{}`),
			r3Mock(set+"-tree-2", now, "GET", "/"+set+"/2", "", `{}`),
		}, models.BaseTime, now.Add(time.Hour))
	}
	stageSet(0)
	const stagings, readers = 400, 4
	done := make(chan struct{})
	errs := make(chan string, readers)
	var wg sync.WaitGroup
	for r := 0; r < readers; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-done:
					return
				default:
				}
				sets := map[string]int{}
				whole, err := mm.RangeRecordedSet(models.HTTP, func(name string, _ func() (*models.Mock, error)) bool {
					sets[name[:strings.IndexByte(name, '-')]]++
					return true
				})
				if err != nil || !whole || len(sets) != 1 {
					errs <- fmt.Sprintf("a range told whole %v, %v and visited the recordings of sets %v; want one set's four", whole, err, sets)
					return
				}
				for set, n := range sets {
					if n != 4 {
						errs <- fmt.Sprintf("a range visited %d recordings of set %s; want its four", n, set)
						return
					}
				}
			}
		}()
	}
	for i := 1; i <= stagings; i++ {
		stageSet(i)
	}
	close(done)
	wg.Wait()
	close(errs)
	if msg, bad := <-errs; bad {
		t.Fatal(msg)
	}
}
