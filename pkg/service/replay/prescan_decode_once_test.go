package replay

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sync"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml"
	"go.keploy.io/server/v3/pkg/platform/yaml/testdb"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// Compile-time check that the real YAML/JSON TestDB satisfies the optional
// presence capability used by replay's last-set-that-runs pre-scan.
var _ TestCasePresenceReader = (*testdb.TestYaml)(nil)

// countingTestDB wraps a real *testdb.TestYaml and records how many times
// GetTestCases and HasTestCases are called per test set.
type countingTestDB struct {
	*testdb.TestYaml
	mu       sync.Mutex
	getCalls map[string]int
	hasCalls map[string]int
}

func newCountingTestDB(inner *testdb.TestYaml) *countingTestDB {
	return &countingTestDB{
		TestYaml: inner,
		getCalls: make(map[string]int),
		hasCalls: make(map[string]int),
	}
}

func (c *countingTestDB) GetTestCases(ctx context.Context, testSetID string) ([]*models.TestCase, error) {
	c.mu.Lock()
	c.getCalls[testSetID]++
	c.mu.Unlock()
	return c.TestYaml.GetTestCases(ctx, testSetID)
}

func (c *countingTestDB) HasTestCases(ctx context.Context, testSetID string) (bool, error) {
	c.mu.Lock()
	c.hasCalls[testSetID]++
	c.mu.Unlock()
	return c.TestYaml.HasTestCases(ctx, testSetID)
}

func (c *countingTestDB) PresenceAppliesTo(db any) bool {
	return db == c
}

func (c *countingTestDB) gets(set string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.getCalls[set]
}

func (c *countingTestDB) hases(set string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.hasCalls[set]
}

func writeRecordedHTTPTestCase(t *testing.T, db *testdb.TestYaml, set string, base *models.TestCase) {
	t.Helper()
	cp := *base
	if err := db.InsertTestCase(context.Background(), &cp, set, false); err != nil {
		t.Fatalf("InsertTestCase(%s/%s): %v", set, cp.Name, err)
	}
}

