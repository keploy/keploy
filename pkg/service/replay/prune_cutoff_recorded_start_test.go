package replay

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml"
	"go.keploy.io/server/v3/pkg/platform/yaml/mockdb"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func insertHTTPMock(t *testing.T, ys *mockdb.MockYaml, testSetID, urlSuffix string, at time.Time, meta map[string]string) string {
	t.Helper()
	if meta == nil {
		meta = map[string]string{"type": "HTTP_CLIENT"}
	}
	m := &models.Mock{
		Version: models.GetVersion(),
		Kind:    models.HTTP,
		Spec: models.MockSpec{
			Metadata:         meta,
			HTTPReq:          &models.HTTPReq{Method: "GET", URL: "http://dep.internal/" + urlSuffix, ProtoMajor: 1, ProtoMinor: 1},
			HTTPResp:         &models.HTTPResp{StatusCode: 200, StatusMessage: "OK", Body: `{"ok":true}`},
			ReqTimestampMock: at,
			ResTimestampMock: at.Add(time.Millisecond),
		},
	}
	if err := ys.InsertMock(context.Background(), m, testSetID); err != nil {
		t.Fatalf("InsertMock(%s): %v", urlSuffix, err)
	}
	return m.Name
}

func readMockNamesFromDisk(t *testing.T, root, testSetID string) []string {
	t.Helper()
	setDir := filepath.Join(root, testSetID)
	reader, err := yaml.NewRecordingMockReaderF(context.Background(), zap.NewNop(), root, setDir, "mocks", yaml.FormatYAML)
	if err != nil {
		t.Fatalf("open mocks.yaml: %v", err)
	}
	defer func() { _ = reader.Close() }()

	var names []string
	for {
		doc, _, err := reader.ReadNextDocBytes()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("read mocks.yaml doc: %v", err)
		}
		if doc != nil {
			names = append(names, doc.Name)
		}
	}
	return names
}

func newDiskPruneRoundReplayer(
	t *testing.T,
	dir, addr, testSetID string,
	testDB TestDB,
	mockStore *mockdb.MockYaml,
	cases []*models.TestCase,
	consumedByTest map[string]models.MockState,
	logger *zap.Logger,
) *Replayer {
	t.Helper()
	cfg := &config.Config{}
	cfg.Path = dir
	cfg.Test.HealthPollTimeout = 2 * time.Second
	cfg.Test.Host = "127.0.0.1"
	cfg.Test.RemoveUnusedMocks = true
	if err := os.MkdirAll(filepath.Join(dir, testSetID), 0o755); err != nil {
		t.Fatalf("mkdir test set dir: %v", err)
	}

	// Simulate realistic consumption: boot drain before any test returns nil
	// (so boot mocks survive solely via startupCutoffTime, not consumption),
	// and each test case consumes only its own recorded per-test mock.
	var calls int
	hooks := prHooks{
		consumedFor: func() []models.MockState {
			calls++
			if calls == 1 {
				return nil
			}
			idx := calls - 2
			if idx >= 0 && idx < len(cases) {
				if st, ok := consumedByTest[cases[idx].Name]; ok {
					return []models.MockState{st}
				}
			}
			return nil
		},
	}

	instr := &prInstr{appStopped: make(chan struct{}), cases: cases}
	return &Replayer{
		logger:               logger,
		testDB:               testDB,
		mockDB:               mockStore,
		mappingDB:            &prMappingDB{},
		reportDB:             &prReportDB{},
		testSetConf:          prTestSetConf{},
		telemetry:            prTelemetry{},
		instrumentation:      instr,
		config:               cfg,
		instrument:           true,
		hookImpl:             hooks,
		completeTestReport:   make(map[string]TestReportVerdict),
		failedTCsBySetID:     make(map[string][]string),
		mockMismatchFailures: NewTestFailureStore(),
		consumedMockNames:    make(map[string]struct{}),
		console:              stdoutConsole{},
	}
}

