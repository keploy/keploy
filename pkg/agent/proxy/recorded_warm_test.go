package proxy

import (
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// warmSourceFunc is the on-disk source WarmRecordedSet reads the kinds named
// through (DiskMocks.RangeRecordedOfKindsBefore).
type warmSourceFunc = func(time.Time, []models.Kind, func(*models.Mock) bool) error

// warmSet is a set of recordings of two kinds, in recorded order, and the
// source that walks those of the kinds named in that order, closing walked
// once it returns and counting the recordings it handed out.
func warmSet(t *testing.T) (all []*models.Mock, src warmSourceFunc, walked chan struct{}, handed *int) {
	t.Helper()
	base := time.Unix(1_700_000_000, 0)
	kinds := []models.Kind{models.Mongo, models.HTTP, models.Mongo, models.Mongo, models.HTTP, models.Mongo}
	for i, k := range kinds {
		m := &models.Mock{Name: "mock-" + string(rune('a'+i)), Kind: k}
		m.Spec.ReqTimestampMock = base.Add(time.Duration(i) * time.Second)
		all = append(all, m)
	}
	walked, n := make(chan struct{}), 0
	src = func(_ time.Time, ks []models.Kind, visit func(*models.Mock) bool) error {
		defer close(walked)
		for _, m := range all {
			if !slices.Contains(ks, m.Kind) {
				continue
			}
			n++
			if !visit(m) {
				return nil
			}
		}
		return nil
	}
	return all, src, walked, &n
}

// warmer registers a warmer of kind that keeps up to room recordings and
// says it is full at the next one, and returns the names it was handed.
func warmer(t *testing.T, kind models.Kind, room int) *[]string {
	t.Helper()
	var got []string
	integrations.RegisterRecordingWarmer(kind, func() func(*models.Mock) bool {
		n := 0
		return func(m *models.Mock) bool {
			got = append(got, m.Name)
			n++
			return n <= room
		}
	})
	t.Cleanup(func() { integrations.RegisterRecordingWarmer(kind, nil) })
	return &got
}

func proxyWithSet() *Proxy {
	return &Proxy{logger: zap.NewNop(), mockManager: NewMockManager(nil, nil, zap.NewNop())}
}

// In replay, the staged set's recordings are handed to each kind's warmer in
// recorded order; a warmer that says it is full is handed no more, and the
// walk stops once every warmer is full, so a set larger than the caches is
// not walked to its end for nothing.
func TestTheStagedSetIsHandedToEachKindsWarmerInRecordedOrderUntilAllAreFull(t *testing.T) {
	provenswitch.Pin(true)
	t.Cleanup(provenswitch.Unpin)
	mongo, http := warmer(t, models.Mongo, 1), warmer(t, models.HTTP, 1)
	_, src, walked, handed := warmSet(t)
	p := proxyWithSet()
	if err := p.WarmRecordedSet(src); err != nil {
		t.Fatal(err)
	}
	<-walked
	if want := []string{"mock-a", "mock-c"}; !warmedNames(*mongo, want) {
		t.Fatalf("mongo warmer handed %v, want %v (in order, none after it said full)", *mongo, want)
	}
	if want := []string{"mock-b", "mock-e"}; !warmedNames(*http, want) {
		t.Fatalf("http warmer handed %v, want %v", *http, want)
	}
	if *handed != 5 {
		t.Fatalf("the walk handed out %d recordings, want 5: it stops once every warmer is full", *handed)
	}
}

// A warmer with room is handed every recording of its kind, to the set's
// end, and the disk is asked for its kind alone.
func TestAWarmerWithRoomIsHandedEveryRecordingOfItsKind(t *testing.T) {
	provenswitch.Pin(true)
	t.Cleanup(provenswitch.Unpin)
	mongo := warmer(t, models.Mongo, 100)
	_, src, walked, handed := warmSet(t)
	if err := proxyWithSet().WarmRecordedSet(src); err != nil {
		t.Fatal(err)
	}
	<-walked
	if want := []string{"mock-a", "mock-c", "mock-d", "mock-f"}; !warmedNames(*mongo, want) {
		t.Fatalf("mongo warmer handed %v, want %v", *mongo, want)
	}
	if *handed != 4 {
		t.Fatalf("the walk read %d recordings, want the 4 Mongo ones: no warmer reads HTTP", *handed)
	}
}

// With the switch off the parsers' caches take nothing, so nothing is read;
// with no warmer registered nothing is read either; with no mock manager there
// is no set.
func TestNothingIsWarmedWithTheSwitchOffOrNoWarmer(t *testing.T) {
	_, src, _, handed := warmSet(t)
	provenswitch.Pin(false)
	t.Cleanup(provenswitch.Unpin)
	mongo := warmer(t, models.Mongo, 100)
	if err := proxyWithSet().WarmRecordedSet(src); err != nil {
		t.Fatal(err)
	}
	if w := (&Proxy{}).newSetWarm(); w != nil {
		t.Fatal("a warm was made with the switch off")
	}
	provenswitch.Pin(true)
	integrations.RegisterRecordingWarmer(models.Mongo, nil)
	if w := (&Proxy{}).newSetWarm(); w != nil {
		t.Fatal("a warm was made with no warmer registered")
	}
	integrations.RegisterRecordingWarmer(models.Mongo, func() func(*models.Mock) bool { return nil })
	if w := (&Proxy{}).newSetWarm(); w != nil {
		t.Fatal("a warm was made with every warmer reading nothing of the set")
	}
	integrations.RegisterRecordingWarmer(models.Mongo, nil)
	if err := proxyWithSet().WarmRecordedSet(src); err != nil {
		t.Fatal(err)
	}
	if *handed != 0 || len(*mongo) != 0 {
		t.Fatalf("read %d recordings, warmed %v: want none", *handed, *mongo)
	}
	if err := (&Proxy{logger: zap.NewNop()}).WarmRecordedSet(src); !errors.Is(err, errNoMockManager) {
		t.Fatalf("no mock manager: got %v, want errNoMockManager", err)
	}
}

// In record's full round the learn step's staging walks the set: the warm
// reads nothing of its own there, and on the staging's walk (wrap) the
// staging's visit is handed every recording, the warm ended or not.
func TestInRecordsFullRoundTheWarmRidesTheLearnStagingsWalk(t *testing.T) {
	provenswitch.Pin(true)
	t.Cleanup(provenswitch.Unpin)
	mongo := warmer(t, models.Mongo, 0)
	all, src, _, handed := warmSet(t)
	p := proxyWithSet()
	p.mockManager.exact.pairs.Store(true)
	if err := p.WarmRecordedSet(src); err != nil {
		t.Fatal(err)
	}
	if *handed != 0 {
		t.Fatalf("the warm walked %d recordings of its own in record's full round, want none", *handed)
	}
	w := p.newSetWarm()
	var visited []string
	visit := w.wrap(func(m *models.Mock) bool { visited = append(visited, m.Name); return true })
	for _, m := range all {
		visit(m)
	}
	if len(visited) != len(all) {
		t.Fatalf("the staging's visit saw %v, want all %d", visited, len(all))
	}
	if want := []string{"mock-a"}; !warmedNames(*mongo, want) {
		t.Fatalf("mongo warmer handed %v, want %v (full at its first)", *mongo, want)
	}
}

// A set staged after a warm began stops it: its recordings are another set's.
func TestALaterSetStopsTheWarmOfTheLastOne(t *testing.T) {
	provenswitch.Pin(true)
	t.Cleanup(provenswitch.Unpin)
	warmer(t, models.Mongo, 100)
	p := &Proxy{}
	first := p.newSetWarm()
	m := &models.Mock{Kind: models.Mongo}
	if !first.warm(m) {
		t.Fatal("the first warm stopped before a later set was staged")
	}
	if !first.warm(nil) || first.warmed != 1 {
		t.Fatal("a nil recording ended the warm or was counted warmed")
	}
	second := p.newSetWarm()
	if first.warm(m) || !first.stopped {
		t.Fatal("the first warm went on after a later set was staged")
	}
	if !second.warm(m) || second.warmed != 1 {
		t.Fatal("the later set's warm did not read")
	}
}

func warmedNames(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// shuffledSecs are the request times, in seconds, of the recordings the
// store rows add, in the order they add them: not their recorded order, so
// a read that does not sort them is told apart.
var shuffledSecs = []int64{103, 100, 106, 101, 107, 102, 105, 104}

// warmStore is an on-disk store holding one recording "rec-i" of kinds[i],
// requested at shuffledSecs[i], every record on its file, and the source
// WarmRecordedSet reads it through, which closes walked once the warm's read
// of it returns.
func warmStore(t *testing.T, kinds ...models.Kind) (*DiskMocks, warmSourceFunc, chan struct{}) {
	t.Helper()
	d, err := newDiskMocks(zap.NewNop(), 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	for i, k := range kinds {
		m := perTestMockAt(fmt.Sprintf("rec-%d", i), shuffledSecs[i])
		m.Kind = k
		if k == models.HTTP {
			m.Spec.HTTPReq = &models.HTTPReq{Method: "GET", URL: fmt.Sprintf("http://svc/%d", i)}
		}
		if err := d.Add(m); err != nil {
			t.Fatal(err)
		}
	}
	d.Finalize()
	walked := make(chan struct{})
	src := func(at time.Time, ks []models.Kind, visit func(*models.Mock) bool) error {
		defer close(walked)
		return d.RangeRecordedOfKindsBefore(at, ks, visit)
	}
	return d, src, walked
}

// visitedNames are the names d's read of kinds visits, recorded before at.
func visitedNames(t *testing.T, d *DiskMocks, at time.Time, kinds ...models.Kind) []string {
	t.Helper()
	var got []string
	if err := d.RangeRecordedOfKindsBefore(at, kinds, func(m *models.Mock) bool {
		got = append(got, m.Name)
		return true
	}); err != nil {
		t.Fatal(err)
	}
	return got
}

// warmNamesOf are the names of the recordings of ms of kind, in their order.
func warmNamesOf(ms []*models.Mock, kind models.Kind) []string {
	var out []string
	for _, m := range ms {
		if m.Kind == kind {
			out = append(out, m.Name)
		}
	}
	return out
}

// An HTTP-only set, with a Mongo warmer registered: the warm reads none of
// its recordings from the disk, where a walk of every kind decodes each one
// (no warmer reads HTTP, so none is handed on either).
func TestAnHTTPOnlySetsWarmReadsNoRecordingFromDisk(t *testing.T) {
	provenswitch.Pin(true)
	t.Cleanup(provenswitch.Unpin)
	mongo := warmer(t, models.Mongo, 100)
	kinds := slices.Repeat([]models.Kind{models.HTTP}, len(shuffledSecs))
	d, src, walked := warmStore(t, kinds...)
	if err := proxyWithSet().WarmRecordedSet(src); err != nil {
		t.Fatal(err)
	}
	<-walked
	if n := d.RecordedReads(); n != 0 || len(*mongo) != 0 {
		t.Fatalf("the warm decoded %d recordings from disk and handed on %v: want none of an HTTP-only set", n, *mongo)
	}
	if n := len(recordedVisits(t, d)); n != len(kinds) || d.RecordedReads() != int64(len(kinds)) {
		t.Fatalf("the walk of every kind visited %d and decoded %d: want all %d (the row reads decodes)", n, d.RecordedReads(), len(kinds))
	}
}

// A set of both kinds: the warm decodes from the disk only its Mongo
// recordings, and hands them on in the order the walk of every kind
// (RangeRecordedBefore) visits them, which is not the order they were added.
func TestAMixedSetsWarmReadsOnlyTheWarmedKindFromDiskInRecordedOrder(t *testing.T) {
	provenswitch.Pin(true)
	t.Cleanup(provenswitch.Unpin)
	mongo := warmer(t, models.Mongo, 100)
	kinds := []models.Kind{models.HTTP, models.Mongo, models.HTTP, models.Mongo, models.Mongo, models.HTTP, models.Mongo, models.HTTP}
	d, src, walked := warmStore(t, kinds...)
	if err := proxyWithSet().WarmRecordedSet(src); err != nil {
		t.Fatal(err)
	}
	<-walked
	if n := d.RecordedReads(); n != 4 {
		t.Fatalf("the warm decoded %d recordings from disk, want its 4 Mongo ones", n)
	}
	if want := warmNamesOf(recordedVisits(t, d), models.Mongo); !warmedNames(*mongo, want) {
		t.Fatalf("mongo warmer handed %v, want %v (recorded order)", *mongo, want)
	}
}

// Under the proven switch the read of the kinds named visits what the walk
// of every kind visits of them, in its order, at each cut; a kind named
// twice is read once; and a recording added again as another kind is that
// kind's alone (its last record, as RangeKind reads it).
func TestTheReadOfKindsNamedIsTheWalkOfEveryKindOfThose(t *testing.T) {
	provenswitch.Pin(true)
	t.Cleanup(provenswitch.Unpin)
	kinds := []models.Kind{models.Mongo, models.HTTP, models.Mongo, models.HTTP, models.Mongo, models.Mongo, models.HTTP, models.HTTP}
	d, _, _ := warmStore(t, kinds...)
	for _, at := range []time.Time{time.Unix(1000, 0), time.Unix(104, 0), time.Unix(99, 0)} {
		var all []*models.Mock
		if err := d.RangeRecordedBefore(at, func(m *models.Mock) bool { all = append(all, m); return true }); err != nil {
			t.Fatal(err)
		}
		for _, k := range []models.Kind{models.Mongo, models.HTTP} {
			if got, want := visitedNames(t, d, at, k, k), warmNamesOf(all, k); !warmedNames(got, want) {
				t.Fatalf("read of %s before %v visited %v, want %v", k, at.Unix(), got, want)
			}
		}
	}
	again := perTestMockAt("rec-0", 108)
	again.Spec.HTTPReq = &models.HTTPReq{Method: "GET", URL: "http://svc/again"}
	if err := d.Add(again); err != nil {
		t.Fatal(err)
	}
	if got := visitedNames(t, d, time.Unix(1000, 0), models.Mongo); slices.Contains(got, "rec-0") {
		t.Fatalf("read of Mongo visited %v: rec-0 was added again as HTTP", got)
	}
	if got := visitedNames(t, d, time.Unix(1000, 0), models.HTTP); !slices.Contains(got, "rec-0") {
		t.Fatalf("read of HTTP visited %v: want rec-0, added again as HTTP", got)
	}
}

// A set kept all in the trees has no on-disk source (nil): the warm hands
// the trees' recordings of its kind on, in recorded order, all the same.
func TestAResidentSetsWarmIsHandedItsTreesRecordings(t *testing.T) {
	provenswitch.Pin(true)
	t.Cleanup(provenswitch.Unpin)
	var got []string
	done := make(chan struct{})
	integrations.RegisterRecordingWarmer(models.Mongo, func() func(*models.Mock) bool {
		return func(m *models.Mock) bool {
			got = append(got, m.Name)
			if len(got) == 2 {
				close(done)
			}
			return true
		}
	})
	t.Cleanup(func() { integrations.RegisterRecordingWarmer(models.Mongo, nil) })
	var ms []*models.Mock
	for i, k := range []models.Kind{models.Mongo, models.HTTP, models.Mongo} {
		m := perTestMockAt(fmt.Sprintf("tree-%d", i), shuffledSecs[i])
		m.Kind = k
		m.TestModeInfo.Lifetime = models.LifetimeSession
		ms = append(ms, m)
	}
	p := proxyWithSet()
	p.mockManager.SetUnFilteredMocks(ms)
	if err := p.WarmRecordedSet(nil); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("the warm of a resident set was not handed its 2 Mongo recordings")
	}
	if want := []string{"tree-0", "tree-2"}; !warmedNames(got, want) {
		t.Fatalf("mongo warmer handed %v, want %v (recorded order)", got, want)
	}
}

// A closed store's read of the kinds named says so, as RangeRecordedBefore
// does, even where it holds none of those kinds.
func TestAClosedStoresReadOfKindsSaysItIsClosed(t *testing.T) {
	provenswitch.Pin(true)
	t.Cleanup(provenswitch.Unpin)
	d, _, _ := warmStore(t, models.HTTP, models.HTTP)
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	visited := 0
	err := d.RangeRecordedOfKindsBefore(time.Unix(1000, 0), []models.Kind{models.Mongo}, func(*models.Mock) bool { visited++; return true })
	if err == nil || visited != 0 {
		t.Fatalf("a closed store's read of Mongo: err %v, %d visited; want its closed error and none", err, visited)
	}
}

// With a kind the store does not list (the proven switch off, a kind not
// placed at its answer), the read of the kinds named still visits each of
// their recordings, in recorded order, and none of another kind.
func TestTheReadOfKindsNamedReadsAKindTheStoreDoesNotList(t *testing.T) {
	provenswitch.Pin(false)
	t.Cleanup(provenswitch.Unpin)
	plain := models.Kind("WarmRowsUnlisted")
	if diskListsKind(plain) {
		t.Fatalf("%s is listed with the switch off: the row needs a kind that is not", plain)
	}
	kinds := []models.Kind{plain, models.HTTP, plain, plain, models.HTTP, plain, models.HTTP, plain}
	d, _, _ := warmStore(t, kinds...)
	want := warmNamesOf(recordedVisits(t, d), plain)
	if got := visitedNames(t, d, time.Unix(1000, 0), plain); len(want) != 5 || !warmedNames(got, want) {
		t.Fatalf("read of an unlisted kind visited %v, want %v", got, want)
	}
}
