package mock

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// A test keeps the begin of its window in the recording; a test begun twice
// keeps the first, and a window without a start gives none.
func TestStampBeginsKeepsEachTestsFirstBegin(t *testing.T) {
	t0 := time.Date(2026, 10, 6, 9, 0, 0, 0, time.UTC)
	byCase := map[string]models.MappedTestCase{"pkg::TestA": {Dir: "pkg"}}
	stampBegins(byCase, []models.ScopeWindow{
		{Name: "pkg::TestA", Start: t0.Add(time.Minute), End: t0.Add(2 * time.Minute)},
		{Name: "pkg::TestA", Start: t0, End: t0.Add(time.Second)},
		{Name: "pkg::TestB", Start: t0.Add(time.Hour)},
		{Name: "pkg::TestC"},
	})
	if got := byCase["pkg::TestA"]; !got.At.Equal(t0) || got.Dir != "pkg" {
		t.Fatalf("pkg::TestA: %+v", got)
	}
	if got := byCase["pkg::TestB"]; !got.At.Equal(t0.Add(time.Hour)) {
		t.Fatalf("pkg::TestB: %+v", got)
	}
	if got, ok := byCase["pkg::TestC"]; ok {
		t.Fatalf("a window with no start gave pkg::TestC %+v", got)
	}
}

type beginsPusher struct {
	Instrumentation
	begins map[string]time.Time
	real   map[string][]string
	seeds  map[string]string
	cases  map[string][]models.ClientCase
	calls  int
}

func (p *beginsPusher) PushTestBegins(_ context.Context, begins map[string]time.Time, real map[string][]string, seeds map[string]string, cases map[string][]models.ClientCase) error {
	p.begins, p.real, p.seeds, p.cases = begins, real, seeds, cases
	p.calls++
	return nil
}

// Replay hands the agent each test's recorded begin; a recording without
// begins still sends an (empty) table.
func TestPushTestBeginsSendsTheRecordingsBegins(t *testing.T) {
	t0 := time.Date(2026, 10, 6, 9, 0, 0, 0, time.UTC)
	p := &beginsPusher{}
	m := &mockService{logger: zap.NewNop(), instrumentation: p, mappingDB: setTableDB{mapping: &models.Mapping{
		TestCases: []models.MappedTestCase{
			{ID: "pkg::TestA", At: t0.Add(time.Minute)},
			{ID: "pkg::TestB", At: t0},
			{ID: "pkg::TestOld"},
		},
	}}}
	if err := m.pushTestBegins(context.Background(), "rec"); err != nil {
		t.Fatal(err)
	}
	if len(p.begins) != 2 || !p.begins["pkg::TestA"].Equal(t0.Add(time.Minute)) || !p.begins["pkg::TestB"].Equal(t0) {
		t.Fatalf("pushed begins %v", p.begins)
	}
	if p.real != nil {
		t.Fatalf("a recording with no REAL test pushed %v", p.real)
	}

	p = &beginsPusher{}
	m = &mockService{logger: zap.NewNop(), instrumentation: p, mappingDB: setTableDB{}}
	if err := m.pushTestBegins(context.Background(), "rec"); err != nil {
		t.Fatal(err)
	}
	if p.calls != 1 || len(p.begins) != 0 {
		t.Fatalf("pushed begins %v (%d calls)", p.begins, p.calls)
	}
}

// A test's mapping keeps the seed its browser drew from in the recording: of
// a test run twice (a retry, which draws the first attempt's seed) the
// earliest window's with one; a window with none gives none.
func TestStampSeedsKeepsTheSeedEachTestsBrowserDrewFrom(t *testing.T) {
	t0 := time.Date(2026, 10, 6, 9, 0, 0, 0, time.UTC)
	byCase := map[string]models.MappedTestCase{"ui::tr": {Dir: "tests"}}
	stampSeeds(byCase, []models.ScopeWindow{
		{Name: "ui::tr", Start: t0.Add(time.Minute), Seed: "2222222222222222"},
		{Name: "ui::tr", Start: t0, Seed: "1111111111111111"},
		{Name: "ui::tr", Start: t0.Add(-time.Minute)},
		{Name: "ui::history", Start: t0},
	})
	if got := byCase["ui::tr"]; got.Seed != "1111111111111111" || got.Dir != "tests" {
		t.Fatalf("ui::tr: %+v", got)
	}
	if got, ok := byCase["ui::history"]; ok {
		t.Fatalf("a window with no seed gave ui::history %+v", got)
	}
}

