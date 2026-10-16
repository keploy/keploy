package agent

import (
	"context"
	"reflect"
	"sort"
	"testing"
	"time"

	proxyPkg "go.keploy.io/server/v3/pkg/agent/proxy"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// stagingStore is a recording whose per-test mocks sit in the agent's on-disk
// store: two from the app's startup, before the set's first test at stageT0,
// the two tests' own, and a poll recorded an hour into the recording.
func stagingStore(t *testing.T) (*Agent, *recordingProxy, *observer.ObservedLogs) {
	t.Helper()
	t.Setenv("KEPLOY_STRICT_MOCK_WINDOW", "1")
	p := &recordingProxy{}
	a := diskAgent(t, p,
		stageMock("boot-config", "/config", stageT0.Add(-20*time.Second)),
		stageMock("boot-auth", "/auth", stageT0.Add(-5*time.Second)),
		stageMock("test-a", "/q", stageT0.Add(time.Second)),
		stageMock("test-b", "/q", stageT0.Add(time.Minute)),
		stageMock("poll-later", "/poll", stageT0.Add(time.Hour)))
	core, logs := observer.New(zap.DebugLevel)
	a.logger = zap.New(core)
	return a, p, logs
}

func sortedNames(names []string) []string {
	out := append([]string(nil), names...)
	sort.Strings(out)
	return out
}

func loadedFromDisk(t *testing.T, logs *observer.ObservedLogs) []int64 {
	t.Helper()
	var out []int64
	for _, e := range logs.FilterMessage("agent mock residency: loaded per-test set from disk").All() {
		n, _ := e.ContextMap()["loadedFromDisk"].(int64)
		out = append(out, n)
	}
	return out
}

// The set's staging call, sent before the app starts with the window BaseTime to
// now, reads from the on-disk store only the mocks recorded before the set's
// first test. The proxy files whatever it is staged with as app-startup traffic,
// so reading the whole window brought every mock of the recording into the
// agent's memory: 81,738 of them OOM-killed a 1200Mi agent on a prod recording.
func TestStagingReadsOnlyTheStartupMocksFromDisk(t *testing.T) {
	a, p, logs := stagingStore(t)
	err := a.UpdateMockParams(context.Background(), models.MockFilterParams{
		AfterTime:              models.BaseTime,
		BeforeTime:             stageT0.Add(2 * time.Hour),
		FirstRecordedTestStart: stageT0,
		StrictMockWindow:       true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := loadedFromDisk(t, logs); !reflect.DeepEqual(got, []int64{2}) {
		t.Fatalf("the staging call read %v mocks from disk; want the 2 recorded before the first test", got)
	}
	if len(p.windowed) != 1 {
		t.Fatalf("staged %d times, want once", len(p.windowed))
	}
	if got, want := sortedNames(p.windowed[0][0]), []string{"boot-auth", "boot-config"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("the staging call staged %v; want the app-startup mocks %v", got, want)
	}
}

// A test's own call is unchanged: its window plus what was recorded before the
// set's first test.
func TestATestStillReadsItsWindowAndTheStartupMocks(t *testing.T) {
	a, p, logs := stagingStore(t)
	err := a.UpdateMockParams(context.Background(), models.MockFilterParams{
		AfterTime:        stageT0,
		BeforeTime:       stageT0.Add(2 * time.Second),
		StrictMockWindow: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := loadedFromDisk(t, logs); !reflect.DeepEqual(got, []int64{3}) {
		t.Fatalf("the test's call read %v mocks from disk; want its own and the 2 startup mocks", got)
	}
	if got, want := sortedNames(p.windowed[0][0]), []string{"boot-auth", "boot-config", "test-a"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("the test's call staged %v; want %v", got, want)
	}
}

// A set whose tests carry no request time has no first test to cut at, so its
// staging call reads the window it names.
func TestStagingWithoutARecordedTestTimeReadsItsWindow(t *testing.T) {
	a, p, logs := stagingStore(t)
	err := a.UpdateMockParams(context.Background(), models.MockFilterParams{
		AfterTime:        models.BaseTime,
		BeforeTime:       stageT0.Add(2 * time.Hour),
		StrictMockWindow: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := loadedFromDisk(t, logs); !reflect.DeepEqual(got, []int64{5}) {
		t.Fatalf("the staging call read %v mocks from disk; want all 5 in its window", got)
	}
	if len(p.windowed[0][0]) != 5 {
		t.Fatalf("the staging call staged %v; want all 5", p.windowed[0][0])
	}
}

// pgQueryMock is a per-test PostgreSQL query mock of the statement sqlHash,
// recorded at at.
func pgQueryMock(name, sqlHash string, at time.Time) *models.Mock {
	return &models.Mock{
		Version: "api.keploy.io/v1beta1",
		Name:    name,
		Kind:    models.PostgresV3,
		Spec: models.MockSpec{
			PostgresV3: &models.PostgresV3Spec{
				Type:  models.PostgresV3TypeQuery,
				Query: &models.PostgresV3QuerySpec{SQLAstHash: sqlHash, InvocationID: name},
			},
			ReqTimestampMock: at,
			ResTimestampMock: at.Add(time.Millisecond),
		},
		TestModeInfo: models.TestModeInfo{Lifetime: models.LifetimePerTest, LifetimeDerived: true},
	}
}

// catalogProxy is a recordingProxy that takes the statement catalog.
type catalogProxy struct {
	*recordingProxy
	catalogs [][]string
}

func (p *catalogProxy) SeedStatementCatalog(mocks []*models.Mock) {
	p.catalogs = append(p.catalogs, namesOf(mocks))
}

// Storing a set hands the proxy its statement catalog, once: the first
// recording of each statement on disk, those recorded inside tests included,
// which the staging's startup band does not reach. An app that prepares at
// start a statement it runs only inside a test is described from it. The
// staging, a test's own call and a scope staging (its begin and end, each a
// call from BaseTime) leave it as stored, and the next set's store replaces it,
// with none when that store keeps nothing on disk.
func TestStoringASetSeedsItsStatementCatalogOnce(t *testing.T) {
	t.Setenv("KEPLOY_STRICT_MOCK_WINDOW", "1")
	p := &catalogProxy{recordingProxy: &recordingProxy{}}
	a := strictAgent(true)
	a.Proxy = p
	filtered := []*models.Mock{
		pgQueryMock("boot-select", "sha256:boot", stageT0.Add(-5*time.Second)),
		pgQueryMock("lists-in-a", "sha256:lists", stageT0.Add(time.Second)),
		pgQueryMock("lists-in-b", "sha256:lists", stageT0.Add(time.Minute)),
		pgQueryMock("insert-in-b", "sha256:insert", stageT0.Add(time.Minute+time.Second)),
		stageMock("http-in-a", "/q", stageT0.Add(2*time.Second)),
	}
	header, dec := readHeaderAndDecoder(t, encodeStreamBody(t, filtered, nil))
	if err := a.StoreMocksStream(context.Background(), header, dec); err != nil {
		t.Fatal(err)
	}
	if loadStorage(t, a).diskMocks == nil {
		t.Fatal("the set's per-test mocks are not on disk")
	}
	if len(p.catalogs) != 1 {
		t.Fatalf("storing the set seeded the catalog %d times, want once", len(p.catalogs))
	}
	if got, want := p.catalogs[0], []string{"boot-select", "lists-in-a", "insert-in-b"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("the catalog is %v; want the first recording of each statement, in recorded order: %v", got, want)
	}

	if err := a.UpdateMockParams(context.Background(), models.MockFilterParams{
		AfterTime:              models.BaseTime,
		BeforeTime:             stageT0.Add(2 * time.Hour),
		FirstRecordedTestStart: stageT0,
		StrictMockWindow:       true,
	}); err != nil {
		t.Fatal(err)
	}
	if got, want := sortedNames(p.windowed[0][0]), []string{"boot-select"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("the staging call staged %v; want only the startup mocks %v (the catalog serves nothing)", got, want)
	}
	if err := a.UpdateMockParams(context.Background(), models.MockFilterParams{
		AfterTime: stageT0, BeforeTime: stageT0.Add(2 * time.Second), StrictMockWindow: true,
	}); err != nil {
		t.Fatal(err)
	}
	if err := a.stageScope(context.Background(), []string{"lists-in-a"}, true); err != nil {
		t.Fatal(err)
	}
	if err := a.stageScope(context.Background(), nil, false); err != nil {
		t.Fatal(err)
	}
	if len(p.catalogs) != 1 {
		t.Fatalf("a call after the store seeded the catalog again (%d seeds, want 1)", len(p.catalogs))
	}

	// The next set keeps nothing on disk: its store replaces the catalog with
	// none, so no prepare is described from the previous set's recordings.
	if err := a.StoreMocks(context.Background(), []*models.Mock{pgQueryMock("next-set", "sha256:next", stageT0)}, nil); err != nil {
		t.Fatal(err)
	}
	if len(p.catalogs) != 2 || len(p.catalogs[1]) != 0 {
		t.Fatalf("the next set's store left the catalog %v; want it replaced with none", p.catalogs)
	}
}

// A set's staging call also stages, beside the startup band, the per-test
// mocks a parser declares it needs staged whatever their recorded time
// (models.StagingWithStartup): here the pushes of a broker kind, one
// recorded before the first test, one at its start, one inside it and one in
// the gap after it. It reads only those of its window, never the rest of the
// recording nor a push past the window's end, and a test's own call is
// unchanged. The mock manager files them as startup traffic, and the first
// test refiles that tier to what was recorded before it.
func TestStagingAlsoStagesTheMocksAParserDeclares(t *testing.T) {
	isPush := func(m *models.Mock) bool { return m.Spec.Metadata["commandType"] == "MESSAGE" }
	defer models.RegisterStagingNeed(agentPulsar, models.StagingNeed{Use: models.StagingWithStartup, Match: isPush})()
	t.Setenv("KEPLOY_STRICT_MOCK_WINDOW", "1")
	mm := proxyPkg.NewMockManager(nil, nil, zap.NewNop())
	defer mm.Close()
	p := &mgrProxy{mm: mm}
	a := diskAgent(t, p,
		stageMock("boot-config", "/config", stageT0.Add(-20*time.Second)),
		agentBroker("push-boot", agentPulsar, "MESSAGE", stageT0.Add(-time.Second)),
		agentBroker("push-at-cut", agentPulsar, "MESSAGE", stageT0),
		stageMock("test-a", "/q", stageT0.Add(time.Second)),
		agentBroker("push-in-a", agentPulsar, "MESSAGE", stageT0.Add(1500*time.Millisecond)),
		agentBroker("send-in-a", agentPulsar, "SEND", stageT0.Add(1600*time.Millisecond)),
		agentBroker("push-gap", agentPulsar, "MESSAGE", stageT0.Add(30*time.Second)),
		stageMock("test-b", "/q", stageT0.Add(time.Minute)),
		agentBroker("push-past-window", agentPulsar, "MESSAGE", stageT0.Add(3*time.Hour)))
	core, logs := observer.New(zap.DebugLevel)
	a.logger = zap.New(core)
	startupNames := func() []string {
		t.Helper()
		startup, err := mm.GetStartupMocks()
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, m := range startup {
			names = append(names, m.Name)
		}
		return sortedNames(names)
	}

	err := a.UpdateMockParams(context.Background(), models.MockFilterParams{
		AfterTime:              models.BaseTime,
		BeforeTime:             stageT0.Add(2 * time.Hour),
		FirstRecordedTestStart: stageT0,
		StrictMockWindow:       true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := loadedFromDisk(t, logs); !reflect.DeepEqual(got, []int64{5}) {
		t.Fatalf("the staging call read %v mocks from disk; want the 2 startup mocks and the 3 later pushes of its window", got)
	}
	staged := logs.FilterMessage("staged the set's app-startup mocks from the on-disk store; each test reads its own when it runs").All()
	if len(staged) != 1 || staged[0].ContextMap()["startupMocks"] != int64(2) || staged[0].ContextMap()["declaredMocks"] != int64(3) {
		t.Fatalf("staging logged %v; want 2 startup mocks and 3 declared ones", staged)
	}
	if got, want := startupNames(), []string{"boot-config", "push-at-cut", "push-boot", "push-gap", "push-in-a"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("the staging call filed %v as startup traffic; want %v", got, want)
	}

	// The first test's call is unchanged: its window and the startup band.
	// The first test refiles the startup tier to what was recorded before it.
	if err := a.UpdateMockParams(context.Background(), models.MockFilterParams{
		AfterTime:        stageT0,
		BeforeTime:       stageT0.Add(2 * time.Second),
		StrictMockWindow: true,
	}); err != nil {
		t.Fatal(err)
	}
	if got := loadedFromDisk(t, logs); !reflect.DeepEqual(got, []int64{5, 6}) {
		t.Fatalf("the test's call read %v mocks from disk; want its 4 and the 2 startup mocks", got)
	}
	if got, want := startupNames(), []string{"boot-config", "push-boot"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("after the first test the startup tier is %v; want what was recorded before it, %v", got, want)
	}
}

// With no kind declaring a need, the staging call reads the startup band
// only, as before.
func TestStagingWithNoDeclaredNeedReadsOnlyTheStartupBand(t *testing.T) {
	t.Setenv("KEPLOY_STRICT_MOCK_WINDOW", "1")
	p := &recordingProxy{}
	a := diskAgent(t, p,
		agentBroker("push-boot", agentPulsar, "MESSAGE", stageT0.Add(-time.Second)),
		agentBroker("push-gap", agentPulsar, "MESSAGE", stageT0.Add(30*time.Second)))
	if err := a.UpdateMockParams(context.Background(), models.MockFilterParams{
		AfterTime:              models.BaseTime,
		BeforeTime:             stageT0.Add(time.Hour),
		FirstRecordedTestStart: stageT0,
		StrictMockWindow:       true,
	}); err != nil {
		t.Fatal(err)
	}
	if got := sortedNames(p.windowed[0][0]); !reflect.DeepEqual(got, []string{"push-boot"}) {
		t.Fatalf("staged %v; want only the startup band", got)
	}
}