// TestAutoReplayMultiRoundPruneStaysBoundedWithRecordedStart simulates 10
// continuous auto-replay rounds against one persistent mocks.yaml where
// replayed test cases are deleted after every round while RecordedStartReader
// remains pinned to Round 1's earliest test (T0).
//
// Rounds alternate between > StartupMockTestCaseWindow (8 tests) and
// <= StartupMockTestCaseWindow (3 tests) to exercise both cutoff branches:
// without consulting RecordedFirstTestStart, startupMockCutoff ratchets forward
// to tcTimes[5] (or pruneBefore for <= 5 tests), keeping 100% of historical
// mocks across every round. With the fix, genuine boot mocks (< T0), config
// mocks, and the current round's consumed mocks survive while unused historical
// mocks from prior rounds and background traffic are pruned, keeping the mock
// file strictly bounded across all 10 rounds.
func TestAutoReplayMultiRoundPruneStaysBoundedWithRecordedStart(t *testing.T) {
	t.Setenv("KEPLOY_MOCK_FORMAT", "")
	t.Setenv(provenswitch.Env, "")

	const (
		testSetID   = "test-set-0"
		numRounds   = 10
		bgPerRound  = 20
		bootCount   = 3
		earlyR1Bg   = 5
		configCount = 1
	)

	dir := t.TempDir()
	addr := prAppListener(t)
	ctx := context.Background()
	ys := mockdb.New(zap.NewNop(), dir, "mocks")

	// Anchor all simulated recorded traffic strictly in the past relative to
	// RunTestSet's pruneBefore (time.Now().UTC()).
	t0 := time.Now().UTC().Add(-12 * time.Hour).Truncate(time.Second)

	// 1. Seed boot mocks strictly before T0 — never consumed during rounds
	// 1..10, so they survive solely via startupCutoffTime.
	bootNames := make([]string, 0, bootCount)
	for i := range bootCount {
		at := t0.Add(-time.Duration(bootCount-i) * 5 * time.Second)
		name := insertHTTPMock(t, ys, testSetID, fmt.Sprintf("boot-%d", i), at, nil)
		bootNames = append(bootNames, name)
	}

	// 2. Seed a type:config mock recorded AFTER T0 to verify config mocks
	// remain exempt from pruning even when startupCutoffTime pins to T0.
	configMockName := insertHTTPMock(t, ys, testSetID, "cfg-post-t0", t0.Add(30*time.Minute), map[string]string{"type": "config"})

	// 3. Seed early-window unconsumed mocks inside Round 1's [T0, tcTimes[5])
	// startup window. They survive Round 1 (zero regression on Round 1), and
	// must be pruned from Round 2 onwards once Round 1's tests are deleted.
	earlyR1Names := make([]string, 0, earlyR1Bg)
	for i := range earlyR1Bg {
		at := t0.Add(time.Duration(i)*500*time.Millisecond + 100*time.Millisecond)
		name := insertHTTPMock(t, ys, testSetID, fmt.Sprintf("r1-early-bg-%d", i), at, nil)
		earlyR1Names = append(earlyR1Names, name)
	}

	// Alternate > 5 and <= 5 test rounds across 10 rounds.
	roundTestCounts := [numRounds]int{8, 3, 8, 3, 8, 3, 8, 3, 8, 3}
	keptPerRound := make([]int, 0, numRounds)

	for round := 1; round <= numRounds; round++ {
		nTests := roundTestCounts[round-1]
		roundStart := t0.Add(time.Duration(round-1) * time.Hour)

		// Append unconsumed background mocks for this round.
		// Round 1 places them after its startup window (t0+10m..29m);
		// Rounds 2..10 place them BEFORE the round's tests (in [t0, roundStart))
		// — the exact historical region immortalized by the ratcheting bug.
		for b := range bgPerRound {
			var at time.Time
			if round == 1 {
				at = roundStart.Add(10*time.Minute + time.Duration(b)*time.Minute)
			} else {
				at = roundStart.Add(-time.Duration(bgPerRound-b) * time.Minute)
			}
			insertHTTPMock(t, ys, testSetID, fmt.Sprintf("r%d-bg-%d", round, b), at, nil)
		}

		// Build this round's fresh test cases and their matching per-test mocks.
		cases := make([]*models.TestCase, 0, nTests)
		consumed := make(map[string]models.MockState, nTests)
		expectedRoundMocks := make([]string, 0, nTests)
		for i := range nTests {
			tcName := fmt.Sprintf("r%d-test-%d", round, i)
			reqAt := roundStart.Add(time.Duration(i) * time.Second)
			tc := prTestCase(tcName, addr)
			tc.HTTPReq.Timestamp = reqAt
			tc.HTTPResp.Timestamp = reqAt.Add(50 * time.Millisecond)
			cases = append(cases, tc)

			mockName := insertHTTPMock(t, ys, testSetID, fmt.Sprintf("r%d-tc-%d-mock", round, i), reqAt.Add(10*time.Millisecond), nil)
			consumed[tcName] = models.MockState{Name: mockName, Kind: models.HTTP}
			expectedRoundMocks = append(expectedRoundMocks, mockName)
		}

		// TestDB holds ONLY this round's tests (prior rounds were deleted),
		// while RecordedFirstTestStart remains pinned at T0.
		tdb := &recordedStartDB{prTestDB: prTestDB{cases: cases}, at: t0}
		r := newDiskPruneRoundReplayer(t, dir, addr, testSetID, tdb, ys, cases, consumed, zap.NewNop())

		runCtx, cancel := context.WithTimeout(ctx, prTimeout)
		status, err := r.RunTestSet(runCtx, testSetID, fmt.Sprintf("test-run-%d", round), false)
		cancel()
		if err != nil {
			t.Fatalf("round %d RunTestSet error: %v", round, err)
		}
		if status != models.TestSetStatusPassed {
			t.Fatalf("round %d status = %s, want PASSED", round, status)
		}

		surviving := readMockNamesFromDisk(t, dir, testSetID)
		keptPerRound = append(keptPerRound, len(surviving))

		wantCount := bootCount + configCount + nTests
		if round == 1 {
			wantCount += earlyR1Bg
		}
		t.Logf("round=%02d tests=%d surviving=%d want=%d", round, nTests, len(surviving), wantCount)

		// Boot mocks (< T0) and type:config mock must survive every round.
		for _, want := range append(slices.Clone(bootNames), configMockName) {
			if !slices.Contains(surviving, want) {
				t.Errorf("round %d: expected mock %q to survive pruning, surviving=%v", round, want, surviving)
			}
		}
		// Current round's consumed mocks must survive.
		for _, want := range expectedRoundMocks {
			if !slices.Contains(surviving, want) {
				t.Errorf("round %d: expected consumed mock %q to survive pruning", round, want)
			}
		}
		// Round 1 keeps early-window mocks; Rounds 2..10 must prune them.
		for _, early := range earlyR1Names {
			present := slices.Contains(surviving, early)
			if round == 1 && !present {
				t.Errorf("round 1: expected early startup-window mock %q to survive Round 1", early)
			}
			if round > 1 && present {
				t.Errorf("round %d: stale Round 1 mock %q was not pruned", round, early)
			}
		}
		// Total mock count must match exact bounded expectation.
		if len(surviving) != wantCount {
			t.Errorf("round %d (tests=%d): surviving mock count = %d, want %d (unbounded growth across rounds: %v)",
				round, nTests, len(surviving), wantCount, keptPerRound)
		}
	}
}