// Replay hands the agent the seed each test's recording kept, by the test's
// entry: an entry renamed keeps its seed under its new name, and one recorded
// before keploy kept seeds (or with seeding off) sends none.
func TestPushTestBeginsSendsEachTestsSeed(t *testing.T) {
	t0 := time.Date(2026, 10, 6, 9, 0, 0, 0, time.UTC)
	p := &beginsPusher{}
	m := &mockService{logger: zap.NewNop(), instrumentation: p, mappingDB: setTableDB{mapping: &models.Mapping{
		TestCases: []models.MappedTestCase{
			{ID: "ui::tr renders the paginated case list", At: t0, Seed: "0123456789abcdef"},
			{ID: "ui::history", At: t0.Add(time.Minute)},
		},
	}}}
	if err := m.pushTestBegins(context.Background(), "rec"); err != nil {
		t.Fatal(err)
	}
	if len(p.seeds) != 1 || p.seeds["ui::tr renders the paginated case list"] != "0123456789abcdef" {
		t.Fatalf("pushed seeds %v", p.seeds)
	}

	p = &beginsPusher{}
	m = &mockService{logger: zap.NewNop(), instrumentation: p, mappingDB: setTableDB{mapping: &models.Mapping{
		TestCases: []models.MappedTestCase{{ID: "ui::history"}},
	}}}
	if err := m.pushTestBegins(context.Background(), "rec"); err != nil {
		t.Fatal(err)
	}
	if p.seeds != nil {
		t.Fatalf("a recording that kept no seed pushed %v", p.seeds)
	}
}

// An agent client that cannot move the clock per test refuses the replay.
func TestPushTestBeginsRefusesWithoutTheCapability(t *testing.T) {
	m := &mockService{logger: zap.NewNop(), instrumentation: &setTablePusher{}, mappingDB: setTableDB{}}
	if err := m.pushTestBegins(context.Background(), "rec"); err == nil {
		t.Fatal("want an error")
	}
}

// mappingsOnly is a mapping DB that cannot read a recording's mappings back.
type mappingsOnly struct{ MappingDB }

// A keploy that cannot read the recording's mappings refuses the replay rather
// than send no begins, which would run every test at the start time with no
// word; so does one with no mapping DB at all.
func TestPushTestBeginsRefusesWithoutTheMappings(t *testing.T) {
	for _, db := range []MappingDB{mappingsOnly{}, nil} {
		p := &beginsPusher{}
		m := &mockService{logger: zap.NewNop(), instrumentation: p, mappingDB: db}
		err := m.pushTestBegins(context.Background(), "rec")
		if err == nil || !strings.Contains(err.Error(), "cannot read when the recording's tests began") {
			t.Fatalf("mapping DB %T: got %v, want the refusal", db, err)
		}
		if p.calls != 0 {
			t.Fatalf("mapping DB %T: sent the agent %v", db, p.begins)
		}
	}
}

// Under the proven switch, an agent that does not take the tests' recorded
// begins refuses the replay before the test command runs: its tests would run
// on another clock than their recording's. A replay that can sends them.
func TestReplayRefusesWhenTheAgentCannotTakeTheBegins(t *testing.T) {
	provenOn(t)
	instr := newInstr(t, agentUpFromSetup, false, models.AppError{AppErrorType: models.ErrAppStopped})
	instr.beginsErr = errors.New("the agent did not take the tests' recorded begins: status 404")
	err := replayErr(t, instr, utils.Native)
	if err == nil || !strings.Contains(err.Error(), "did not take the tests' recorded begins") {
		t.Fatalf("Replay returned %v, want the clock refusal", err)
	}
	if runs, _ := instr.counts(); runs != 0 {
		t.Fatalf("the test command ran %d times after the replay was refused", runs)
	}
}

// Each test keeps the key of the claim it began under, its test process's,
// so a reader who moves one of its calls to worker: can name the key a
// replay serves the moved entry under, in a set whose test processes have
// several. A test begun before any claim, or with no start, gets none.
func TestStampKeysGivesEachTestItsProcesssClaimKey(t *testing.T) {
	t0 := time.Date(2026, 10, 6, 9, 0, 0, 0, time.UTC)
	byCase := map[string]models.MappedTestCase{"a::TestA": {Dir: "a"}}
	claims := []models.ScopeWindow{{Claim: "a", Start: t0}, {Claim: "b", Start: t0.Add(time.Minute)}}
	stampKeys(byCase, []models.ScopeWindow{
		{Name: "TestEarly", Start: t0.Add(-time.Second)},
		{Name: "a::TestA", Start: t0.Add(time.Second), End: t0.Add(2 * time.Second)},
		{Name: "b::TestB", Start: t0.Add(2 * time.Minute)},
		{Name: "b::TestC"},
	}, claims)
	if got := byCase["a::TestA"]; got.Key != "a" || got.Dir != "a" {
		t.Fatalf("a::TestA: %+v", got)
	}
	if got := byCase["b::TestB"]; got.Key != "b" {
		t.Fatalf("b::TestB: %+v", got)
	}
	for _, id := range []string{"TestEarly", "b::TestC"} {
		if got, ok := byCase[id]; ok {
			t.Fatalf("%s got %+v", id, got)
		}
	}
}

