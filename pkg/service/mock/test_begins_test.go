package mock

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
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
	in     map[string][]models.TestIncoming
	calls  int
}

func (p *beginsPusher) PushTestBegins(_ context.Context, req models.TestBeginsReq) error {
	p.begins, p.real, p.in = req.Begins, req.Real, req.Incoming
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

// Replay hands the agent, by test, the requests the application was sent
// while each test was open in its recording (its mapping's cases, read from
// the set's test cases), earliest first, URL and headers alone, so the
// agent's learn step reads them as the test's given place. A case the store
// does not hold, or of another kind, is left out; a test with none, a
// recording with no test case, or no test-case store gives none.
func TestPushTestBeginsSendsEachTestsRecordedRequests(t *testing.T) {
	t0 := time.Date(2026, 10, 30, 15, 13, 36, 0, time.UTC)
	auth := map[string]string{"Authorization": "Bearer t-1"}
	db := &memTestDB{existing: []*models.TestCase{
		{Name: "test-2", Kind: models.HTTP, HTTPReq: models.HTTPReq{Method: "POST", URL: "http://app/b", Header: auth, Body: "{}", Timestamp: t0.Add(2 * time.Second)}},
		{Name: "test-1", Kind: models.HTTP, HTTPReq: models.HTTPReq{Method: "GET", URL: "http://app/a", Header: auth, Timestamp: t0.Add(time.Second)}},
		{Name: "test-9", Kind: models.HTTP, HTTPReq: models.HTTPReq{Method: "GET", URL: "http://app/other", Timestamp: t0}},
		{Name: "grpc-1", Kind: models.GRPC_EXPORT},
	}}
	mapping := &models.Mapping{TestCases: []models.MappedTestCase{
		{ID: "pkg::TestA", At: t0, Cases: []string{"test-2", "test-1", "grpc-1", "gone-1"}},
		{ID: "pkg::TestB", At: t0},
	}}
	p := &beginsPusher{}
	m := &mockService{logger: zap.NewNop(), instrumentation: p, mappingDB: setTableDB{mapping: mapping}, testDB: db}
	if err := m.pushTestBegins(context.Background(), "rec"); err != nil {
		t.Fatal(err)
	}
	want := map[string][]models.TestIncoming{"pkg::TestA": {
		{Case: "test-1", At: t0.Add(time.Second), URL: "http://app/a", Header: auth},
		{Case: "test-2", At: t0.Add(2 * time.Second), URL: "http://app/b", Header: auth},
	}}
	if !reflect.DeepEqual(p.in, want) {
		t.Fatalf("pushed requests %+v, want %+v", p.in, want)
	}
	// Only the mapped cases are read, one at a time: never the whole set.
	if db.loads != 0 || !reflect.DeepEqual(db.read, []string{"test-2", "test-1", "grpc-1", "gone-1"}) {
		t.Fatalf("read the whole set %d times and the cases %v; want the mapped cases alone", db.loads, db.read)
	}

	// A case keploy cannot read binds nothing from any test's requests.
	p.in = nil
	broken := &mockService{logger: zap.NewNop(), instrumentation: p, mappingDB: setTableDB{mapping: mapping}, testDB: unreadableCase{db}}
	if err := broken.pushTestBegins(context.Background(), "rec"); err != nil {
		t.Fatal(err)
	}
	if p.in != nil {
		t.Fatalf("a case that could not be read: pushed requests %+v", p.in)
	}

	// A test that mapped more cases than the bound keeps its earliest.
	many := &memTestDB{}
	var names []string
	for i := 2*maxTestIncoming + 7; i >= 0; i-- {
		n := fmt.Sprintf("test-%d", i)
		names = append(names, n)
		many.existing = append(many.existing, &models.TestCase{Name: n, Kind: models.HTTP, HTTPReq: models.HTTPReq{URL: "http://app/" + n, Timestamp: t0.Add(time.Duration(i) * time.Millisecond)}})
	}
	p.in = nil
	bounded := &mockService{logger: zap.NewNop(), instrumentation: p, mappingDB: setTableDB{mapping: &models.Mapping{TestCases: []models.MappedTestCase{{ID: "pkg::TestMany", At: t0, Cases: names}}}}, testDB: many}
	if err := bounded.pushTestBegins(context.Background(), "rec"); err != nil {
		t.Fatal(err)
	}
	got := p.in["pkg::TestMany"]
	if len(got) != maxTestIncoming || got[0].Case != "test-0" || got[maxTestIncoming-1].Case != fmt.Sprintf("test-%d", maxTestIncoming-1) {
		t.Fatalf("a test of %d cases pushed %d, first %+v; want its earliest %d", len(names), len(got), got[:1], maxTestIncoming)
	}

	for name, m := range map[string]*mockService{
		"no test-case store": {logger: zap.NewNop(), instrumentation: p, mappingDB: setTableDB{mapping: mapping}},
		"no test case":       {logger: zap.NewNop(), instrumentation: p, mappingDB: setTableDB{mapping: &models.Mapping{TestCases: []models.MappedTestCase{{ID: "pkg::TestA", At: t0}}}}, testDB: db},
	} {
		p.in = nil
		if err := m.pushTestBegins(context.Background(), "rec"); err != nil {
			t.Fatal(err)
		}
		if p.in != nil {
			t.Fatalf("%s: pushed requests %+v", name, p.in)
		}
	}
}

// unreadableCase is a test-case store whose case test-1 cannot be read.
type unreadableCase struct{ *memTestDB }

func (u unreadableCase) GetTestCase(ctx context.Context, set, name string) (*models.TestCase, error) {
	if name == "test-1" {
		return nil, errors.New("permission denied")
	}
	return u.memTestDB.GetTestCase(ctx, set, name)
}