// TestRunTestSetPruneFallsBackWhenRecordedStartErrors verifies that an error
// from RecordedStartReader.RecordedFirstTestStart does not fail the run and
// falls back safely to the held test cases' timestamps (retaining mocks rather
// than over-pruning).
func TestRunTestSetPruneFallsBackWhenRecordedStartErrors(t *testing.T) {
	t.Setenv("KEPLOY_MOCK_FORMAT", "")
	t.Setenv(provenswitch.Env, "")

	const testSetID = "test-set-0"
	dir := t.TempDir()
	addr := prAppListener(t)
	ctx := context.Background()
	ys := mockdb.New(zap.NewNop(), dir, "mocks")

	t0 := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Second)
	bootName := insertHTTPMock(t, ys, testSetID, "boot-0", t0.Add(-10*time.Second), nil)
	betweenName := insertHTTPMock(t, ys, testSetID, "between-rounds-0", t0.Add(30*time.Minute), nil)

	// Only 2 tests (<= StartupMockTestCaseWindow), so fallback keeps all mocks.
	roundStart := t0.Add(time.Hour)
	cases := []*models.TestCase{
		prTestCase("test-0", addr),
		prTestCase("test-1", addr),
	}
	for i, tc := range cases {
		at := roundStart.Add(time.Duration(i) * time.Second)
		tc.HTTPReq.Timestamp = at
		tc.HTTPResp.Timestamp = at.Add(50 * time.Millisecond)
	}

	core, logs := observer.New(zap.WarnLevel)
	tdb := &recordedStartDB{
		prTestDB: prTestDB{cases: cases},
		at:       t0,
		err:      errors.New("first-test-start unreadable"),
	}
	r := newDiskPruneRoundReplayer(t, dir, addr, testSetID, tdb, ys, cases, nil, zap.New(core))

	runCtx, cancel := context.WithTimeout(ctx, prTimeout)
	status, err := r.RunTestSet(runCtx, testSetID, "test-run-err", false)
	cancel()
	if err != nil {
		t.Fatalf("RunTestSet error: %v", err)
	}
	if status != models.TestSetStatusPassed {
		t.Fatalf("status = %s, want PASSED", status)
	}

	surviving := readMockNamesFromDisk(t, dir, testSetID)
	if !slices.Contains(surviving, bootName) || !slices.Contains(surviving, betweenName) {
		t.Fatalf("expected fallback to keep mocks (%s, %s) when RecordedFirstTestStart errors; surviving=%v", bootName, betweenName, surviving)
	}
	if logs.FilterMessage("could not read when the test set's first test was recorded; its startup traffic is taken from the test cases it holds now").Len() == 0 {
		t.Fatalf("expected warning log when RecordedFirstTestStart fails; got %d logs", logs.Len())
	}
}