// Start's last-set-that-runs pre-scan must not decode any test set via
// GetTestCases: every set is decoded once (inside RunTestSet), while
// HasTestCases decides which trailing sets are empty and KEPLOY-LAST-TESTCASE
// still lands on the last set that actually runs.
//
// Fails on main: lastSetThatRuns called GetTestCases on every trailing set
// until it hit the last non-empty one, so that set (and any trailing empty
// sets) had GetTestCases called twice.
func TestStart_PrescanDecodesEachSetOnce(t *testing.T) {
	for _, on := range []bool{false, true} {
		mode := "switch off"
		if on {
			mode = "switch on"
		}
		t.Run("every set runs/"+mode, func(t *testing.T) {
			provenOn(t, on)
			h := newPartialRunHarness(t, 1, 0)
			root := t.TempDir()
			inner := testdb.New(zap.NewNop(), root)
			writeRecordedHTTPTestCase(t, inner, "test-set-0", h.cases[0])
			writeRecordedHTTPTestCase(t, inner, "test-set-1", h.cases[0])
			db := newCountingTestDB(inner)

			reports := &setsReportDB{prReportDB: h.report}
			h.replayer.reportDB = reports
			h.replayer.testDB = db
			h.replayer.config.Command = "./app"
			h.replayer.config.CommandType = string(utils.Native)
			h.replayer.config.Test.MaxFlakyChecks = 1
			h.replayer.config.DisableANSI = true
			hooks := &lastCaseHooks{}
			h.replayer.hookImpl = hooks

			_, statuses := startEarlierSetRun(t, h, reports)
			if statuses["test-set-0"] != string(models.TestSetStatusPassed) || statuses["test-set-1"] != string(models.TestSetStatusPassed) {
				t.Fatalf("statuses = %v, want both PASSED", statuses)
			}
			if last, _ := hooks.seen(); !slices.Equal(last, []string{"test-set-1/test-1"}) {
				t.Fatalf("KEPLOY-LAST-TESTCASE sent on %v, want [test-set-1/test-1]", last)
			}
			for _, set := range []string{"test-set-0", "test-set-1"} {
				if got := db.gets(set); got != 1 {
					t.Fatalf("GetTestCases(%s) called %d time(s), want 1 (decode once)", set, got)
				}
			}
			if db.hases("test-set-1") != 1 || db.hases("test-set-0") != 0 {
				t.Fatalf("HasTestCases calls = {test-set-0:%d, test-set-1:%d}, want {0, 1}",
					db.hases("test-set-0"), db.hases("test-set-1"))
			}
		})

		t.Run("trailing zero-byte and missing-dir sets skipped/"+mode, func(t *testing.T) {
			provenOn(t, on)
			h := newPartialRunHarness(t, 1, 0)
			root := t.TempDir()
			inner := testdb.NewWithFormat(zap.NewNop(), root, yaml.FormatJSON)
			writeRecordedHTTPTestCase(t, inner, "test-set-0", h.cases[0])
			// test-set-1: no tests directory.
			if err := os.MkdirAll(filepath.Join(root, "test-set-1"), 0o755); err != nil {
				t.Fatal(err)
			}
			// test-set-2: only a 0-byte test case file.
			if err := os.MkdirAll(filepath.Join(root, "test-set-2", "tests"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, "test-set-2", "tests", "test-1.yaml"), nil, 0o644); err != nil {
				t.Fatal(err)
			}
			db := newCountingTestDB(inner)

			reports := &setsReportDB{prReportDB: h.report}
			h.replayer.reportDB = reports
			h.replayer.testDB = db
			h.replayer.config.Command = "./app"
			h.replayer.config.CommandType = string(utils.Native)
			h.replayer.config.Test.MaxFlakyChecks = 1
			h.replayer.config.DisableANSI = true
			hooks := &lastCaseHooks{}
			h.replayer.hookImpl = hooks

			_, statuses := startEarlierSetRun(t, h, reports)
			if statuses["test-set-0"] != string(models.TestSetStatusPassed) ||
				statuses["test-set-1"] != string(models.TestSetStatusNoTestsToRun) ||
				statuses["test-set-2"] != string(models.TestSetStatusNoTestsToRun) {
				t.Fatalf("statuses = %v, want test-set-0 PASSED and test-set-1/2 NO_TESTS_TO_RUN", statuses)
			}
			if last, _ := hooks.seen(); !slices.Equal(last, []string{"test-set-0/test-1"}) {
				t.Fatalf("KEPLOY-LAST-TESTCASE sent on %v, want [test-set-0/test-1]", last)
			}
			for _, set := range []string{"test-set-0", "test-set-1", "test-set-2"} {
				if got := db.gets(set); got != 1 {
					t.Fatalf("GetTestCases(%s) called %d time(s), want 1 (decode once)", set, got)
				}
				if got := db.hases(set); got != 1 {
					t.Fatalf("HasTestCases(%s) called %d time(s), want 1", set, got)
				}
			}
		})

		t.Run("trailing wholly-ignored set skips HasTestCases/"+mode, func(t *testing.T) {
			provenOn(t, on)
			h := newPartialRunHarness(t, 1, 0)
			root := t.TempDir()
			inner := testdb.New(zap.NewNop(), root)
			writeRecordedHTTPTestCase(t, inner, "test-set-0", h.cases[0])
			writeRecordedHTTPTestCase(t, inner, "test-set-1", h.cases[0])
			db := newCountingTestDB(inner)

			reports := &setsReportDB{prReportDB: h.report}
			h.replayer.reportDB = reports
			h.replayer.testDB = db
			h.replayer.config.Command = "./app"
			h.replayer.config.CommandType = string(utils.Native)
			h.replayer.config.Test.MaxFlakyChecks = 1
			h.replayer.config.Test.IgnoredTests = map[string][]string{"test-set-1": {}}
			h.replayer.config.DisableANSI = true
			hooks := &lastCaseHooks{}
			h.replayer.hookImpl = hooks

			_, statuses := startEarlierSetRun(t, h, reports)
			if statuses["test-set-0"] != string(models.TestSetStatusPassed) || statuses["test-set-1"] != string(models.TestSetStatusIgnored) {
				t.Fatalf("statuses = %v, want test-set-0 PASSED and test-set-1 IGNORED", statuses)
			}
			if last, _ := hooks.seen(); !slices.Equal(last, []string{"test-set-0/test-1"}) {
				t.Fatalf("KEPLOY-LAST-TESTCASE sent on %v, want [test-set-0/test-1]", last)
			}
			for _, set := range []string{"test-set-0", "test-set-1"} {
				if got := db.gets(set); got != 1 {
					t.Fatalf("GetTestCases(%s) called %d time(s), want 1", set, got)
				}
			}
			if db.hases("test-set-1") != 0 || db.hases("test-set-0") != 1 {
				t.Fatalf("HasTestCases calls = {test-set-0:%d, test-set-1:%d}, want {1, 0}",
					db.hases("test-set-0"), db.hases("test-set-1"))
			}
		})
	}
}

