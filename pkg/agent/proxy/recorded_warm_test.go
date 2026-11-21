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

// warmSet is a set of recordings of two kinds, in recorded order, and the
// source that walks them in that order, closing walked once it returns and
// counting the recordings it handed out.
func warmSet(t *testing.T) (all []*models.Mock, src func(time.Time, func(*models.Mock) bool) error, walked chan struct{}, handed *int) {
	t.Helper()
	base := time.Unix(1_700_000_000, 0)
	kinds := []models.Kind{models.Mongo, models.HTTP, models.Mongo, models.Mongo, models.HTTP, models.Mongo}
	for i, k := range kinds {
		m := &models.Mock{Name: "mock-" + string(rune('a'+i)), Kind: k}
		m.Spec.ReqTimestampMock = base.Add(time.Duration(i) * time.Second)
		all = append(all, m)
	}
	walked, n := make(chan struct{}), 0
	src = func(_ time.Time, visit func(*models.Mock) bool) error {
		defer close(walked)
		for _, m := range all {
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

func proxyWithSet(src func(time.Time, func(*models.Mock) bool) error) *Proxy {
	mm := NewMockManager(nil, nil, zap.NewNop())
	mm.SetRecordedBefore(src)
	return &Proxy{logger: zap.NewNop(), mockManager: mm}
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
	p := proxyWithSet(src)
	if err := p.WarmRecordedSet(); err != nil {
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

// A warmer with room is handed every recording of its kind, to the set's end.
func TestAWarmerWithRoomIsHandedEveryRecordingOfItsKind(t *testing.T) {
	provenswitch.Pin(true)
	t.Cleanup(provenswitch.Unpin)
	mongo := warmer(t, models.Mongo, 100)
	_, src, walked, handed := warmSet(t)
	if err := proxyWithSet(src).WarmRecordedSet(); err != nil {
		t.Fatal(err)
	}
	<-walked
	if want := []string{"mock-a", "mock-c", "mock-d", "mock-f"}; !warmedNames(*mongo, want) {
		t.Fatalf("mongo warmer handed %v, want %v", *mongo, want)
	}
	if *handed != 6 {
		t.Fatalf("the walk handed out %d recordings, want all 6", *handed)
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
	if err := proxyWithSet(src).WarmRecordedSet(); err != nil {
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
	if err := proxyWithSet(src).WarmRecordedSet(); err != nil {
		t.Fatal(err)
	}
	if *handed != 0 || len(*mongo) != 0 {
		t.Fatalf("read %d recordings, warmed %v: want none", *handed, *mongo)
	}
	if err := (&Proxy{logger: zap.NewNop()}).WarmRecordedSet(); !errors.Is(err, errNoMockManager) {
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
	p := proxyWithSet(src)
	p.mockManager.exact.pairs.Store(true)
	if err := p.WarmRecordedSet(); err != nil {
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
