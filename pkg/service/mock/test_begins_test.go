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
	calls  int
}

func (p *beginsPusher) PushTestBegins(_ context.Context, begins map[string]time.Time, real map[string][]string) error {
	p.begins, p.real = begins, real
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
