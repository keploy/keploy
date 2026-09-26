package mock

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/safeyaml"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
	"golang.org/x/sync/errgroup"
)

// runWrites is the fake agent with one addition: while the "test command"
// runs, it writes what a real runner would -- a coverage report.
type runWrites struct {
	*composeInstr
	onRun    func()
	setupErr error
}

func (r *runWrites) Setup(ctx context.Context, cmd string, opts models.SetupOptions) error {
	if r.setupErr != nil {
		return r.setupErr
	}
	return r.composeInstr.Setup(ctx, cmd, opts)
}

func (r *runWrites) Run(ctx context.Context, opts models.RunOptions) models.AppError {
	if r.onRun != nil {
		r.onRun()
	}
	return r.composeInstr.Run(ctx, opts)
}

// A cover profile with 3 of 4 statements covered: 75%.
const profile75 = "mode: set\nex/a.go:1.1,2.2 3 1\nex/a.go:3.1,4.2 1 0\n"

type replayCase struct {
	runExit    models.AppError
	mockErrs   []models.UnmatchedCall
	mockErrErr error
	onMiss     string
	minCov     float64
	covReport  string // written by the "runner" as coverage.out when non-empty
	staleCov   bool   // write it BEFORE the run instead, as an hour-old leftover
	prevRun    bool   // write it BEFORE the run, a moment ago, as the previous run would
	// rewriteLaunch rewrites cfg.Command after construction, as native
	// instrumentation does.
	rewriteLaunch bool
	// noneServed: the agent served no recorded call (the default serves one).
	noneServed bool
	bypass     []models.BypassRule
	setupErr   error
	// keployFails: the run fails on keploy's side, and Replay returns that
	// failure as its own error rather than only an exit code.
	keployFails bool
}

