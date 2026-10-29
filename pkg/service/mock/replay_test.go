package mock

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func TestSetTableCarriesTheFirstSuiteStart(t *testing.T) {
	t0 := time.Date(2026, 10, 6, 10, 0, 0, 0, time.UTC)
	m := &models.Mapping{
		Boots:     []models.BootSpec{{Dir: "tests/e2e", Key: "k", Mocks: []models.MockEntry{{Name: "mock-1"}}}},
		TestCases: []models.MappedTestCase{{ID: "T", Dir: "tests/e2e", Mocks: []models.MockEntry{{Name: "mock-2"}}}},
		Suites: []models.SuiteSpan{
			{Dir: "tests/e2e", Start: t0.Add(time.Second)},
			{Dir: "tests/e2e", Start: t0},
			{Dir: "other", Start: t0.Add(-time.Hour)},
		},
	}
	sets := setTable(m, "")
	if len(sets) != 1 || !sets["tests/e2e"].Start.Equal(t0) {
		t.Fatalf("sets %+v", sets)
	}
}

type setTableDB struct {
	MappingDB
	mapping *models.Mapping
}

func (d setTableDB) GetMapping(context.Context, string) (*models.Mapping, error) {
	return d.mapping, nil
}

type setTablePusher struct {
	Instrumentation
	sets map[string]models.SetTable
}

func (p *setTablePusher) PushSetTable(_ context.Context, _ string, sets map[string]models.SetTable) error {
	p.sets = sets
	return nil
}

func TestPushSetTableWithSuitesButNoBoots(t *testing.T) {
	t0 := time.Date(2026, 10, 12, 1, 6, 5, 0, time.UTC)
	p := &setTablePusher{}
	m := &mockService{logger: zap.NewNop(), instrumentation: p, mappingDB: setTableDB{mapping: &models.Mapping{
		TestCases: []models.MappedTestCase{{ID: "T", Dir: "e2e/b", Mocks: []models.MockEntry{{Name: "mock-1"}}}},
		Suites:    []models.SuiteSpan{{Dir: "e2e/b", Start: t0}},
	}}}
	if !m.pushSetTable(context.Background(), "rec") {
		t.Fatal("the set table was not pushed")
	}
	if !p.sets["e2e/b"].Start.Equal(t0) {
		t.Fatalf("sets %+v", p.sets)
	}
}

func TestPushSetTableSkipsAMappingWithNoBootsOrSuites(t *testing.T) {
	p := &setTablePusher{}
	m := &mockService{logger: zap.NewNop(), instrumentation: p, mappingDB: setTableDB{mapping: &models.Mapping{
		TestCases: []models.MappedTestCase{{ID: "T", Dir: "e2e/b", Mocks: []models.MockEntry{{Name: "mock-1"}}}},
	}}}
	if m.pushSetTable(context.Background(), "rec") || p.sets != nil {
		t.Fatalf("pushed %+v", p.sets)
	}
}

// A replay's start the CLI gave up on for want of progress
// (pkg.ErrAgentNotReady) is tried again with a fresh agent, as keploy test
// tries it, and the replay goes on with that one: each judging round of keploy
// mock record is such a replay, and one start given up on ended the round.
// Any other failure of the start is not tried again.
func TestAReplayTriesAStalledStartAgain(t *testing.T) {
	for _, tc := range []struct {
		name   string
		first  error
		starts int32
		want   error
	}{
		{"a stalled start is tried again", errStalledStart, 2, nil},
		{"a start that cannot work is not", fmt.Errorf("%w: the keploy agent could not start (exit status 3)", utils.ErrPrivilegeRequired), 1, utils.ErrPrivilegeRequired},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := t.TempDir()
			t.Chdir(repo)
			keployDir := filepath.Join(repo, "keploy")
			if err := os.MkdirAll(filepath.Join(keployDir, "set"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(keployDir, "set", "mocks.yaml"), []byte("version: api.keploy.io/v1beta1\nkind: Http\nname: mock-0\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			base := newInstr(t, agentUpFromSetup, false, models.AppError{AppErrorType: models.ErrAppStopped})
			base.consumedMocks = []models.MockState{{Name: "mock-0"}}
			instr := &setupFails{composeInstr: base, errs: []error{tc.first}}
			cfg := instrConfig(base, utils.Native, "go test ./...")
			cfg.Path = keployDir
			cfg.Mock.OnMiss = "fail"
			utils.ErrCode = 0
			t.Cleanup(func() { utils.ErrCode = 0 })
			err := New(zap.NewNop(), instr, stubMockDB{}, nil, nil, nil, cfg).Replay(context.Background())
			if n := instr.setups.Load(); n != tc.starts {
				t.Fatalf("%d starts, want %d (error %v)", n, tc.starts, err)
			}
			if tc.want == nil {
				if err != nil {
					t.Fatalf("the replay failed after its second start came up: %v", err)
				}
				if runs, _ := base.counts(); runs == 0 {
					t.Fatal("the test command never ran after the second start came up")
				}
				return
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("error %v, want %v kept", err, tc.want)
			}
		})
	}
}

// keploy mock says each background call once, worded from where it was made:
// a call made outside every test's window as such, and a call of a marked
// background series made inside a test's window (its parser marks it,
// models.UnmatchedCall.MadeInTest) as made inside one, never outside every
// test (background-poll-series s21).
func TestLogMissSaysWhereABackgroundCallWasMade(t *testing.T) {
	for _, c := range []struct {
		kind, line string
	}{
		{models.MissOutsideTest, "a call the application made outside every test had no recorded answer"},
		{models.MissInTest, "a call of a marked background series, made inside a test's window, had no recorded answer"},
		{models.MissExtraCall, "a call of a marked background series, made inside a test's window, had no recorded answer"},
	} {
		core, logs := observer.New(zap.WarnLevel)
		logMiss(zap.New(core), models.UnmatchedCall{Protocol: "HTTP", ActualSummary: "GET /jobs", Destination: "jobs:80",
			Kind: c.kind, Background: true, ClosestMock: "mock-12"})
		said := logs.All()
		if len(said) != 1 || said[0].Level != zap.WarnLevel || !strings.HasPrefix(said[0].Message, c.line) {
			t.Fatalf("kind %q: said %+v; want one WARN line beginning %q", c.kind, said, c.line)
		}
		if f := said[0].ContextMap(); f["call"] != "GET /jobs" || f["why"] != "no recording of the set is this call; closest mock-12" {
			t.Fatalf("kind %q: the line does not say what was called and why: %+v", c.kind, f)
		}
	}
}
