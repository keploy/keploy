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
// alone: the staging changes the trees and publishes them with the source as
// one set (stagedSet), and the range reads that one set.
func TestRangesInterleavedWithStagingsEachSeeOneSet(t *testing.T) {
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

// handAndStage hands mm src as the agent hands a set's on-disk source, before
// a staging, and stages the per-test tier again, empty in the sets that use
// it: the staging publishes what was handed with its trees (stagedSet), the
// trees left as they were.
func handAndStage(mm *MockManager, src func(models.Kind, func(string, func() (*models.Mock, error)) bool) error) {
	mm.SetRecordedOfKind(src)
	mm.SetFilteredMocks(nil)
}

// handKeyedAndStage is handAndStage of the keyed on-disk source
// (SetRecordingSource), which the agent hands before a staging too.
func handKeyedAndStage(mm *MockManager, src RecordingSource) {
	mm.SetRecordingSource(src)
	mm.SetFilteredMocks(nil)
}

// keyedSetSource is a keyed on-disk source (RecordingSource) that files each
// of names under every key.
func keyedSetSource(names ...string) RecordingSource {
	of := setSource(names...)
	return func(kind models.Kind, _ []string, visit func(string, func() (*models.Mock, error)) bool) error {
		return of(kind, visit)
	}
}

// pairingSet is a manager that pairs (record's full round under the proven
// switch, a set served by the background rules) in a set staged as the agent
// stages one: its on-disk source handed, then its mocks staged, then its
// background named and its test T1 open. stageSet stages set after set: each
// holds its test's own recording (set-own), another test's (set-tree), one
// the agent keeps on disk (set-disk) and one it keeps on disk by key
// (set-keyed).
func pairingSet(t *testing.T) (*MockManager, func(set string)) {
	t.Helper()
	t.Setenv(provenswitch.Env, "1")
	mm := NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(mm.Close)
	mm.SetPairing(true)
	stageSet := func(set string) {
		own := httpCall(set+"-own", 0, "/"+set+"-own")
		mm.StageExactBackground(models.RecordedWithS12Routing)
		mm.SetRecordedOfKind(setSource(set + "-disk"))
		mm.SetRecordingSource(keyedSetSource(set + "-keyed"))
		mm.SetMocksWithWindow(nil, []*models.Mock{own, httpCall(set+"-tree", time.Second, "/"+set)}, models.BaseTime, time.Now())
		mm.ServeExactBackground(models.RecordedWithS12Routing, nil, nil)
		mm.ServeExactTest("T1", []*models.Mock{own})
	}
	return mm, stageSet
}

// pairedNames is what each read of the pairing gives of the HTTP recordings:
// PairingCandidates, the learn step's candidates under no keys, and the
// round's snapshot (the trees', then the names kept on disk).
func pairedNames(t *testing.T, mm *MockManager) map[string][]string {
	t.Helper()
	snap, ok := mm.RoundSnapshot(integrations.RoundQuery{Kind: models.HTTP, Pairing: true})
	if !ok {
		t.Fatal("no snapshot under exact serving")
	}
	round := exactNames(snap.Candidates)
	for _, d := range snap.OnDisk {
		round = append(round, d.Name)
	}
	return map[string][]string{
		"PairingCandidates":        exactNames(mm.PairingCandidates(models.HTTP)),
		"LearnElsewhereCandidates": exactNames(mm.LearnElsewhereCandidates(models.HTTP, nil)),
		"RoundSnapshot":            round,
	}
}

// The agent hands the next set's on-disk source before it stages that set
// (Agent.stageMocks). A pairing of record's full round between the two pairs
// with the set staged, its trees and its source, never with its trees and
// the next set's on-disk recordings, whatever read it pairs from: the
// staging installs the source with the trees it builds, and every pairing
// read takes both as one staging left them.
func TestAPairingBeforeTheNextSetIsStagedPairsWithTheSetStaged(t *testing.T) {
	mm, stageSet := pairingSet(t)
	stageSet("a")
	setA := []string{"a-tree", "a-disk"}
	for read, got := range pairedNames(t, mm) {
		if !reflect.DeepEqual(got, setA) {
			t.Fatalf("set a staged: %s paired with %v; want set a's %v", read, got, setA)
		}
	}
	mm.SetRecordedOfKind(setSource("b-disk"))
	for read, got := range pairedNames(t, mm) {
		if !reflect.DeepEqual(got, setA) {
			t.Fatalf("set b's source handed, set a still staged: %s paired with %v; want set a's alone, %v", read, got, setA)
		}
	}
	stageSet("b")
	setB := []string{"b-tree", "b-disk"}
	for read, got := range pairedNames(t, mm) {
		if !reflect.DeepEqual(got, setB) {
			t.Fatalf("set b staged: %s paired with %v; want set b's %v", read, got, setB)
		}
	}
}

// A pairing reads its view's staging epoch, the trees and the source the
// staging installed holding the staging lock, so no staging comes between
// them: a pairing that read set a's view pairs with set a's recordings, not
// with set b's, staged after its view was read. The view is read here while
// the agent stages set b, as far as the lock lets it.
func TestAPairingReadsItsViewAndTheSetStagedUnderOneStagingLock(t *testing.T) {
	t.Cleanup(models.RegisterRequestKeys(models.HTTP, func(*models.Mock) []string { return []string{"k"} }))
	mm, stageSet := pairingSet(t)
	stageSet("a")
	ofKind := func(in func() (*exactView, uint64)) []*models.Mock { return mm.pairableOfKind(in, models.HTTP) }
	underKeys := func(in func() (*exactView, uint64)) []*models.Mock {
		return mm.pairableUnderKeys(in, models.HTTP, []string{"k"})
	}
	for _, read := range []struct {
		name string
		in   func() (*exactView, uint64)
		pair func(func() (*exactView, uint64)) []*models.Mock
		want []string
	}{
		{"pairing", mm.pairing, ofKind, []string{"a-tree", "a-disk"}},
		{"learn step", mm.learningElsewhere, ofKind, []string{"a-tree", "a-disk"}},
		{"learn step under keys", mm.learningElsewhere, underKeys, []string{"a-tree", "a-keyed"}},
	} {
		var underLock bool
		got := exactNames(read.pair(func() (*exactView, uint64) {
			v, epoch := read.in()
			// The agent hands set b's sources and stages it now, unless
			// the staging lock is held: then it waits for the read.
			if mm.swapMu.TryLock() {
				mm.swapMu.Unlock()
				stageSet("b")
			} else {
				underLock = true
			}
			return v, epoch
		}))
		if !underLock || !reflect.DeepEqual(got, read.want) {
			t.Fatalf("%s: the view was read under the staging lock: %v; it paired with %v; want set a's %v", read.name, underLock, got, read.want)
		}
	}
}

// readsByKey is what each read by key of the set's recordings kept on disk
// gives of the HTTP recordings filed under the key k: the learn step's
// candidates (LearnElsewhereCandidates, the trees' then the disk's), the
// staged recordings (RangeStagedRecordings, sorted) and a miss's diagnosis
// (rangeRecorded, the disk's alone).
func readsByKey(t *testing.T, mm *MockManager) map[string][]string {
	t.Helper()
	var staged, diagnosed []string
	if err := mm.RangeStagedRecordings(models.HTTP, []string{"k"}, func(mk *models.Mock) bool {
		staged = append(staged, mk.Name)
		return true
	}); err != nil {
		t.Fatal(err)
	}
	sort.Strings(staged)
	mm.rangeRecorded(mm.stagedNow(), models.HTTP, []string{"k"}, func(name string, _ func() (*models.Mock, error)) bool {
		diagnosed = append(diagnosed, name)
		return true
	})
	return map[string][]string{
		"LearnElsewhereCandidates": exactNames(mm.LearnElsewhereCandidates(models.HTTP, []string{"k"})),
		"RangeStagedRecordings":    staged,
		"rangeRecorded":            diagnosed,
	}
}

// The agent hands the next set's keyed on-disk source before it stages that
// set (Agent.handUnstagedSources), as it hands the source of a kind. A read by
// key between the two, the learn step's pairing under request keys or a
// miss's, reads the set staged, its trees and its keyed source, never its
// trees with the next set's on-disk recordings: the staging installs that
// source with the trees it builds.
func TestAReadByKeyBeforeTheNextSetIsStagedReadsTheSetStaged(t *testing.T) {
	t.Cleanup(models.RegisterRequestKeys(models.HTTP, func(*models.Mock) []string { return []string{"k"} }))
	mm, stageSet := pairingSet(t)
	setOf := func(set string) map[string][]string {
		return map[string][]string{
			"LearnElsewhereCandidates": {set + "-tree", set + "-keyed"},
			"RangeStagedRecordings":    {set + "-keyed", set + "-own", set + "-tree"},
			"rangeRecorded":            {set + "-keyed"},
		}
	}
	check := func(when string, want map[string][]string) {
		t.Helper()
		for read, got := range readsByKey(t, mm) {
			if !reflect.DeepEqual(got, want[read]) {
				t.Fatalf("%s: %s read %v; want %v", when, read, got, want[read])
			}
		}
	}
	stageSet("a")
	check("set a staged", setOf("a"))
	mm.SetRecordingSource(keyedSetSource("b-keyed"))
	check("set b's keyed source handed, set a still staged", setOf("a"))
	stageSet("b")
	check("set b staged", setOf("b"))
}

// earlierSet is a manager and the staging of a set the agent keeps on disk,
// as the learn step's earlier places and keploy mock replay's teardown bound
// read it: hand hands set's reader of the recordings before an instant and
// the times of its recordings (Agent.handUnstagedSources), and stage stages
// its trees, set+"-tree", a pool recording at 1 s. Set a's test was recorded
// at 5 s (a-disk), set b's at 50 s (b-disk), on disk alone.
func earlierSet(t *testing.T) (mm *MockManager, hand, stage func(set string)) {
	t.Helper()
	t.Setenv(provenswitch.Env, "1")
	mm = NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(mm.Close)
	disk := map[string]*models.Mock{
		"a": exactMock("a-disk", 5*time.Second, models.LifetimePerTest),
		"b": exactMock("b-disk", 50*time.Second, models.LifetimePerTest),
	}
	hand = func(set string) {
		mk := disk[set]
		mm.SetRecordedBefore(func(before time.Time, visit func(*models.Mock) bool) error {
			if !mk.Spec.ReqTimestampMock.After(before) {
				visit(mk)
			}
			return nil
		})
		mm.SetRecordingTimes(func(visit func(string, time.Time, bool, string) bool) error {
			visit(mk.Name, models.PlaceOf(mk), false, "")
			return nil
		})
	}
	stage = func(set string) {
		mm.SetMocksWithWindow(nil, []*models.Mock{exactMock(set+"-tree", time.Second, models.LifetimeSession)}, models.BaseTime, time.Now())
	}
	mm.SetPoolNames([]string{"a-tree", "b-tree"})
	return mm, hand, stage
}

// earlierReads is what each read of earlierSet's recordings gives: the
// learn step's earlier places (RangeRecordedBefore, the trees' then the
// disk's), the set's staging read in recorded order (rangeRecordedInOrder),
// and whether a pool call at 10 s, after set a's test and before set b's,
// is the teardown (teardownAfter).
func earlierReads(t *testing.T, mm *MockManager) map[string]string {
	t.Helper()
	names := func(read func(time.Time, func(*models.Mock) bool) error) string {
		var out []string
		if err := read(lastRecorded, func(mk *models.Mock) bool {
			out = append(out, mk.Name)
			return true
		}); err != nil {
			t.Fatal(err)
		}
		return strings.Join(out, ",")
	}
	after := mm.teardownAfter("", "")
	return map[string]string{
		"RangeRecordedBefore":  names(mm.RangeRecordedBefore),
		"rangeRecordedInOrder": names(mm.rangeRecordedInOrder),
		"teardownAfter":        strconv.FormatBool(after != nil && after(exactT0.Add(10*time.Second))),
	}
}

// The agent hands the next set's reader of the recordings before an instant
// and the times of its recordings before it stages that set
// (Agent.handUnstagedSources). A read between the two, of the learn step's
// earlier places, of the set's staging read in recorded order or of keploy
// mock replay's teardown bound, reads the set staged, its trees and its
// on-disk recordings, never its trees with the next set's: the staging
// installs both with the trees it builds.
func TestAnEarlierPlacesOrTeardownReadBeforeTheNextSetIsStagedReadsTheSetStaged(t *testing.T) {
	mm, hand, stage := earlierSet(t)
	setOf := map[string]map[string]string{
		"a": {"RangeRecordedBefore": "a-tree,a-disk", "rangeRecordedInOrder": "a-tree,a-disk", "teardownAfter": "true"},
		"b": {"RangeRecordedBefore": "b-tree,b-disk", "rangeRecordedInOrder": "b-tree,b-disk", "teardownAfter": "false"},
	}
	check := func(when, set string) {
		t.Helper()
		got := earlierReads(t, mm)
		var wrong []string
		for _, read := range []string{"RangeRecordedBefore", "rangeRecordedInOrder", "teardownAfter"} {
			if want := setOf[set][read]; got[read] != want {
				wrong = append(wrong, fmt.Sprintf("%s read %s, want set %s's %s", read, got[read], set, want))
			}
		}
		if len(wrong) > 0 {
			t.Fatalf("%s: %s", when, strings.Join(wrong, "; "))
		}
	}
	hand("a")
	stage("a")
	check("set a staged", "a")
	hand("b")
	check("set b's sources handed, set a still staged", "a")
	stage("b")
	check("set b staged", "b")
}

// The learn step's earlier places read the staged trees and their source of
// one staging (stagedSet): the agent hands set b's sources and stages it
// after RangeRecordedBefore read the trees and before it reads the source,
// and the read must give set a's recordings, its trees and its disk.
func TestTheEarlierPlacesReadTheTreesAndTheirSourceOfOneStaging(t *testing.T) {
	mm, hand, stage := earlierSet(t)
	set := "a"
	for _, read := range []struct {
		name string
		read func(time.Time, func(*models.Mock) bool) error
	}{{"RangeRecordedBefore", mm.RangeRecordedBefore}, {"rangeRecordedInOrder", mm.rangeRecordedInOrder}} {
		hand(set)
		stage(set)
		next := map[string]string{"a": "b", "b": "a"}[set]
		var ran bool
		mm.exact.onBefore = func() {
			ran = true
			hand(next)
			stage(next)
		}
		var got []string
		if err := read.read(lastRecorded, func(mk *models.Mock) bool {
			got = append(got, mk.Name)
			return true
		}); err != nil {
			t.Fatal(err)
		}
		mm.exact.onBefore = nil
		if want := []string{set + "-tree", set + "-disk"}; !ran || !reflect.DeepEqual(got, want) {
			t.Fatalf("%s: set %s staged while it read (seam ran: %v); it read %v; want set %s's %v", read.name, next, ran, got, set, want)
		}
		set = next
	}
}

// raceSet is a manager and the staging of a set the agent keeps on disk, as
// keploy mock replay's teardown bound and the learn step's earlier places
// read it: hand hands set's reader of the recordings before an instant and
// the times of its recordings, and stage stages its trees. Set a's trees
// hold a pool recording at 1 s and a test's at 20 s, its disk a test's at
// 5 s; set b's trees hold a pool recording at 1 s, its disk a test's at
// 50 s. reads is what each read gives: whether a pool call at 10 s and one
// at 30 s are the teardown (teardownAfter) and the earlier places
// (RangeRecordedBefore, the trees' then the disk's). One set's trees with
// the other's times read neither set's teardown.
func raceSet(t *testing.T) (mm *MockManager, hand, stage func(set string), reads func() string) {
	t.Helper()
	t.Setenv(provenswitch.Env, "1")
	mm = NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(mm.Close)
	disk := map[string]*models.Mock{
		"a": exactMock("a-disk", 5*time.Second, models.LifetimePerTest),
		"b": exactMock("b-disk", 50*time.Second, models.LifetimePerTest),
	}
	trees := map[string][]*models.Mock{
		"a": {exactMock("a-test", 20*time.Second, models.LifetimePerTest)},
		"b": nil,
	}
	hand = func(set string) {
		mk := disk[set]
		mm.SetRecordedBefore(func(before time.Time, visit func(*models.Mock) bool) error {
			if !mk.Spec.ReqTimestampMock.After(before) {
				visit(mk)
			}
			return nil
		})
		mm.SetRecordingTimes(func(visit func(string, time.Time, bool, string) bool) error {
			visit(mk.Name, models.PlaceOf(mk), false, "")
			return nil
		})
		load := func() (*models.Mock, error) { return mk, nil }
		mm.SetRecordedOfKind(func(kind models.Kind, visit func(string, func() (*models.Mock, error)) bool) error {
			if kind == mk.Kind {
				visit(mk.Name, load)
			}
			return nil
		})
		mm.SetRecordingSource(func(kind models.Kind, _ []string, visit func(string, func() (*models.Mock, error)) bool) error {
			if kind == mk.Kind {
				visit(mk.Name, load)
			}
			return nil
		})
	}
	stage = func(set string) {
		mm.SetMocksWithWindow(trees[set], []*models.Mock{exactMock(set+"-pool", time.Second, models.LifetimeSession)}, models.BaseTime, time.Now())
	}
	mm.SetPoolNames([]string{"a-pool", "b-pool"})
	reads = func() string {
		after := mm.teardownAfter("", "")
		is := func(at time.Duration) bool { return after != nil && after(exactT0.Add(at)) }
		var names []string
		err := mm.RangeRecordedBefore(lastRecorded, func(mk *models.Mock) bool {
			names = append(names, mk.Name)
			return true
		})
		kind := disk["a"].Kind
		var ofKind []string
		whole, setErr := mm.RangeRecordedSet(kind, func(name string, _ func() (*models.Mock, error)) bool {
			ofKind = append(ofKind, name)
			return true
		})
		var byKey []string
		keyErr := mm.RangeStagedRecordings(kind, []string{"k"}, func(mk *models.Mock) bool {
			byKey = append(byKey, mk.Name)
			return true
		})
		sort.Strings(ofKind)
		sort.Strings(byKey)
		return fmt.Sprintf("teardown at 10s %v, at 30s %v; earlier places %s (err %v); set as recorded %s (whole %v, err %v); by key %s (err %v)",
			is(10*time.Second), is(30*time.Second), strings.Join(names, ","), err, strings.Join(ofKind, ","), whole, setErr, strings.Join(byKey, ","), keyErr)
	}
	return mm, hand, stage, reads
}

// A teardown read races the next set's staging: the agent hands set b's
// sources and stages it while another goroutine reads keploy mock replay's
// teardown bound and the earlier places, taking no staging lock. The
// staging publishes its trees and the sources handed before it as one set
// (stagedSet), so the read gives set a's whole until the staging published,
// and set b's whole once it did, though the trees and sources in place, and
// the staging generation, are already set b's or still set a's: never one
// set's trees with the other's times or disk.
func TestATeardownReadRacingTheNextSetsStagingReadsOneSet(t *testing.T) {
	mm, hand, stage, reads := raceSet(t)
	setOf := map[string]string{}
	for _, set := range []string{"a", "b"} {
		hand(set)
		stage(set)
		setOf[set] = reads()
	}
	if setOf["a"] == setOf["b"] {
		t.Fatalf("sets a and b read alike (%s): the row tells neither", setOf["a"])
	}
	hand("a")
	stage("a")
	// No read of set a before the race: its teardown bound is read anew there.
	got := map[bool]string{}
	gen := mm.StagingGeneration()
	genAt := map[bool]uint64{}
	mm.exact.onPublish = func(published bool) {
		genAt[published] = mm.StagingGeneration()
		// The read runs on its own goroutine, as a parser's would; one
		// that waited for the staging would never answer in time.
		done := make(chan string, 1)
		go func() { done <- reads() }()
		select {
		case got[published] = <-done:
		case <-time.After(5 * time.Second):
			got[published] = "no answer: the read waited for the staging"
		}
	}
	hand("b")
	stage("b")
	mm.exact.onPublish = nil
	// The generation moves once the set is published, never before: a
	// parser that read the new generation reads the new set.
	if genAt[false] != gen || genAt[true] != gen || mm.StagingGeneration() == gen {
		t.Errorf("the staging generation was %d before the staging, %d and %d as it published its set, %d after: it must move only after the set is published", gen, genAt[false], genAt[true], mm.StagingGeneration())
	}
	for _, c := range []struct {
		published bool
		set       string
	}{{false, "a"}, {true, "b"}} {
		if got[c.published] != setOf[c.set] {
			t.Errorf("a read as set b's staging had published its set: %v: %s; want set %s's: %s", c.published, got[c.published], c.set, setOf[c.set])
		}
	}
}