// replayIn runs one replay against a real keploy/ directory in a temp repo
// and returns that directory, the exit code and the log.
func replayIn(t *testing.T, c replayCase) (string, int, *observer.ObservedLogs) {
	t.Helper()
	repo := t.TempDir()
	t.Chdir(repo)
	keployDir := filepath.Join(repo, "keploy")
	if err := os.MkdirAll(filepath.Join(keployDir, "set"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(keployDir, "set", "mocks.yaml"), []byte("version: api.keploy.io/v1beta1\nkind: Http\nname: mock-0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if c.covReport != "" && (c.staleCov || c.prevRun) {
		p := filepath.Join(repo, "coverage.out")
		if err := os.WriteFile(p, []byte(c.covReport), 0o644); err != nil {
			t.Fatal(err)
		}
		if c.staleCov {
			old := time.Now().Add(-time.Hour)
			_ = os.Chtimes(p, old, old)
		}
	}

	exit := c.runExit
	if exit.AppErrorType == "" {
		exit = models.AppError{AppErrorType: models.ErrAppStopped}
	}
	base := newInstr(t, agentUpFromSetup, false, exit)
	base.mockErrors = c.mockErrs
	base.mockErrorsErr = c.mockErrErr
	if !c.noneServed {
		base.consumedMocks = []models.MockState{{Name: "mock-0"}}
	}
	instr := &runWrites{composeInstr: base, setupErr: c.setupErr, onRun: func() {
		if c.covReport != "" && !c.staleCov && !c.prevRun {
			_ = os.WriteFile(filepath.Join(repo, "coverage.out"), []byte(c.covReport), 0o644)
		}
	}}

	cfg := instrConfig(base, utils.Native, "go test ./...")
	cfg.Path = keployDir
	cfg.Mock.OnMiss = c.onMiss
	if cfg.Mock.OnMiss == "" {
		cfg.Mock.OnMiss = "fail"
	}
	cfg.Mock.MinCoverage = c.minCov
	cfg.BypassRules = c.bypass

	utils.ErrCode = 0
	t.Cleanup(func() { utils.ErrCode = 0 })

	core, logs := observer.New(zapcore.DebugLevel)
	svc := New(zap.New(core), instr, stubMockDB{}, nil, nil, nil, cfg)
	// What a native macOS build does between constructing the service and
	// running it: prefix the launch line with the interception shim.
	if c.rewriteLaunch {
		cfg.Command = "DYLD_INSERT_LIBRARIES='/tmp/keploy-native-1/keploy_shim.dylib' " + cfg.Command
	}
	err := svc.Replay(context.Background())
	if err != nil && c.setupErr == nil && !c.keployFails {
		t.Fatalf("Replay: %v", err)
	}
	if err == nil && c.keployFails {
		t.Fatal("Replay returned no error for a run keploy itself did not complete: its exit code alone reads as the test command's")
	}
	return keployDir, utils.ErrCode, logs
}

func mustReceipt(t *testing.T, keployDir string) *Receipt {
	t.Helper()
	r, err := ReadReceipt(keployDir, "set")
	if err != nil || r == nil {
		t.Fatalf("no receipt: %v", err)
	}
	return r
}

// The receipt is the replay's verdict outliving the terminal it scrolled past
// in: what ran, how, what it proved, and which recording it proved it about.
func TestReplayWritesAReceipt(t *testing.T) {
	keployDir, code, _ := replayIn(t, replayCase{covReport: profile75})
	if code != 0 {
		t.Fatalf("exit %d, want 0", code)
	}
	r := mustReceipt(t, keployDir)
	if r.Set != "set" || r.Command != "go test ./..." || r.OnMiss != "fail" || r.ExitCode != 0 || r.Missed != 0 {
		t.Fatalf("receipt does not describe the run: %+v", r)
	}
	if r.MocksDigest == "" || r.MocksDigest != MocksDigest(keployDir, "set") {
		t.Fatalf("receipt digest %q does not name the recording replayed (%q)", r.MocksDigest, MocksDigest(keployDir, "set"))
	}
	if !r.Isolated || r.IsolationNote != "" {
		t.Fatalf("a passing --on-miss fail run that served its recording with nothing missed is isolated: %+v", r)
	}
	if r.RunnerExitCode != 0 || r.FailedBy != "" {
		t.Fatalf("runner exit %d, failedBy %q for a passing run", r.RunnerExitCode, r.FailedBy)
	}
	if r.Coverage == nil || r.Coverage.Covered != 3 || r.Coverage.Total != 4 || r.Coverage.Source != "coverage.out" {
		t.Fatalf("coverage %+v, want 3 of 4 statements from coverage.out", r.Coverage)
	}
	// Local, so it is git-ignored the moment it exists.
	gi, err := os.ReadFile(filepath.Join(keployDir, ".gitignore"))
	if err != nil || !strings.Contains(string(gi), "/*/last-replay.yaml") {
		t.Fatalf("keploy/.gitignore does not ignore receipts: %q (%v)", gi, err)
	}
}

// Isolation is what the run PROVED, so each way of not proving it must read
// as not isolated -- including the one that looks clean: a miss list nobody
// could read.
func TestReceiptIsolation(t *testing.T) {
	for _, tc := range []struct {
		name string
		c    replayCase
	}{
		{"the runner failed", replayCase{runExit: models.AppError{AppErrorType: models.ErrCommandError, ExitCode: 2}}},
		{"calls could reach real services", replayCase{onMiss: "passthrough"}},
		{"a call was missed", replayCase{mockErrs: []models.UnmatchedCall{{Protocol: "http", Destination: "dep:80"}}}},
		{"the miss list was never read", replayCase{mockErrErr: errors.New("connection refused")}},
		// A run keploy never intercepted also passes with nothing missed: on
		// macOS `npm test` does, because npm's script shell drops the shim.
		// Serving at least one recorded call is the evidence of interception.
		{"no recorded call was served", replayCase{noneServed: true}},
		// Bypassed destinations are dialled for real, so they never miss.
		{"a destination bypasses keploy", replayCase{bypass: []models.BypassRule{{Port: 5432}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			keployDir, _, _ := replayIn(t, tc.c)
			r := mustReceipt(t, keployDir)
			if r.Isolated {
				t.Fatalf("receipt claims isolation: %+v", r)
			}
			if r.IsolationNote == "" {
				t.Fatal("a run that is not isolated must say why")
			}
		})
	}
	t.Run("unknown counts are -1, not 0", func(t *testing.T) {
		keployDir, _, _ := replayIn(t, replayCase{mockErrErr: errors.New("connection refused")})
		if r := mustReceipt(t, keployDir); r.Missed != -1 {
			t.Fatalf("missed = %d, want -1 for a read that failed", r.Missed)
		}
	})
}

func TestMinCoverage(t *testing.T) {
	for _, tc := range []struct {
		name     string
		c        replayCase
		wantExit int
		wantLog  string
	}{
		{"above the floor passes", replayCase{minCov: 70, covReport: profile75}, 0, "offline coverage"},
		{"below the floor fails", replayCase{minCov: 80, covReport: profile75}, 1, "less of the code than the floor"},
		// A floor that passes whenever it cannot measure is a floor nobody has to clear.
		{"no report fails", replayCase{minCov: 10}, 1, "no coverage report to check"},
		// Yesterday's report is not this run's number.
		{"a leftover report fails", replayCase{minCov: 10, covReport: profile75, staleCov: true}, 1, "no coverage report to check"},
		// The case a timestamp window got wrong on a real Linux run: the
		// previous replay wrote coverage.out a second before this one started,
		// and this run -- which wrote no report at all -- was credited with it.
		{"the previous run's report fails", replayCase{minCov: 10, covReport: profile75, prevRun: true}, 1, "no coverage report to check"},
		// The runner already failed the build; its code is the one to keep.
		{"a failing runner keeps its own code", replayCase{minCov: 80, covReport: profile75,
			runExit: models.AppError{AppErrorType: models.ErrCommandError, ExitCode: 3}}, 3, ""},
		{"no floor, no report: nothing to enforce", replayCase{}, 0, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			keployDir, code, logs := replayIn(t, tc.c)
			if code != tc.wantExit {
				t.Fatalf("exit %d, want %d", code, tc.wantExit)
			}
			if tc.wantLog != "" && logs.FilterMessageSnippet(tc.wantLog).Len() == 0 {
				var all []string
				for _, e := range logs.All() {
					all = append(all, e.Message)
				}
				t.Fatalf("no log mentioning %q; got:\n%s", tc.wantLog, strings.Join(all, "\n"))
			}
			// The receipt carries the exit the gate produced, so a reader
			// never sees a green receipt for a red run.
			r := mustReceipt(t, keployDir)
			if r.ExitCode != tc.wantExit {
				t.Fatalf("receipt exit %d, want %d", r.ExitCode, tc.wantExit)
			}
			// ...and says WHICH check failed it. A gate failure is not a
			// failing test, and it does not un-prove the isolation.
			if tc.wantExit == 1 && tc.c.runExit.AppErrorType == "" {
				if r.FailedBy != FailedByMinCoverage || r.RunnerExitCode != 0 || !r.Isolated {
					t.Fatalf("gate failure recorded as failedBy=%q runner=%d isolated=%v", r.FailedBy, r.RunnerExitCode, r.Isolated)
				}
			}
			if tc.c.runExit.ExitCode != 0 && (r.FailedBy != FailedByRunner || r.RunnerExitCode != tc.c.runExit.ExitCode) {
				t.Fatalf("runner failure recorded as failedBy=%q runner=%d", r.FailedBy, r.RunnerExitCode)
			}
		})
	}
}

// The terminal says what the run proved, whether or not a coverage report was
// written -- the verdict used to exist only in the receipt.
func TestTheReplaySaysWhatItProved(t *testing.T) {
	_, _, logs := replayIn(t, replayCase{})
	if logs.FilterMessageSnippet("ran with every dependency answered from the recording").Len() != 1 {
		t.Fatal("an isolated replay never said so")
	}
	_, _, logs = replayIn(t, replayCase{onMiss: "passthrough"})
	if logs.FilterMessageSnippet("did not prove the tests run with the dependencies off").Len() != 1 {
		t.Fatal("a replay that proved nothing claimed to, or said nothing at all")
	}
	if logs.FilterMessageSnippet("ran with every dependency answered from the recording").Len() != 0 {
		t.Fatal("a passthrough replay claimed every dependency came from the recording")
	}
}

// Coverage from a run that could reach real services is not offline coverage,
// and the log must not say it is.
func TestCoverageIsOnlyOfflineWhenIsolated(t *testing.T) {
	_, _, logs := replayIn(t, replayCase{onMiss: "passthrough", covReport: profile75})
	if logs.FilterMessageSnippet("offline coverage").Len() != 0 {
		t.Fatal("a passthrough run's coverage was called offline coverage")
	}
	if logs.FilterMessageSnippet("test coverage of this replay").Len() != 1 {
		t.Fatal("the passthrough run's coverage was not reported")
	}
}

// keploy mock runs as root under sudo on Linux, inside a repository that may
// have just been cloned. A link planted at the receipt's path must not be
// followed into a root-owned write somewhere else. (Defence in depth: the test
// command itself also runs as root there.)
func TestReceiptRefusesASymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no O_NOFOLLOW on Windows")
	}
	outside := filepath.Join(t.TempDir(), "victim")
	if err := os.WriteFile(outside, []byte("untouched"), 0o644); err != nil {
		t.Fatal(err)
	}
	keployDir := filepath.Join(t.TempDir(), "keploy")
	if err := os.MkdirAll(filepath.Join(keployDir, "set"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(keployDir, "set", ReceiptFile)); err != nil {
		t.Fatal(err)
	}
	writeReceipt(zap.NewNop(), keployDir, Receipt{Set: "set", OnMiss: "fail"})
	if b, _ := os.ReadFile(outside); string(b) != "untouched" {
		t.Fatalf("the receipt was written through a symlink: %q", b)
	}
	// The link is replaced by the receipt, not followed.
	if info, err := os.Lstat(filepath.Join(keployDir, "set", ReceiptFile)); err != nil || !info.Mode().IsRegular() {
		t.Fatalf("the receipt path is not a regular file after the write: %v", err)
	}

	// A set directory that is itself a link is not written through either.
	realDir := t.TempDir()
	if err := os.Symlink(realDir, filepath.Join(keployDir, "linked")); err != nil {
		t.Fatal(err)
	}
	writeReceipt(zap.NewNop(), keployDir, Receipt{Set: "linked"})
	if _, err := os.Stat(filepath.Join(realDir, ReceiptFile)); err == nil {
		t.Fatal("the receipt was written through a symlinked set directory")
	}
}

// Nothing to replay means no receipt: writing one would invent a set.
func TestNoReceiptForAMissingSet(t *testing.T) {
	keployDir := filepath.Join(t.TempDir(), "keploy")
	writeReceipt(zap.NewNop(), keployDir, Receipt{Set: "nope"})
	if _, err := os.Stat(filepath.Join(keployDir, "nope")); err == nil {
		t.Fatal("a receipt created a set directory that did not exist")
	}
	if r, err := ReadReceipt(keployDir, "nope"); r != nil || err != nil {
		t.Fatalf("ReadReceipt on a never-replayed set = %+v, %v; want nil, nil", r, err)
	}
}

// The receipt names the command the USER runs. A build that instruments
// natively rewrites the launch line after the service is built; recording that
// line leaked a temp path into the receipt, and status then told agents to run
// it.
func TestReceiptRecordsTheUsersCommandNotTheLaunchLine(t *testing.T) {
	keployDir, _, logs := replayIn(t, replayCase{rewriteLaunch: true})
	if r := mustReceipt(t, keployDir); r.Command != "go test ./..." {
		t.Fatalf("receipt command = %q, want the user's %q", r.Command, "go test ./...")
	}
	for _, e := range logs.FilterMessageSnippet("Replaying mocks for your test command").All() {
		if got := e.ContextMap()["command"]; got != "go test ./..." {
			t.Fatalf("the replay log named %q as the user's command", got)
		}
	}
}

// A replay that could not start must not leave the last green receipt behind:
// keploy exited 1, and every reader would report a proof this run had just
// failed to repeat.
func TestSetupFailureReplacesTheReceipt(t *testing.T) {
	keployDir, _, _ := replayIn(t, replayCase{})
	if r := mustReceipt(t, keployDir); !r.Isolated {
		t.Fatalf("precondition: first run isolated: %+v", r)
	}
	repo := filepath.Dir(keployDir)

	// Same repo, second run, the agent cannot start.
	base := newInstr(t, agentUpFromSetup, false, models.AppError{AppErrorType: models.ErrAppStopped})
	instr := &runWrites{composeInstr: base, setupErr: errors.New("could not load the eBPF hooks")}
	cfg := instrConfig(base, utils.Native, "go test ./...")
	cfg.Path = keployDir
	cfg.Mock.OnMiss = "fail"
	t.Chdir(repo)
	svc := New(zap.NewNop(), instr, stubMockDB{}, nil, nil, nil, cfg)
	if err := svc.Replay(context.Background()); err == nil {
		t.Fatal("a replay whose setup failed returned no error")
	}
	r := mustReceipt(t, keployDir)
	if r.Isolated || r.FailedBy != FailedBySetup || r.ExitCode == 0 || r.RunnerExitCode != -1 || !strings.Contains(r.Error, "eBPF") {
		t.Fatalf("setup failure receipt: %+v", r)
	}
}

// An agent that could not start arms keploy's own specific exit code (3: no
// privileges, 6: the environment lacks something) before Setup returns. That
// code is not the runner's: the receipt must say the tests never ran, while
// still recording the exit the shell sees.
func TestAKeployExitCodeIsNotTheRunners(t *testing.T) {
	for _, tc := range []struct {
		code int
		why  error
	}{
		{utils.ExitPrivilegeRequired, utils.ErrPrivilegeRequired},
		{utils.ExitEnvironmentUnsupported, utils.ErrEnvironmentUnsupported},
	} {
		t.Run(fmt.Sprint(tc.code), func(t *testing.T) {
			repo := t.TempDir()
			t.Chdir(repo)
			keployDir := filepath.Join(repo, "keploy")
			if err := os.MkdirAll(filepath.Join(keployDir, "set"), 0o755); err != nil {
				t.Fatal(err)
			}
			base := newInstr(t, agentUpFromSetup, false, models.AppError{AppErrorType: models.ErrAppStopped})
			setupErr := fmt.Errorf("%w: the keploy agent could not start (exit status %d)", tc.why, tc.code)
			// What pkg/platform/http does on the way out of Setup.
			instr := &armsThenFails{runWrites: &runWrites{composeInstr: base, setupErr: setupErr}, code: tc.code}
			cfg := instrConfig(base, utils.Native, "go test ./...")
			cfg.Path = keployDir
			cfg.Mock.OnMiss = "fail"
			utils.ErrCode = 0
			t.Cleanup(func() { utils.ErrCode = 0 })

			if err := New(zap.NewNop(), instr, stubMockDB{}, nil, nil, nil, cfg).Replay(context.Background()); err == nil {
				t.Fatal("a replay whose agent could not start returned no error")
			}
			r := mustReceipt(t, keployDir)
			if r.FailedBy != FailedBySetup || r.RunnerExitCode != -1 || r.IsolationNote != "the test command never ran" {
				t.Fatalf("the receipt blames the test command for keploy's exit %d: %+v", tc.code, r)
			}
			if r.ExitCode != tc.code {
				t.Fatalf("receipt exit %d, but the process exits %d", r.ExitCode, tc.code)
			}
		})
	}
}

// armsThenFails arms keploy's exit code and then fails Setup, the order the
// agent client does both in.
type armsThenFails struct {
	*runWrites
	code int
}

func (a *armsThenFails) Setup(ctx context.Context, cmd string, opts models.SetupOptions) error {
	utils.SetExitCodeOnce(a.code)
	return a.runWrites.Setup(ctx, cmd, opts)
}

// A user's Ctrl+C is not a failure, wherever it lands: no error, exit 0, and
// the last completed run's receipt stays, since it still truthfully describes
// that run. While the test command runs, the interrupt cancels the run's own
// context too, which is exactly what keploy's agent dying under it looks like
// from there; only the caller's context tells the two apart.
func TestAnInterruptedReplayIsNotAFailure(t *testing.T) {
	for _, tc := range []struct {
		stage string
		// What the stopped runner reports: the cancellation, or -- when its
		// own exit wins Run's select, as when the interrupt reached it too
		// (a CI job being cancelled signals every process) -- that exit.
		runExit models.AppError
	}{
		{"setup", models.AppError{AppErrorType: models.ErrAppStopped}},
		{"run", models.AppError{AppErrorType: models.ErrCtxCanceled}},
		{"run-exit", models.AppError{AppErrorType: models.ErrUnExpected, ExitCode: 130}},
	} {
		t.Run(tc.stage, func(t *testing.T) {
			keployDir, _, _ := replayIn(t, replayCase{})
			// The last completed run was an hour ago, so any receipt this run
			// writes differs from it.
			last := mustReceipt(t, keployDir)
			last.At = last.At.Add(-time.Hour)
			writeReceipt(zap.NewNop(), keployDir, *last)
			receiptPath := filepath.Join(keployDir, "set", ReceiptFile)
			before, err := os.ReadFile(receiptPath)
			if err != nil {
				t.Fatal(err)
			}

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			base := newInstr(t, agentUpFromSetup, false, tc.runExit)
			instr := &runWrites{composeInstr: base, onRun: cancel}
			if tc.stage == "setup" {
				instr = &runWrites{composeInstr: base, setupErr: context.Canceled}
				cancel()
			}
			cfg := instrConfig(base, utils.Native, "go test ./...")
			cfg.Path = keployDir
			cfg.Mock.OnMiss = "fail"
			utils.ErrCode = 0

			if err := New(zap.NewNop(), instr, stubMockDB{}, nil, nil, nil, cfg).Replay(ctx); err != nil {
				t.Fatalf("an interrupted replay failed: %v", err)
			}
			if utils.ErrCode != 0 {
				t.Fatalf("an interrupted replay exits %d", utils.ErrCode)
			}
			if after, err := os.ReadFile(receiptPath); err != nil || !bytes.Equal(after, before) {
				t.Fatalf("an interrupted replay replaced the last receipt (%v):\n%s", err, after)
			}
		})
	}
}

// An empty or foreign file unmarshals into a zero Receipt with no error --
// which would read as a replay that passed with nothing missed.
func TestReadReceiptRejectsANonReceipt(t *testing.T) {
	keployDir := filepath.Join(t.TempDir(), "keploy")
	if err := os.MkdirAll(filepath.Join(keployDir, "set"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{"", "# just a comment\n", "foo: bar\n"} {
		if err := os.WriteFile(filepath.Join(keployDir, "set", ReceiptFile), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if r, err := ReadReceipt(keployDir, "set"); err == nil {
			t.Fatalf("read %q as a receipt: %+v", body, r)
		}
	}
}

// A receipt in a cloned repo can be a symlink to /dev/zero or a FIFO. Reading
// it to EOF, as ReadReceipt once did, ran keploy out of memory or blocked it;
// it must now be refused AT ONCE. `keploy status`, which the VS Code extension
// runs on every sidebar render, reads receipts, so a block here freezes the
// panel.
func TestReadReceiptRefusesNonRegular(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no FIFOs or /dev/zero on windows")
	}
	cases := map[string]func(t *testing.T, path string){
		"fifo": func(t *testing.T, path string) { mkfifo(t, path) },
		"devzero": func(t *testing.T, path string) {
			if _, err := os.Stat("/dev/zero"); err != nil {
				t.Skip("no /dev/zero")
			}
			if err := os.Symlink("/dev/zero", path); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, plant := range cases {
		t.Run(name, func(t *testing.T) {
			keployDir := filepath.Join(t.TempDir(), "keploy")
			if err := os.MkdirAll(filepath.Join(keployDir, "set"), 0o755); err != nil {
				t.Fatal(err)
			}
			plant(t, filepath.Join(keployDir, "set", ReceiptFile))
			done := make(chan error, 1)
			go func() {
				_, err := ReadReceipt(keployDir, "set")
				done <- err
			}()
			select {
			case err := <-done:
				if !errors.Is(err, safeyaml.ErrNotRegular) {
					t.Fatalf("ReadReceipt on a %s receipt = %v, want ErrNotRegular", name, err)
				}
				if !strings.Contains(err.Error(), ReceiptFile) {
					t.Errorf("error %q does not name the receipt", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatalf("ReadReceipt blocked on a %s receipt", name)
			}
		})
	}
}

// A receipt is a handful of scalar fields; one larger than the bound is refused
// rather than read.
func TestReadReceiptRefusesOversized(t *testing.T) {
	keployDir := filepath.Join(t.TempDir(), "keploy")
	if err := os.MkdirAll(filepath.Join(keployDir, "set"), 0o755); err != nil {
		t.Fatal(err)
	}
	big := make([]byte, ReceiptBytes+1)
	if err := os.WriteFile(filepath.Join(keployDir, "set", ReceiptFile), big, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadReceipt(keployDir, "set"); !safeyaml.IsRefused(err) {
		t.Fatalf("ReadReceipt of a receipt past %d bytes = %v, want refused", ReceiptBytes, err)
	}
}

// The digest names the file the loader actually serves: mocks.gob wins when
// it exists, so hashing mocks.yaml beside it would describe a file never read.
func TestMocksFileFollowsTheLoader(t *testing.T) {
	keployDir := filepath.Join(t.TempDir(), "keploy")
	dir := filepath.Join(keployDir, "set")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"mocks.yaml", "mocks.gob"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte(f), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if got := MocksFile(keployDir, "set"); filepath.Base(got) != "mocks.gob" {
		t.Fatalf("MocksFile = %s, want mocks.gob", got)
	}
}

// A compose project that dies while keploy is still starting mirrors its exit
// code, so the receipt must carry THAT code -- not a flat 1 that contradicts
// the exit the shell sees, and not "the test command never ran" about a runner
// that had just exited 7. Nor when the runner exited 1: that is keploy's
// generic code too, but keploy arms it for no failure of its own (a Setup
// error travels as the error), so a 1 already set is the runner's.
func TestSetupFailureRecordsTheMirroredExitCode(t *testing.T) {
	for _, runnerExit := range []int{7, 1} {
		t.Run(fmt.Sprint(runnerExit), func(t *testing.T) {
			repo := t.TempDir()
			t.Chdir(repo)
			keployDir := filepath.Join(repo, "keploy")
			if err := os.MkdirAll(filepath.Join(keployDir, "set"), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("KEPLOY_AGENT_READY_TIMEOUT", "120")
			instr := newInstr(t, agentNeverUp, false, models.AppError{AppErrorType: models.ErrUnExpected, ExitCode: runnerExit})
			cfg := instrConfig(instr, utils.DockerCompose, "docker compose run --rm tests")
			cfg.Path = keployDir
			cfg.Mock.Name = "set"
			cfg.Mock.OnMiss = "fail"
			utils.ErrCode = 0
			t.Cleanup(func() { utils.ErrCode = 0 })

			svc := New(zap.NewNop(), instr, stubMockDB{}, nil, nil, nil, cfg)
			if err := svc.Replay(context.Background()); err == nil {
				t.Fatal("Replay succeeded against a compose project that died")
			}
			r := mustReceipt(t, keployDir)
			if r.ExitCode != utils.ErrCode {
				t.Fatalf("receipt exit %d but the process exits %d", r.ExitCode, utils.ErrCode)
			}
			if r.ExitCode != runnerExit || r.RunnerExitCode != runnerExit || r.FailedBy != FailedByRunner {
				t.Fatalf("receipt: exit=%d runner=%d failedBy=%q, want the runner's %d", r.ExitCode, r.RunnerExitCode, r.FailedBy, runnerExit)
			}
			if r.Isolated || r.Error == "" {
				t.Fatalf("a run that never replayed anything claims isolation: %+v", r)
			}
		})
	}
}

// failsGroup fails a goroutine inside the run's own errgroup -- what a dying
// agent does -- and then fails at the chosen stage of the replay.
type failsGroup struct {
	*composeInstr
	stage string
}

// killGroup fails a goroutine in the run's errgroup and waits for the cancel
// it causes, so the stage that fails next sees a cancelled ctx.
func killGroup(ctx context.Context) {
	if g, ok := ctx.Value(models.ErrGroupKey).(*errgroup.Group); ok {
		g.Go(func() error { return errors.New("the agent stopped") })
		for i := 0; i < 200 && ctx.Err() == nil; i++ {
			time.Sleep(time.Millisecond)
		}
	}
}

func (f *failsGroup) Setup(ctx context.Context, cmd string, opts models.SetupOptions) error {
	if f.stage == "setup" {
		killGroup(ctx)
		return errors.New("could not reach the agent")
	}
	if f.stage == "compose" {
		// startComposeApp waits for an agent that never comes up; the group
		// dies underneath it.
		killGroup(ctx)
	}
	return f.composeInstr.Setup(ctx, cmd, opts)
}

// Run is where an agent that dies after coming up takes the run with it: the
// app runner sees its context cancelled and stops the test command, and says
// so -- or, at "run-silent", reports nothing at all, the other exit
// propagateExit leaves unmirrored.
func (f *failsGroup) Run(ctx context.Context, opts models.RunOptions) models.AppError {
	switch f.stage {
	case "run":
		killGroup(ctx)
		return models.AppError{AppErrorType: models.ErrCtxCanceled}
	case "run-silent":
		killGroup(ctx)
		return models.AppError{}
	}
	return f.composeInstr.Run(ctx, opts)
}

func (f *failsGroup) MockOutgoing(ctx context.Context, opts models.OutgoingOptions) error {
	if f.stage == "mock-outgoing" {
		killGroup(ctx)
		return errors.New("could not enable mock serving")
	}
	return f.composeInstr.MockOutgoing(ctx, opts)
}

func (f *failsGroup) StoreMocks(ctx context.Context, a []*models.Mock, b []*models.Mock) error {
	if f.stage == "store" {
		killGroup(ctx)
		return errors.New("could not store the mocks on the agent")
	}
	return f.composeInstr.StoreMocks(ctx, a, b)
}

func (f *failsGroup) UpdateMockParams(ctx context.Context, p models.MockFilterParams) error {
	if f.stage == "arm" {
		killGroup(ctx)
		return errors.New("could not arm the mock pool")
	}
	return f.composeInstr.UpdateMockParams(ctx, p)
}

// keploy's own failure is not the user pressing Ctrl+C. The errgroup cancels
// whenever anything in it fails, so testing that context made an internal
// failure exit 0 -- and leave the previous receipt standing as proof of a run
// that had just failed.
func TestAnInternalFailureIsNotAUserInterrupt(t *testing.T) {
	keployDir, _, _ := replayIn(t, replayCase{})
	if r := mustReceipt(t, keployDir); !r.Isolated {
		t.Fatalf("precondition: the first run is isolated: %+v", r)
	}
	repo := filepath.Dir(keployDir)
	t.Chdir(repo)

	// Every stage that can fail, because each one guards itself: the compose
	// bring-up guard was missed by a test that only failed Setup.
	for _, stage := range []string{"setup", "compose", "mock-outgoing", "store", "arm"} {
		t.Run(stage, func(t *testing.T) {
			keployDir, _, _ := replayIn(t, replayCase{})
			repo := filepath.Dir(keployDir)
			t.Chdir(repo)
			if r := mustReceipt(t, keployDir); !r.Isolated {
				t.Fatalf("precondition: the first run is isolated: %+v", r)
			}

			lifetime, cmdType := agentUpFromSetup, utils.Native
			if stage == "compose" {
				lifetime, cmdType = agentNeverUp, utils.DockerCompose
				t.Setenv("KEPLOY_AGENT_READY_TIMEOUT", "120")
			}
			base := newInstr(t, lifetime, false, models.AppError{AppErrorType: models.ErrAppStopped})
			cfg := instrConfig(base, cmdType, "go test ./...")
			cfg.Path = keployDir
			cfg.Mock.OnMiss = "fail"
			utils.ErrCode = 0
			t.Cleanup(func() { utils.ErrCode = 0 })

			svc := New(zap.NewNop(), &failsGroup{composeInstr: base, stage: stage}, stubMockDB{}, nil, nil, nil, cfg)
			if err := svc.Replay(context.Background()); err == nil {
				t.Fatalf("a replay that failed at %s reported success", stage)
			}
			if r := mustReceipt(t, keployDir); r.Isolated {
				t.Fatalf("the previous run's receipt is still vouching after a failure at %s: %+v", stage, r)
			}
		})
	}
}

// The agent dying while the test command runs stops the runner with it, which
// reached the replay as a plain cancellation: nothing mirrored, exit 0, and a
// CI step went green on a suite that never finished.
func TestAnAgentDyingMidRunFailsTheReplay(t *testing.T) {
	for _, stage := range []string{"run", "run-silent"} {
		t.Run(stage, func(t *testing.T) {
			keployDir, _, _ := replayIn(t, replayCase{})
			t.Chdir(filepath.Dir(keployDir))
			base := newInstr(t, agentUpFromSetup, false, models.AppError{AppErrorType: models.ErrAppStopped})
			cfg := instrConfig(base, utils.Native, "go test ./...")
			cfg.Path = keployDir
			cfg.Mock.OnMiss = "fail"
			utils.ErrCode = 0
			t.Cleanup(func() { utils.ErrCode = 0 })

			core, logs := observer.New(zap.ErrorLevel)
			err := New(zap.New(core), &failsGroup{composeInstr: base, stage: stage}, stubMockDB{}, nil, nil, nil, cfg).Replay(context.Background())
			if utils.ErrCode == 0 {
				t.Fatal("a replay whose agent died mid-run exits 0")
			}
			// Returned, not only armed: keploy's 1 and a failing suite's 1 are
			// the same number, and the error is what tells them apart.
			if err == nil || !strings.Contains(err.Error(), "keploy did not complete the run") {
				t.Fatalf("Replay returned %v, want keploy's own failure", err)
			}
			// And logged by the command that receives it, not by Replay as
			// well. (The errgroup's teardown reports its own error as it
			// drains; that report is not Replay's verdict.)
			for _, e := range logs.All() {
				if e.Message != "failed to drain mock-replay goroutines" && strings.Contains(fmt.Sprint(e.ContextMap()["error"]), "the agent stopped") {
					t.Fatalf("Replay logged the failure it returns: %q %v", e.Message, e.ContextMap())
				}
			}
			// Keploy stopped the test command, so it has no exit of its own
			// to report -- a 0 there read as a suite that passed -- and the
			// receipt says why keploy did not finish.
			if r := mustReceipt(t, keployDir); r.FailedBy != FailedByKeploy || r.Isolated || r.RunnerExitCode != -1 || !strings.Contains(r.Error, "the agent stopped") {
				t.Fatalf("receipt: failedBy=%q isolated=%v runner=%d error=%q, want keploy's own failure and why", r.FailedBy, r.Isolated, r.RunnerExitCode, r.Error)
			}
		})
	}
}

// A panic inside keploy's own app-runner arrives as ErrInternal. Recording it
// as the runner's failure wrote "the test command failed" into the receipt
// about a suite that may never have been reached.
func TestKeploysOwnFailureIsNotTheTestsFailing(t *testing.T) {
	keployDir, code, _ := replayIn(t, replayCase{runExit: models.AppError{AppErrorType: models.ErrInternal, Err: errors.New("the app runner panicked")}, keployFails: true})
	if code == 0 {
		t.Fatal("an internal failure exited cleanly")
	}
	r := mustReceipt(t, keployDir)
	if r.FailedBy != FailedByKeploy || r.RunnerExitCode != -1 {
		t.Fatalf("receipt blames the runner for keploy's failure: failedBy=%q runner=%d", r.FailedBy, r.RunnerExitCode)
	}
	if r.Error != "the app runner panicked" {
		t.Fatalf("receipt error %q does not say why keploy did not complete the run", r.Error)
	}
	if r.Isolated || !strings.Contains(r.IsolationNote, "keploy did not complete") {
		t.Fatalf("isolation note: %q (isolated=%v)", r.IsolationNote, r.Isolated)
	}
}

// A receipt that cannot be written must take the previous one with it: a
// stale green receipt is a false proof, where none at all is only a gap.
func TestAFailedWriteRemovesTheStaleReceipt(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes through a read-only directory")
	}
	keployDir, _, _ := replayIn(t, replayCase{})
	dir := filepath.Join(keployDir, "set")
	if _, err := os.Stat(filepath.Join(dir, ReceiptFile)); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	core, logs := observer.New(zapcore.DebugLevel)
	writeReceipt(zap.New(core), keployDir, Receipt{Set: "set", OnMiss: "fail", Isolated: false})
	if logs.FilterMessageSnippet("could not write the replay receipt").Len() == 0 {
		t.Fatal("a receipt that could not be written was not reported")
	}
	// Either it took the stale receipt with it, or it says the one on disk no
	// longer describes the last run. Silence is the one thing it must not do:
	// that leaves a green, isolated receipt vouching for a run nobody saw.
	_, err := os.Stat(filepath.Join(dir, ReceiptFile))
	stillThere := err == nil
	if stillThere && logs.FilterMessageSnippet("no longer describes it").Len() == 0 {
		t.Fatal("a stale receipt was left in place without a word")
	}
}

// The temporary file the write goes through is git-ignored and is not a
// .yaml: a kill between creating and renaming it leaves one behind, and
// `git add -A` would commit it.
func TestTheReceiptsTemporaryFileIsIgnored(t *testing.T) {
	keployDir, _, _ := replayIn(t, replayCase{})
	gi, err := os.ReadFile(filepath.Join(keployDir, ".gitignore"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"/*/last-replay.yaml", "/*/.last-replay-*"} {
		if !strings.Contains(string(gi), want) {
			t.Errorf("keploy/.gitignore does not carry %q:\n%s", want, gi)
		}
	}
	if strings.HasSuffix(receiptTempPrefix+"x.tmp", ".yaml") {
		t.Fatal("the temporary file looks like a receipt")
	}
	// Read first, written only when something is missing. The helper is an
	// unlocked read-modify-write, so a replay that opens the file for writing
	// every time turns a first-run race into one on every run.
	if err := os.Chmod(filepath.Join(keployDir, ".gitignore"), 0o444); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(keployDir, ".gitignore"), 0o644) })
	core, logs := observer.New(zapcore.DebugLevel)
	ignoreReceipts(zap.New(core), keployDir)
	if logs.Len() != 0 {
		t.Fatalf("keploy/.gitignore was opened for writing when its entries were already there: %v", logs.All()[0])
	}
	if after, _ := os.ReadFile(filepath.Join(keployDir, ".gitignore")); string(after) != string(gi) {
		t.Fatalf("a second pass rewrote keploy/.gitignore:\n%s", after)
	}
}