// When a trailing test set has an unreadable or malformed test case (or an
// unreadable tests directory), Start must emit the testdb ERROR line ONCE
// (when RunTestSet reads it) rather than twice (pre-scan + RunTestSet), and
// must still count that set as running so the earlier set does not send
// KEPLOY-LAST-TESTCASE.
//
// Fails on main: every subtest emits 2 identical ERROR lines from testdb.
func TestStart_UnreadableTestCaseEmitsOneErrorLine(t *testing.T) {
	cases := []struct {
		name       string
		wantErrors []string
		unixOnly   bool
		breakSet1  func(t *testing.T, root string)
	}{
		{
			name:       "directory in place of test case file",
			wantErrors: []string{"failed to read the testcase"},
			breakSet1: func(t *testing.T, root string) {
				if err := os.MkdirAll(filepath.Join(root, "test-set-1", "tests", "test-1.yaml"), 0o755); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name:       "symlink escaping recording root",
			wantErrors: []string{"failed to read the testcase"},
			unixOnly:   true,
			breakSet1: func(t *testing.T, root string) {
				outside := filepath.Join(t.TempDir(), "outside.yaml")
				if err := os.WriteFile(outside, []byte("kind: Http\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				dir := filepath.Join(root, "test-set-1", "tests")
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, filepath.Join(dir, "test-1.yaml")); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name:       "malformed yaml test case",
			wantErrors: []string{"failed to unmarshal testcase data"},
			breakSet1: func(t *testing.T, root string) {
				dir := filepath.Join(root, "test-set-1", "tests")
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "test-1.yaml"), []byte(":::not valid yaml\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name:       "malformed json test case",
			wantErrors: []string{"failed to unmarshal json testcase data"},
			breakSet1: func(t *testing.T, root string) {
				dir := filepath.Join(root, "test-set-1", "tests")
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "test-1.json"), []byte("{bad json"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name:       "invalid testcase kind",
			wantErrors: []string{"invalid testcase kind", "failed to decode the testcase"},
			breakSet1: func(t *testing.T, root string) {
				dir := filepath.Join(root, "test-set-1", "tests")
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "test-1.yaml"), []byte("kind: Unknown\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name:       "regular file in place of tests directory",
			wantErrors: []string{"failed to open the directory containing yaml testcases"},
			breakSet1: func(t *testing.T, root string) {
				set := filepath.Join(root, "test-set-1")
				if err := os.MkdirAll(set, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(set, "tests"), []byte("not a dir"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name:       "symlink loop at tests directory",
			wantErrors: []string{"failed to look at the directory containing yaml testcases"},
			unixOnly:   true,
			breakSet1: func(t *testing.T, root string) {
				set := filepath.Join(root, "test-set-1")
				if err := os.MkdirAll(set, 0o755); err != nil {
					t.Fatal(err)
				}
				tests := filepath.Join(set, "tests")
				if err := os.Symlink(tests, tests); err != nil {
					t.Fatal(err)
				}
			},
		},
	}

	for _, on := range []bool{false, true} {
		for _, tc := range cases {
			name := tc.name
			if on {
				name += "/switch on"
			} else {
				name += "/switch off"
			}
			t.Run(name, func(t *testing.T) {
				if tc.unixOnly && runtime.GOOS == "windows" {
					t.Skip("symlinks require elevated privilege on Windows")
				}
				provenOn(t, on)
				h := newPartialRunHarness(t, 1, 0)
				root := t.TempDir()
				dbCore, dbLogs := observer.New(zap.ErrorLevel)
				inner := testdb.New(zap.New(dbCore), root)
				writeRecordedHTTPTestCase(t, inner, "test-set-0", h.cases[0])
				tc.breakSet1(t, root)
				db := newCountingTestDB(inner)

				reports := &setsReportDB{prReportDB: h.report}
				reports.statuses = map[string]string{}
				h.replayer.reportDB = reports
				h.replayer.testDB = db
				h.replayer.config.Command = "./app"
				h.replayer.config.CommandType = string(utils.Native)
				h.replayer.config.Test.MaxFlakyChecks = 1
				h.replayer.config.DisableANSI = true
				hooks := &lastCaseHooks{}
				h.replayer.hookImpl = hooks

				saved := utils.ErrCode
				t.Cleanup(func() { utils.ErrCode = saved })
				utils.ErrCode = 0

				ctx, cancel := context.WithTimeout(context.Background(), prTimeout)
				defer cancel()
				err := h.replayer.Start(ctx)
				if err == nil {
					t.Fatalf("Start succeeded on unreadable test-set-1; want error")
				}

				for _, wantErr := range tc.wantErrors {
					if n := dbLogs.FilterMessageSnippet(wantErr).Len(); n != 1 {
						t.Fatalf("testdb logged %q %d time(s), want 1 (all logs: %+v)", wantErr, n, dbLogs.All())
					}
				}
				if got := db.gets("test-set-1"); got != 1 {
					t.Fatalf("GetTestCases(test-set-1) called %d time(s), want 1", got)
				}
				// test-set-1 counts as running, so test-set-0 must NOT send KEPLOY-LAST-TESTCASE.
				if last, _ := hooks.seen(); len(last) != 0 {
					t.Fatalf("KEPLOY-LAST-TESTCASE sent on %v, want none (test-set-1 is the trailing unreadable set)", last)
				}
			})
		}
	}
}

// presenceOnlyDB implements TestCasePresenceReader and records whether
// GetTestCases was wrongly called during lastSetThatRuns.
type presenceOnlyDB struct {
	*prTestDB
	has      map[string]bool
	errs     map[string]error
	getCalls int
	hasCalls []string
}

func (d *presenceOnlyDB) GetTestCases(ctx context.Context, set string) ([]*models.TestCase, error) {
	d.getCalls++
	return d.prTestDB.GetTestCases(ctx, set)
}

func (d *presenceOnlyDB) HasTestCases(_ context.Context, set string) (bool, error) {
	d.hasCalls = append(d.hasCalls, set)
	if err := d.errs[set]; err != nil {
		return d.has[set], err
	}
	return d.has[set], nil
}

func (d *presenceOnlyDB) PresenceAppliesTo(db any) bool {
	return db == d
}

// promotedWrapperDB embeds *presenceOnlyDB without opting into
// PresenceAppliesTo itself, simulating a wrapper that inherits HasTestCases
// via Go struct embedding.
type promotedWrapperDB struct {
	*presenceOnlyDB
	getCalls []string
}

func (w *promotedWrapperDB) GetTestCases(ctx context.Context, set string) ([]*models.TestCase, error) {
	w.getCalls = append(w.getCalls, set)
	if err := w.errs[set]; err != nil {
		return nil, err
	}
	if !w.has[set] {
		return nil, nil
	}
	return w.prTestDB.GetTestCases(ctx, set)
}

// fallbackOnlyDB implements TestDB without TestCasePresenceReader, exercising
// setHasTestCases's GetTestCases fallback.
type fallbackOnlyDB struct {
	*prTestDB
	has      map[string]bool
	errs     map[string]error
	getCalls []string
}

func (d *fallbackOnlyDB) GetTestCases(ctx context.Context, set string) ([]*models.TestCase, error) {
	d.getCalls = append(d.getCalls, set)
	if err := d.errs[set]; err != nil {
		return nil, err
	}
	if !d.has[set] {
		return nil, nil
	}
	return d.prTestDB.GetTestCases(ctx, set)
}

// Unit rows for setHasTestCases / lastSetThatRuns covering both the
// TestCasePresenceReader fast path (including error cases) and the fallback
// path for stores that do not implement TestCasePresenceReader.
func TestLastSetThatRuns_PrefersHasTestCasesAndFallsBackToGetTestCases(t *testing.T) {
	rows := []struct {
		name      string
		ignored   map[string][]string
		has       map[string]bool
		errs      map[string]error
		want      int
		wantCalls []string
	}{
		{
			name:      "last set has cases",
			has:       map[string]bool{"s0": true, "s1": true, "s2": true},
			want:      2,
			wantCalls: []string{"s2"},
		},
		{
			name:      "trailing empty set skipped",
			has:       map[string]bool{"s0": true, "s1": true, "s2": false},
			want:      1,
			wantCalls: []string{"s2", "s1"},
		},
		{
			name:      "trailing ignored and empty skipped",
			ignored:   map[string][]string{"s2": {}},
			has:       map[string]bool{"s0": true, "s1": false},
			want:      0,
			wantCalls: []string{"s1", "s0"},
		},
		{
			name:      "trailing unreadable set counts as running (false, err)",
			has:       map[string]bool{"s0": true, "s1": true, "s2": false},
			errs:      map[string]error{"s2": errors.New("permission denied")},
			want:      2,
			wantCalls: []string{"s2"},
		},
		{
			name:      "all empty returns -1",
			has:       map[string]bool{"s0": false, "s1": false, "s2": false},
			want:      -1,
			wantCalls: []string{"s2", "s1", "s0"},
		},
	}

	for _, tc := range rows {
		t.Run("TestCasePresenceReader/"+tc.name, func(t *testing.T) {
			h := newPartialRunHarness(t, 1, 0)
			h.replayer.config.Test.IgnoredTests = tc.ignored
			db := &presenceOnlyDB{prTestDB: &prTestDB{cases: h.cases}, has: tc.has, errs: tc.errs}
			h.replayer.testDB = db

			got := h.replayer.lastSetThatRuns(context.Background(), []string{"s0", "s1", "s2"})
			if got != tc.want {
				t.Fatalf("lastSetThatRuns = %d, want %d", got, tc.want)
			}
			if db.getCalls != 0 {
				t.Fatalf("GetTestCases called %d time(s), want 0 when TestCasePresenceReader is implemented", db.getCalls)
			}
			if !slices.Equal(db.hasCalls, tc.wantCalls) {
				t.Fatalf("HasTestCases called on %v, want %v", db.hasCalls, tc.wantCalls)
			}
		})

		t.Run("GetTestCases fallback/"+tc.name, func(t *testing.T) {
			h := newPartialRunHarness(t, 1, 0)
			h.replayer.config.Test.IgnoredTests = tc.ignored
			db := &fallbackOnlyDB{prTestDB: &prTestDB{cases: h.cases}, has: tc.has, errs: tc.errs}
			h.replayer.testDB = db

			got := h.replayer.lastSetThatRuns(context.Background(), []string{"s0", "s1", "s2"})
			if got != tc.want {
				t.Fatalf("lastSetThatRuns = %d, want %d", got, tc.want)
			}
			if !slices.Equal(db.getCalls, tc.wantCalls) {
				t.Fatalf("GetTestCases called on %v, want %v", db.getCalls, tc.wantCalls)
			}
		})

		t.Run("promoted wrapper rejected/"+tc.name, func(t *testing.T) {
			h := newPartialRunHarness(t, 1, 0)
			h.replayer.config.Test.IgnoredTests = tc.ignored
			inner := &presenceOnlyDB{prTestDB: &prTestDB{cases: h.cases}, has: tc.has, errs: tc.errs}
			wrap := &promotedWrapperDB{presenceOnlyDB: inner}
			h.replayer.testDB = wrap

			got := h.replayer.lastSetThatRuns(context.Background(), []string{"s0", "s1", "s2"})
			if got != tc.want {
				t.Fatalf("lastSetThatRuns = %d, want %d", got, tc.want)
			}
			if len(inner.hasCalls) != 0 {
				t.Fatalf("HasTestCases called on %v for un-opted-in embedded wrapper, want none", inner.hasCalls)
			}
			if !slices.Equal(wrap.getCalls, tc.wantCalls) {
				t.Fatalf("GetTestCases called on %v, want %v", wrap.getCalls, tc.wantCalls)
			}
		})
	}
}

func writeNamedRecordedHTTPTestCase(t *testing.T, db *testdb.TestYaml, set, name string, base *models.TestCase) {
	t.Helper()
	cp := *base
	cp.Name = name
	if err := db.InsertTestCase(context.Background(), &cp, set, false); err != nil {
		t.Fatalf("InsertTestCase(%s/%s): %v", set, name, err)
	}
}

// Plain *testdb.TestYaml (with no test wrapper) must satisfy presenceReaderOf,
// leave r.prescanned nil, and emit a single ERROR line on an unreadable set.
func TestStart_BareTestYamlUsesHasTestCasesAndEmitsOneError(t *testing.T) {
	h := newPartialRunHarness(t, 1, 0)
	root := t.TempDir()
	dbCore, dbLogs := observer.New(zap.ErrorLevel)
	db := testdb.New(zap.New(dbCore), root)
	writeNamedRecordedHTTPTestCase(t, db, "test-set-0", "set0-case", h.cases[0])
	if err := os.MkdirAll(filepath.Join(root, "test-set-1", "tests", "test-1.yaml"), 0o755); err != nil {
		t.Fatal(err)
	}

	if presenceReaderOf(db) == nil {
		t.Fatal("presenceReaderOf(*testdb.TestYaml) = nil, want non-nil")
	}

	reports := &setsReportDB{prReportDB: h.report}
	reports.statuses = map[string]string{}
	h.replayer.reportDB = reports
	h.replayer.testDB = db
	h.replayer.config.Command = "./app"
	h.replayer.config.CommandType = string(utils.Native)
	h.replayer.config.Test.MaxFlakyChecks = 1
	h.replayer.config.DisableANSI = true

	saved := utils.ErrCode
	t.Cleanup(func() { utils.ErrCode = saved })
	utils.ErrCode = 0

	ctx, cancel := context.WithTimeout(context.Background(), prTimeout)
	defer cancel()
	if err := h.replayer.Start(ctx); err == nil {
		t.Fatal("Start succeeded on unreadable test-set-1; want error")
	}
	if n := dbLogs.FilterMessageSnippet("failed to read the testcase").Len(); n != 1 {
		t.Fatalf("bare *TestYaml logged failed to read the testcase %d time(s), want 1 (all: %+v)", n, dbLogs.All())
	}
	if h.replayer.prescanned != nil {
		t.Fatalf("bare *TestYaml populated r.prescanned = %+v, want nil", h.replayer.prescanned)
	}
}

// autoReplayLikeDB mirrors k8s-proxy's 1-level wrapper (*autoReplayTestDB)
// embedding *testdb.TestYaml.
type autoReplayLikeDB struct {
	*testdb.TestYaml
	mu       sync.Mutex
	getCalls map[string]int
}

func (a *autoReplayLikeDB) GetTestCases(ctx context.Context, set string) ([]*models.TestCase, error) {
	a.mu.Lock()
	a.getCalls[set]++
	a.mu.Unlock()
	return a.TestYaml.GetTestCases(ctx, set)
}

func (a *autoReplayLikeDB) gets(set string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.getCalls[set]
}

// replayerFilterLikeDB mirrors k8s-proxy's 2-level wrapper (replayerTestDB)
// embedding *autoReplayTestDB and filtering out unreplayable cases in
// GetTestCases. It does NOT override HasTestCases or PresenceAppliesTo.
type replayerFilterLikeDB struct {
	*autoReplayLikeDB
	dropSet string
}

func (r replayerFilterLikeDB) GetTestCases(ctx context.Context, set string) ([]*models.TestCase, error) {
	tcs, err := r.autoReplayLikeDB.GetTestCases(ctx, set)
	if err != nil {
		return nil, err
	}
	if set == r.dropSet {
		return nil, nil
	}
	return tcs, nil
}

// A 2-level wrapper embedding *testdb.TestYaml (k8s-proxy's replayerTestDB
// shape) whose GetTestCases filters out all cases of a trailing set must NOT
// silently inherit *TestYaml's HasTestCases shortcut: lastSetThatRuns must
// pick the set before it (as main does), KEPLOY-LAST-TESTCASE must land on
// that set, and the fallback cache must still read every set exactly once.
func TestStart_EmbeddedFilteringWrapperDoesNotInheritHasTestCases(t *testing.T) {
	for _, on := range []bool{false, true} {
		for _, asPointer := range []bool{false, true} {
			name := "value wrapper"
			if asPointer {
				name = "pointer wrapper"
			}
			if on {
				name += "/switch on"
			} else {
				name += "/switch off"
			}
			t.Run(name, func(t *testing.T) {
				provenOn(t, on)
				h := newPartialRunHarness(t, 1, 0)
				root := t.TempDir()
				inner := testdb.New(zap.NewNop(), root)
				writeNamedRecordedHTTPTestCase(t, inner, "test-set-0", "set0-case", h.cases[0])
				writeNamedRecordedHTTPTestCase(t, inner, "test-set-1", "set1-case", h.cases[0])
				writeNamedRecordedHTTPTestCase(t, inner, "test-set-2", "set2-case", h.cases[0])

				auto := &autoReplayLikeDB{TestYaml: inner, getCalls: make(map[string]int)}
				wrapVal := replayerFilterLikeDB{autoReplayLikeDB: auto, dropSet: "test-set-2"}
				var db TestDB = wrapVal
				if asPointer {
					db = &wrapVal
				}

				if presenceReaderOf(db) != nil {
					t.Fatalf("presenceReaderOf(%T) != nil; embedded wrapper must not inherit HasTestCases", db)
				}

				reports := &setsReportDB{prReportDB: h.report}
				h.replayer.reportDB = reports
				h.replayer.testDB = db
				h.replayer.config.Command = "./app"
				h.replayer.config.CommandType = string(utils.Native)
				h.replayer.config.Test.MaxFlakyChecks = 1
				h.replayer.config.DisableANSI = true
				hooks := &lastCaseHooks{}
				h.replayer.hookImpl = hooks

				_, statuses := startEarlierSetRun(t, h, reports)
				if statuses["test-set-0"] != string(models.TestSetStatusPassed) ||
					statuses["test-set-1"] != string(models.TestSetStatusPassed) ||
					statuses["test-set-2"] != string(models.TestSetStatusNoTestsToRun) {
					t.Fatalf("statuses = %v, want test-set-0/1 PASSED and test-set-2 NO_TESTS_TO_RUN", statuses)
				}
				if last, _ := hooks.seen(); !slices.Equal(last, []string{"test-set-1/set1-case"}) {
					t.Fatalf("KEPLOY-LAST-TESTCASE sent on %v, want [test-set-1/set1-case]", last)
				}
				for _, set := range []string{"test-set-0", "test-set-1", "test-set-2"} {
					if got := auto.gets(set); got != 1 {
						t.Fatalf("GetTestCases(%s) called %d time(s), want 1 (fallback must reuse pre-scanned set)", set, got)
					}
				}
				if h.replayer.prescanned != nil || len(h.replayer.prescannedEmpty) != 0 {
					t.Fatalf("Start left prescanned state behind: prescanned=%+v empty=%v", h.replayer.prescanned, h.replayer.prescannedEmpty)
				}
			})
		}
	}
}

// apiServerLikeDB implements TestDB only (no TestCasePresenceReader), matching
// k8s-proxy's api-server-backed store. Each set can hold distinct cases, be
// empty, return a logged error, or return different cases on a retry call.
type apiServerLikeDB struct {
	mu       sync.Mutex
	ids      []string
	bySet    map[string][][]*models.TestCase // per-call slices for flaky retry tests
	errBySet map[string]error
	logger   *zap.Logger
	getCalls map[string]int
}

func (a *apiServerLikeDB) GetAllTestSetIDs(context.Context) ([]string, error) {
	return a.ids, nil
}

func (a *apiServerLikeDB) GetTestCases(_ context.Context, set string) ([]*models.TestCase, error) {
	a.mu.Lock()
	callIdx := a.getCalls[set]
	a.getCalls[set] = callIdx + 1
	err := a.errBySet[set]
	seq := a.bySet[set]
	logger := a.logger
	a.mu.Unlock()

	if err != nil {
		if logger != nil {
			logger.Error("failed to fetch test cases from store", zap.String("testSet", set), zap.Error(err))
		}
		return nil, err
	}
	if len(seq) == 0 {
		return nil, nil
	}
	if callIdx >= len(seq) {
		callIdx = len(seq) - 1
	}
	// Return fresh copies so per-run mutations (e.g. IsLast) don't alias across attempts.
	src := seq[callIdx]
	out := make([]*models.TestCase, len(src))
	for i, tc := range src {
		cp := *tc
		out[i] = &cp
	}
	return out, nil
}

func (a *apiServerLikeDB) UpdateTestCase(context.Context, *models.TestCase, string, bool) error {
	return nil
}
func (a *apiServerLikeDB) DeleteTests(context.Context, string, []string) error { return nil }
func (a *apiServerLikeDB) DeleteTestSet(context.Context, string) error         { return nil }

func (a *apiServerLikeDB) gets(set string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.getCalls[set]
}

// recordingCaseHooks records every "set/case" simulated and which carried
// KEPLOY-LAST-TESTCASE, and can optionally fail a named test case to drive a
// flaky retry.
type recordingCaseHooks struct {
	lastCaseHooks
	failCase string
	rmu      sync.Mutex
	ran      []string
}

func (h *recordingCaseHooks) SimulateRequest(ctx context.Context, tc *models.TestCase, set string) (interface{}, error) {
	h.rmu.Lock()
	h.ran = append(h.ran, set+"/"+tc.Name)
	fail := h.failCase != "" && tc.Name == h.failCase
	h.rmu.Unlock()
	if fail {
		bad := *tc
		bad.HTTPResp.StatusCode = 500
		return &bad.HTTPResp, nil
	}
	return h.lastCaseHooks.SimulateRequest(ctx, tc, set)
}

func (h *recordingCaseHooks) ranCases() []string {
	h.rmu.Lock()
	defer h.rmu.Unlock()
	return slices.Clone(h.ran)
}

func namedPRCase(base *models.TestCase, name string) *models.TestCase {
	cp := *base
	cp.Name = name
	return &cp
}

// A fallback store that does not implement TestCasePresenceReader (such as
// k8s-proxy's api-server store) must still fetch/decode every set once during
// Start, never mix cases across sets, emit a single ERROR line on an unreadable
// trailing set, and read fresh on a flaky-check retry attempt.
func TestStart_FallbackStoreDecodesOnceAndReusesSettledSet(t *testing.T) {
	for _, on := range []bool{false, true} {
		mode := "switch off"
		if on {
			mode = "switch on"
		}

		t.Run("every set runs/"+mode, func(t *testing.T) {
			provenOn(t, on)
			h := newPartialRunHarness(t, 1, 0)
			db := &apiServerLikeDB{
				ids: []string{"test-set-0", "test-set-1"},
				bySet: map[string][][]*models.TestCase{
					"test-set-0": {{namedPRCase(h.cases[0], "set0-case")}},
					"test-set-1": {{namedPRCase(h.cases[0], "set1-case")}},
				},
				getCalls: make(map[string]int),
			}
			reports := &setsReportDB{prReportDB: h.report}
			h.replayer.reportDB = reports
			h.replayer.testDB = db
			h.replayer.config.Command = "./app"
			h.replayer.config.CommandType = string(utils.Native)
			h.replayer.config.Test.MaxFlakyChecks = 1
			h.replayer.config.DisableANSI = true
			hooks := &recordingCaseHooks{}
			h.replayer.hookImpl = hooks

			_, statuses := startEarlierSetRun(t, h, reports)
			if statuses["test-set-0"] != string(models.TestSetStatusPassed) || statuses["test-set-1"] != string(models.TestSetStatusPassed) {
				t.Fatalf("statuses = %v, want both PASSED", statuses)
			}
			if got := hooks.ranCases(); !slices.Equal(got, []string{"test-set-0/set0-case", "test-set-1/set1-case"}) {
				t.Fatalf("ran cases = %v, want [test-set-0/set0-case test-set-1/set1-case]", got)
			}
			if last, _ := hooks.seen(); !slices.Equal(last, []string{"test-set-1/set1-case"}) {
				t.Fatalf("KEPLOY-LAST-TESTCASE sent on %v, want [test-set-1/set1-case]", last)
			}
			for _, set := range []string{"test-set-0", "test-set-1"} {
				if got := db.gets(set); got != 1 {
					t.Fatalf("GetTestCases(%s) called %d time(s), want 1 (fallback must not double-fetch)", set, got)
				}
			}
			if h.replayer.prescanned != nil {
				t.Fatalf("r.prescanned not cleared after Start: %+v", h.replayer.prescanned)
			}
		})

		t.Run("trailing empty set fetched once/"+mode, func(t *testing.T) {
			provenOn(t, on)
			h := newPartialRunHarness(t, 1, 0)
			db := &apiServerLikeDB{
				ids: []string{"test-set-0", "test-set-1", "test-set-2"},
				bySet: map[string][][]*models.TestCase{
					"test-set-0": {{namedPRCase(h.cases[0], "set0-case")}},
					"test-set-1": {{namedPRCase(h.cases[0], "set1-case")}},
					"test-set-2": nil,
				},
				getCalls: make(map[string]int),
			}
			reports := &setsReportDB{prReportDB: h.report}
			h.replayer.reportDB = reports
			h.replayer.testDB = db
			h.replayer.config.Command = "./app"
			h.replayer.config.CommandType = string(utils.Native)
			h.replayer.config.Test.MaxFlakyChecks = 1
			h.replayer.config.DisableANSI = true
			hooks := &recordingCaseHooks{}
			h.replayer.hookImpl = hooks

			_, statuses := startEarlierSetRun(t, h, reports)
			if statuses["test-set-0"] != string(models.TestSetStatusPassed) ||
				statuses["test-set-1"] != string(models.TestSetStatusPassed) ||
				statuses["test-set-2"] != string(models.TestSetStatusNoTestsToRun) {
				t.Fatalf("statuses = %v, want test-set-0/1 PASSED and test-set-2 NO_TESTS_TO_RUN", statuses)
			}
			if last, _ := hooks.seen(); !slices.Equal(last, []string{"test-set-1/set1-case"}) {
				t.Fatalf("KEPLOY-LAST-TESTCASE sent on %v, want [test-set-1/set1-case]", last)
			}
			for _, set := range []string{"test-set-0", "test-set-1", "test-set-2"} {
				if got := db.gets(set); got != 1 {
					t.Fatalf("GetTestCases(%s) called %d time(s), want 1", set, got)
				}
			}
		})

		t.Run("trailing unreadable set logs once/"+mode, func(t *testing.T) {
			provenOn(t, on)
			h := newPartialRunHarness(t, 1, 0)
			dbCore, dbLogs := observer.New(zap.ErrorLevel)
			db := &apiServerLikeDB{
				ids: []string{"test-set-0", "test-set-1"},
				bySet: map[string][][]*models.TestCase{
					"test-set-0": {{namedPRCase(h.cases[0], "set0-case")}},
				},
				errBySet: map[string]error{
					"test-set-1": errors.New("backend read failed"),
				},
				logger:   zap.New(dbCore),
				getCalls: make(map[string]int),
			}
			reports := &setsReportDB{prReportDB: h.report}
			reports.statuses = map[string]string{}
			h.replayer.reportDB = reports
			h.replayer.testDB = db
			h.replayer.config.Command = "./app"
			h.replayer.config.CommandType = string(utils.Native)
			h.replayer.config.Test.MaxFlakyChecks = 1
			h.replayer.config.DisableANSI = true
			hooks := &recordingCaseHooks{}
			h.replayer.hookImpl = hooks

			saved := utils.ErrCode
			t.Cleanup(func() { utils.ErrCode = saved })
			utils.ErrCode = 0

			ctx, cancel := context.WithTimeout(context.Background(), prTimeout)
			defer cancel()
			if err := h.replayer.Start(ctx); err == nil {
				t.Fatal("Start succeeded on failing test-set-1; want error")
			}
			if got := db.gets("test-set-1"); got != 1 {
				t.Fatalf("GetTestCases(test-set-1) called %d time(s), want 1", got)
			}
			if n := dbLogs.FilterMessageSnippet("failed to fetch test cases from store").Len(); n != 1 {
				t.Fatalf("fallback store logged error %d time(s), want 1 (all: %+v)", n, dbLogs.All())
			}
			if last, _ := hooks.seen(); len(last) != 0 {
				t.Fatalf("KEPLOY-LAST-TESTCASE sent on %v, want none (test-set-1 counts as running)", last)
			}
			if h.replayer.prescanned != nil {
				t.Fatalf("r.prescanned not cleared after failing Start: %+v", h.replayer.prescanned)
			}
		})

		t.Run("flaky retry reads fresh on second attempt/"+mode, func(t *testing.T) {
			provenOn(t, on)
			h := newPartialRunHarness(t, 1, 0)
			db := &apiServerLikeDB{
				ids: []string{"test-set-0", "test-set-1"},
				bySet: map[string][][]*models.TestCase{
					"test-set-0": {{namedPRCase(h.cases[0], "set0-case")}},
					"test-set-1": {
						{namedPRCase(h.cases[0], "set1-v1")},
						{namedPRCase(h.cases[0], "set1-v2")},
					},
				},
				getCalls: make(map[string]int),
			}
			reports := &setsReportDB{prReportDB: h.report}
			h.replayer.reportDB = reports
			h.replayer.testDB = db
			h.replayer.config.Command = "./app"
			h.replayer.config.CommandType = string(utils.Native)
			h.replayer.config.Test.MaxFlakyChecks = 2
			h.replayer.config.DisableANSI = true
			hooks := &recordingCaseHooks{failCase: "set1-v1"}
			h.replayer.hookImpl = hooks

			_, statuses := startEarlierSetRun(t, h, reports)
			if statuses["test-set-1"] != string(models.TestSetStatusPassed) {
				t.Fatalf("test-set-1 status = %q, want PASSED on attempt 2 (fresh read)", statuses["test-set-1"])
			}
			// Attempt 1 reuses pre-scanned set1-v1 (1st call); attempt 2 reads fresh set1-v2 (2nd call).
			if got := db.gets("test-set-1"); got != 2 {
				t.Fatalf("GetTestCases(test-set-1) called %d time(s) across 2 attempts, want 2 (1 reused + 1 fresh)", got)
			}
			wantRan := []string{"test-set-0/set0-case", "test-set-0/set0-case", "test-set-1/set1-v1", "test-set-1/set1-v2"}
			if got := hooks.ranCases(); !slices.Equal(got, wantRan) {
				t.Fatalf("ran cases = %v, want %v (attempt 1 set1-v1 then attempt 2 fresh set1-v2)", got, wantRan)
			}
		})
	}
}