// Replay hands the agent the requests each client test sent in its
// recording, read from its HTTP test cases in the order they reached the
// application: a case the store does not hold, or not of HTTP, is left out,
// and a recording that kept no requests sends none.
func TestPushTestBeginsSendsEachClientTestsRequests(t *testing.T) {
	t0 := time.Date(2026, 11, 9, 9, 0, 0, 0, time.UTC)
	req := func(path string, at time.Time) models.HTTPReq {
		return models.HTTPReq{Method: "POST", URL: "http://api" + path, Timestamp: at}
	}
	db := &memTestDB{existing: []*models.TestCase{
		{Name: "post-apps-2", Kind: models.HTTP, HTTPReq: req("/apps", t0.Add(2*time.Second))},
		{Name: "post-apps-1", Kind: models.HTTP, HTTPReq: req("/apps", t0.Add(time.Second))},
		{Name: "grpc-1", Kind: models.GRPC_EXPORT, HTTPReq: req("/grpc", t0)},
		{Name: "get-suites-1", Kind: models.HTTP, HTTPReq: req("/suites", t0)},
	}}
	p := &beginsPusher{}
	m := &mockService{logger: zap.NewNop(), instrumentation: p, testDB: db, mappingDB: setTableDB{mapping: &models.Mapping{
		TestCases: []models.MappedTestCase{
			{ID: "pkg::TestApps", Cases: []string{"post-apps-2", "post-apps-1", "grpc-1", "gone-1"}},
			{ID: "pkg::TestSuites", Cases: []string{"get-suites-1"}},
			{ID: "pkg::TestNone"},
		},
	}}}
	if err := m.pushTestBegins(context.Background(), "rec"); err != nil {
		t.Fatal(err)
	}
	apps := p.cases["pkg::TestApps"]
	if len(p.cases) != 2 || len(apps) != 2 || apps[0].Name != "post-apps-1" || apps[1].Name != "post-apps-2" ||
		apps[0].Req.URL != "http://api/apps" || len(p.cases["pkg::TestSuites"]) != 1 {
		t.Fatalf("pushed cases %+v", p.cases)
	}

	p = &beginsPusher{}
	counted := &countingTestDB{TestDB: db}
	m = &mockService{logger: zap.NewNop(), instrumentation: p, testDB: counted, mappingDB: setTableDB{mapping: &models.Mapping{
		TestCases: []models.MappedTestCase{{ID: "pkg::TestNone"}},
	}}}
	if err := m.pushTestBegins(context.Background(), "rec"); err != nil {
		t.Fatal(err)
	}
	if p.cases != nil || counted.reads != 0 {
		t.Fatalf("a recording that kept no requests pushed %+v, reading its test cases %d times", p.cases, counted.reads)
	}

	// A store keploy cannot read gives none, and says so; the begins still go.
	p = &beginsPusher{}
	core, logs := observer.New(zap.WarnLevel)
	m = &mockService{logger: zap.New(core), instrumentation: p, testDB: &countingTestDB{TestDB: db, err: errors.New("unreadable")}, mappingDB: setTableDB{mapping: &models.Mapping{
		TestCases: []models.MappedTestCase{{ID: "pkg::TestApps", At: t0, Cases: []string{"post-apps-1"}}},
	}}}
	if err := m.pushTestBegins(context.Background(), "rec"); err != nil {
		t.Fatal(err)
	}
	if p.cases != nil || p.begins["pkg::TestApps"].IsZero() || logs.FilterMessageSnippet("cannot read the requests").Len() != 1 {
		t.Fatalf("an unreadable store pushed cases %+v, begins %v, and said %v", p.cases, p.begins, logs.All())
	}
}

// countingTestDB counts the reads of a recording's test cases, failing each
// with err where it is set.
type countingTestDB struct {
	TestDB
	reads int
	err   error
}

func (d *countingTestDB) GetTestCases(ctx context.Context, set string) ([]*models.TestCase, error) {
	d.reads++
	if d.err != nil {
		return nil, d.err
	}
	return d.TestDB.GetTestCases(ctx, set)
}
