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

// A set begins, before the app starts, with only the mocks recorded before the
// set's first test read from the on-disk store: the app's startup traffic. A
// set used to be staged with the window 2000-01-01 to now, and reading that
// window brought every mock of the recording into the agent's memory only to
// file it as startup traffic: 81,738 of them OOM-killed a 1200Mi agent on a
// prod recording.
func TestBeginTestSetReadsOnlyTheStartupMocksFromDisk(t *testing.T) {
	a, p, logs := stagingStore(t)
	err := a.BeginTestSet(context.Background(), models.TestSetBegin{
		FirstRecordedTestStart: stageT0,
		StrictMockWindow:       true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := loadedFromDisk(t, logs); !reflect.DeepEqual(got, []int64{2}) {
		t.Fatalf("the set's begin read %v mocks from disk; want the 2 recorded before the first test", got)
	}
	if len(p.begun) != 1 {
		t.Fatalf("began %d times, want once", len(p.begun))
	}
	if got, want := sortedNames(p.begun[0][0]), []string{"boot-auth", "boot-config"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("the set began with %v; want the app-startup mocks %v", got, want)
	}
	if !p.firsts[0].Equal(stageT0) {
		t.Fatalf("the set began with cutoff %v, want its first recorded test %v", p.firsts[0], stageT0)
	}
}

// The mapping's startup mocks are the set's too, wherever they were recorded.
func TestBeginTestSetReadsTheMappingsStartupMocks(t *testing.T) {
	a, p, logs := stagingStore(t)
	err := a.BeginTestSet(context.Background(), models.TestSetBegin{
		FirstRecordedTestStart: stageT0,
		StartupMockNames:       []string{"test-a", "boot-auth"},
		StrictMockWindow:       true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := loadedFromDisk(t, logs); !reflect.DeepEqual(got, []int64{3}) {
		t.Fatalf("the set's begin read %v mocks from disk; want the 2 recorded before the first test and test-a", got)
	}
	if got, want := sortedNames(p.begun[0][0]), []string{"boot-auth", "boot-config", "test-a"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("the set began with %v; want %v", got, want)
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

// A set whose tests carry no request time has no first test to cut at, so it
// begins with every mock; and so does a set replayed with the lax window, whose
// tests may be served any mock of the recording.
func TestBeginTestSetWithoutACutReadsEveryMock(t *testing.T) {
	for _, tc := range []struct {
		name  string
		begin models.TestSetBegin
	}{
		{"no recorded test time", models.TestSetBegin{StrictMockWindow: true}},
		{"lax", models.TestSetBegin{FirstRecordedTestStart: stageT0}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, p, logs := stagingStore(t)
			if tc.name == "lax" {
				t.Setenv("KEPLOY_STRICT_MOCK_WINDOW", "0")
			}
			if err := a.BeginTestSet(context.Background(), tc.begin); err != nil {
				t.Fatal(err)
			}
			if got := loadedFromDisk(t, logs); !reflect.DeepEqual(got, []int64{5}) {
				t.Fatalf("the set's begin read %v mocks from disk; want all 5", got)
			}
			if len(p.begun[0][0]) != 5 {
				t.Fatalf("the set began with %v; want all 5", p.begun[0][0])
			}
		})
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
// which the begin's startup band does not reach. An app that prepares at
// start a statement it runs only inside a test is described from it. The
// set's begin, a test's own call and a scope staging (its begin and end)
// leave it as stored, and the next set's store replaces it, with none when
// that store keeps nothing on disk.
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

	if err := a.BeginTestSet(context.Background(), models.TestSetBegin{
		FirstRecordedTestStart: stageT0,
		StrictMockWindow:       true,
	}); err != nil {
		t.Fatal(err)
	}
	if got, want := sortedNames(p.begun[0][0]), []string{"boot-select"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("the set began with %v; want only the startup mocks %v (the catalog serves nothing)", got, want)
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

// Under the proven switch the set's exact begin stages, with the app's
// startup traffic, the per-test mocks of its pool that the on-disk store
// holds: calls recorded while no test was open (a poll between tests, a
// publish after the last test), which no test owns and the pool serves by
// name between tests from what is staged. Reading only the startup band left
// them unstaged, and every such call missed. What tests own is still read
// only as each test opens.
func TestTheExactStagingStagesThePoolsMocksFromDisk(t *testing.T) {
	t.Setenv("KEPLOY_PROVEN", "1")
	t.Setenv("KEPLOY_STRICT_MOCK_WINDOW", "1")
	p := &exactProxy{}
	a := diskAgent(t, p,
		stageMock("boot-config", "/config", stageT0.Add(-20*time.Second)),
		stageMock("boot-auth", "/auth", stageT0.Add(-5*time.Second)),
		stageMock("test-a", "/q", stageT0.Add(time.Second)),
		stageMock("test-b", "/q", stageT0.Add(time.Minute)),
		stageMock("poll-later", "/poll", stageT0.Add(time.Hour)))
	core, logs := observer.New(zap.DebugLevel)
	a.logger = zap.New(core)
	err := a.BeginTestSet(context.Background(), models.TestSetBegin{
		FirstRecordedTestStart: stageT0,
		StrictMockWindow:       true,
		Exact:                  &models.ExactPool{Pool: []string{"boot-config", "boot-auth", "poll-later"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.begun) != 1 {
		t.Fatalf("began %d times, want once", len(p.begun))
	}
	if got, want := sortedNames(p.begun[0][0]), []string{"boot-auth", "boot-config", "poll-later"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("the exact begin staged %v; want the startup mocks and the pool's poll %v, and no test's own", got, want)
	}
	// The pool's mocks the startup band read already are not read again: the
	// begin reads three mocks from disk, and says one came for the pool.
	if got := loadedFromDisk(t, logs); !reflect.DeepEqual(got, []int64{3}) {
		t.Fatalf("the exact begin read %v mocks from disk; want 3, each once", got)
	}
	staged := logs.FilterMessageSnippet("staged the set's app-startup mocks from the on-disk store").All()
	if len(staged) != 1 {
		t.Fatalf("%d staging lines, want one", len(staged))
	}
	if n, _ := staged[0].ContextMap()["poolMocks"].(int64); n != 1 {
		t.Fatalf("the staging line says %v pool mocks; want 1, the poll the startup band did not read", staged[0].ContextMap()["poolMocks"])
	}
}

// workerExactProxy is an exactProxy with a worker scope
// (coreAgent.ExactWorkerServer): it keeps each reachable list it is told.
type workerExactProxy struct {
	exactProxy
	workers [][]string
	// source is the recording source the staging set
	// (coreAgent.RecordingSourceSetter).
	source func(kind models.Kind, keys []string, visit func(name string, load func() (*models.Mock, error)) bool) error
}

func (p *workerExactProxy) ServeExactWorker(names []string) error {
	p.workers = append(p.workers, append([]string{}, names...))
	return nil
}

func (p *workerExactProxy) SetRecordingSource(src func(kind models.Kind, keys []string, visit func(name string, load func() (*models.Mock, error)) bool) error) error {
	p.source = src
	return nil
}

// Under the proven switch the set's exact begin stages the worker scope's
// mocks (models.Mapping.Worker) by name from the on-disk store, as it does the
// pool's: a worker call recorded after the set's first test is in no startup
// band, and unstaged, it would miss wherever the replay made it. Before the
// begin stages, the previous set's worker scope is withdrawn: names are
// unique only within a set.
func TestTheExactStagingStagesTheWorkerScopesMocksFromDisk(t *testing.T) {
	t.Setenv("KEPLOY_PROVEN", "1")
	t.Setenv("KEPLOY_STRICT_MOCK_WINDOW", "1")
	defer models.RegisterRequestKeys(models.HTTP, func(m *models.Mock) []string {
		if m.Name == "test-a" {
			return []string{"key-of-test-a"}
		}
		return nil
	})()
	p := &workerExactProxy{}
	a := diskAgent(t, p,
		stageMock("boot-config", "/config", stageT0.Add(-20*time.Second)),
		stageMock("test-a", "/q", stageT0.Add(time.Second)),
		stageMock("w-delete", "/delete", stageT0.Add(30*time.Second)),
		stageMock("w-poll", "/poll", stageT0.Add(time.Hour)))
	err := a.BeginTestSet(context.Background(), models.TestSetBegin{
		FirstRecordedTestStart: stageT0,
		StrictMockWindow:       true,
		Exact: &models.ExactPool{Pool: []string{"boot-config"},
			Worker: []models.MockEntry{{Name: "w-delete"}, {Name: "w-poll", Place: "test-a"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := sortedNames(p.begun[0][0]), []string{"boot-config", "w-delete", "w-poll"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("the exact begin staged %v; want the startup mock and the worker scope's %v, and no test's own", got, want)
	}
	if want := [][]string{{}, {"w-delete"}}; !reflect.DeepEqual(p.workers, want) {
		t.Fatalf("the proxy was told the worker scopes %v; want %v: none before the begin stages, then the one placed at the set's start", p.workers, want)
	}
	// A miss's diagnosis reads the on-disk store through the source the
	// begin set: the test's own recording, staged only as it opens, is there
	// before it.
	if p.source == nil {
		t.Fatal("the begin set no recording source over the on-disk store")
	}
	var onDisk []string
	found := false
	if err := p.source(models.HTTP, []string{"key-of-test-a"}, func(name string, load func() (*models.Mock, error)) bool {
		onDisk = append(onDisk, name)
		m, err := load()
		if err != nil {
			t.Fatal(err)
		}
		found = found || m.Name == "test-a"
		return true
	}); err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatalf("the recording source visited %v, none of them test-a's own", onDisk)
	}
	// The next set's begin withdraws this set's worker scope before its view
	// goes on: none of its names is reachable in the next set.
	if err := a.BeginTestSet(context.Background(), models.TestSetBegin{
		FirstRecordedTestStart: stageT0, StrictMockWindow: true,
		Exact: &models.ExactPool{Pool: []string{"boot-config"}},
	}); err != nil {
		t.Fatal(err)
	}
	if got := p.workers[2:]; !reflect.DeepEqual(got, [][]string{{}, {}}) {
		t.Fatalf("the next set's begin told the proxy %v; want the previous set's worker scope withdrawn", got)
	}
}

// A set's begin also stages, beside the startup band, the per-test mocks a
// parser declares it needs staged whatever their recorded time
// (models.StagingWithStartup): here the pushes of a broker kind, one
// recorded before the first test, one at its start, one inside it and one in
// the gap after it. It reads only those, never the rest of the recording, and
// a test's own call is unchanged. The mock manager files them as startup
// traffic, and the first test refiles that tier to what was recorded before
// it.
func TestBeginTestSetAlsoStagesTheMocksAParserDeclares(t *testing.T) {
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
		stageMock("test-b", "/q", stageT0.Add(time.Minute)))
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

	err := a.BeginTestSet(context.Background(), models.TestSetBegin{
		FirstRecordedTestStart: stageT0,
		StrictMockWindow:       true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := loadedFromDisk(t, logs); !reflect.DeepEqual(got, []int64{5}) {
		t.Fatalf("the set's begin read %v mocks from disk; want the 2 startup mocks and the 3 later pushes", got)
	}
	staged := logs.FilterMessage("staged the set's app-startup mocks from the on-disk store; each test reads its own when it runs").All()
	if len(staged) != 1 || staged[0].ContextMap()["startupMocks"] != int64(2) || staged[0].ContextMap()["declaredMocks"] != int64(3) {
		t.Fatalf("the begin logged %v; want 2 startup mocks and 3 declared ones", staged)
	}
	if got, want := startupNames(), []string{"boot-config", "push-at-cut", "push-boot", "push-gap", "push-in-a"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("the set's begin filed %v as startup traffic; want %v", got, want)
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

// With no kind declaring a need, the set's begin reads the startup band only,
// as before.
func TestBeginTestSetWithNoDeclaredNeedReadsOnlyTheStartupBand(t *testing.T) {
	t.Setenv("KEPLOY_STRICT_MOCK_WINDOW", "1")
	p := &recordingProxy{}
	a := diskAgent(t, p,
		agentBroker("push-boot", agentPulsar, "MESSAGE", stageT0.Add(-time.Second)),
		agentBroker("push-gap", agentPulsar, "MESSAGE", stageT0.Add(30*time.Second)))
	if err := a.BeginTestSet(context.Background(), models.TestSetBegin{
		FirstRecordedTestStart: stageT0,
		StrictMockWindow:       true,
	}); err != nil {
		t.Fatal(err)
	}
	if got := sortedNames(p.begun[0][0]); !reflect.DeepEqual(got, []string{"push-boot"}) {
		t.Fatalf("staged %v; want only the startup band", got)
	}
}

// destinationsProxy records what the agent seeds as the stored set's
// recorded destinations.
type destinationsProxy struct {
	recordingProxy
	seeded [][]string
}

func (p *destinationsProxy) SeedRecordedDestinations(mocks []*models.Mock) {
	p.seeded = append(p.seeded, sortedNames(namesOf(mocks)))
}

// When a store is published (finalizeClientMocks) the agent seeds the proxy
// with the first mock its on-disk store holds of each recorded destination:
// the whole recording's, not only what a staging stages. A store with no
// on-disk store seeds none, replacing the previous store's.
func TestAStoreSeedsTheProxyWithItsRecordedDestinations(t *testing.T) {
	t.Setenv("KEPLOY_STRICT_MOCK_WINDOW", "1")
	at := func(name, addr string, d time.Duration) *models.Mock {
		m := stageMock(name, "/q", stageT0.Add(d))
		m.Kind = models.MySQL
		m.Spec.Metadata["destAddr"] = addr
		return m
	}
	disk, err := proxyPkg.NewDiskMocks(zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range []*models.Mock{
		at("db-first", "10.0.0.5:6033", time.Second),
		at("db-later", "10.0.0.5:6033", time.Hour),
		at("cache", "10.0.0.6:6390", time.Minute),
	} {
		if err := disk.Add(m); err != nil {
			t.Fatal(err)
		}
	}
	disk.Finalize()
	p := &destinationsProxy{}
	a := &Agent{logger: zap.NewNop(), Proxy: p}
	a.finalizeClientMocks(context.Background(), &ClientMockStorage{diskMocks: disk})
	a.finalizeClientMocks(context.Background(), &ClientMockStorage{})
	if want := [][]string{{"cache", "db-first"}, nil}; !reflect.DeepEqual(p.seeded, want) {
		t.Fatalf("seeded %v; want the first mock of each destination, then none", p.seeded)
	}
}
