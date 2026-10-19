package mock

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/coverage/report"
	"go.keploy.io/server/v3/pkg/platform/safeyaml"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"gopkg.in/yaml.v3"
)

// ReceiptFile is the per-set record of the last replay, kept next to the set
// it describes: keploy/<set>/last-replay.yaml.
//
// It exists so that a replay's verdict outlives the terminal it scrolled past
// in. An editor panel, a coding agent or a status command needs to know
// whether a set has been proven to run with its dependencies off, and before
// this the only record was a log line -- so each client kept its own copy of
// the verdict, and a replay an agent ran in a terminal was invisible to all of
// them.
//
// It is a LOCAL result: it describes a run on this machine, it changes on every
// replay, and a copy committed from one laptop would vouch for a clone that
// never ran anything. So the CLI adds it to keploy/.gitignore when it writes it.
const ReceiptFile = "last-replay.yaml"

// receiptIgnore are the keploy/.gitignore entries that keep receipts local.
// Set names are single path segments (validateMockFlags), so /*/ reaches all.
// The second covers the temporary file the write goes through: a kill between
// creating it and renaming it leaves one behind, and `git add -A` would commit
// it.
var receiptIgnore = []string{"/*/" + ReceiptFile, "/*/" + receiptTempPrefix + "*", "/*/" + TestsReceiptFile}

// receiptTempPrefix names the temporary file. Deliberately NOT *.yaml: an
// editor watching keploy/*/*.yaml would pick the transient file up as though
// it were a receipt.
const receiptTempPrefix = ".last-replay-"

// Why a replay failed, when it did.
const (
	FailedBySetup           = "setup"            // keploy could not start the run; the tests never ran
	FailedByRunner          = "runner"           // the test command itself failed
	FailedByStrict          = "strict"           // --strict: a counted miss, or the miss list or PID-map count was unreadable
	FailedByMinCoverage     = "min-coverage"     // --min-coverage: the run covered too little, or wrote no report
	FailedByKeploy          = "keploy"           // keploy itself did not complete the run
	FailedByNothingVerified = "nothing-verified" // the run served none of the recorded calls, so it verified nothing
	// FailedByCaptureIncomplete: the agent could not track a process the test
	// command ran (its PID map refused it), so that process's calls went to the
	// real dependency, neither mocked nor counted (models.PIDMapRefusals).
	FailedByCaptureIncomplete = "capture_incomplete"
)

// Receipt is what one `keploy mock replay` proved.
type Receipt struct {
	Set     string    `yaml:"set" json:"set"`
	At      time.Time `yaml:"at" json:"at"`
	Command string    `yaml:"command" json:"command"`
	// OnMiss is the miss policy the run used. Only "fail" keeps a call that
	// was never recorded from reaching the real dependency.
	OnMiss string `yaml:"onMiss" json:"onMiss"`
	Strict bool   `yaml:"strict" json:"strict"`
	// ExitCode is what keploy exited with. RunnerExitCode is what the test
	// command itself exited with (-1 when it has no exit of its own: it never
	// ran, or keploy failed before it exited), so a run failed by a gate is
	// not mistaken for a failing test. FailedBy names which one failed the
	// run, when one did.
	ExitCode       int    `yaml:"exitCode" json:"exitCode"`
	RunnerExitCode int    `yaml:"runnerExitCode" json:"runnerExitCode"`
	FailedBy       string `yaml:"failedBy,omitempty" json:"failedBy,omitempty"`
	// Error is why keploy could not run the replay, or did not complete it.
	Error string `yaml:"error,omitempty" json:"error,omitempty"`
	// Loaded, Consumed and Missed are mock counts. -1 means the agent never
	// reported that count, which is not the same as zero.
	Loaded   int `yaml:"loaded" json:"loaded"`
	Consumed int `yaml:"consumed" json:"consumed"`
	Missed   int `yaml:"missed" json:"missed"`
	// MissedData counts, under the proven switch only, the misses that count
	// against a test (models.UnmatchedCall.CountsAgainstTest: not DNS lookups,
	// which are protocol-phase, nor connections that sent nothing): what the
	// proven model's verdict fails on. -1 when the misses were not read;
	// absent with the switch off.
	MissedData *int `yaml:"missedData,omitempty" json:"missedData,omitempty"`
	// Bypass lists the destinations configured to skip keploy (bypassRules,
	// --pass-through-ports): calls to them reach the real service.
	Bypass []string `yaml:"bypass,omitempty" json:"bypass,omitempty"`
	// Isolated says the run proved the suite passes with no dependency
	// reachable, and IsolationNote says why not when it did not — or, when
	// the run gated tests out (Tests.Gated), that it proved it only for the
	// tests that ran. Decided once,
	// when the run ends, so the log line and every reader agree.
	Isolated      bool   `yaml:"isolated" json:"isolated"`
	IsolationNote string `yaml:"isolationNote,omitempty" json:"isolationNote,omitempty"`
	// MocksDigest is the sha256 of the mocks file this run replayed, taken
	// before the run, so a reader can tell a receipt about THIS recording
	// from one about a set that has since been re-recorded. Under
	// `--on-miss record` the run appends to the set as it goes, so its own
	// receipt is stale the moment it is written -- which is correct: the
	// recording it describes is no longer the one on disk.
	MocksDigest string `yaml:"mocksDigest" json:"mocksDigest"`
	// Coverage is the runner's own coverage report for this run, when it
	// wrote one. CoverageNote says why there is none.
	Coverage     *report.Summary `yaml:"coverage,omitempty" json:"coverage,omitempty"`
	CoverageNote string          `yaml:"coverageNote,omitempty" json:"coverageNote,omitempty"`
	// MinCoverage is the --min-coverage floor this run was held to, if any.
	MinCoverage float64 `yaml:"minCoverage,omitempty" json:"minCoverage,omitempty"`
	Version     string  `yaml:"keployVersion,omitempty" json:"keployVersion,omitempty"`
	// Tests counts the run's tests by verdict, when the recording or the run
	// named any; what each one did is in TestsReceiptFile, next to this one.
	Tests *TestCounts `yaml:"tests,omitempty" json:"tests,omitempty"`
	// Refusals are, under the proven switch, what keploy refused during the
	// run (a second test process, a test it could not judge) and what a test
	// framework's integration refused and told keploy (go test running more
	// than one test binary at once): keploy's verdict fails the run naming
	// each. Absent with the switch off.
	Refusals []models.RunRefusal `yaml:"refusals,omitempty" json:"refusals,omitempty"`
	// CannotCapture is what the test command did that keploy cannot capture at
	// all (decision 20: a call over a Unix socket, or Unix sockets keploy
	// cannot see; CaptureLossKind.NotJudged), counted over the whole run
	// from its base, one entry per kind with its latest What. Non-empty
	// means the run has no verdict, whatever its tests' lines say: a call
	// after the last test's read is on no line. Empty with the switch off.
	CannotCapture models.CaptureLosses `yaml:"cannotCapture,omitempty" json:"cannotCapture,omitempty"`
}

// TestCounts counts a replay's tests by their verdict this run.
type TestCounts struct {
	Passed  int `yaml:"passed" json:"passed"`
	Failed  int `yaml:"failed" json:"failed"`
	Skipped int `yaml:"skipped" json:"skipped"`
	// Gated tests were told not to run this replay, for GateReason. A run
	// with gated tests proved nothing about them.
	Gated      int    `yaml:"gated" json:"gated"`
	GateReason string `yaml:"gateReason,omitempty" json:"gateReason,omitempty"`
	// NoVerdict tests did not run this replay, or ran with a harness that
	// reported no verdict (or one keploy does not know).
	NoVerdict int `yaml:"noVerdict" json:"noVerdict"`
	// GateNote says the run was asked to run only some tests but ran more
	// (no harness reported, or one that cannot skip ran a test outside the
	// list — Ungated counts those); "" when no gate was asked for or it held.
	GateNote string `yaml:"gateNote,omitempty" json:"gateNote,omitempty"`
	Ungated  int    `yaml:"ungated,omitempty" json:"ungated,omitempty"`
}

// countTests summarises per-test lines.
func countTests(tests []TestReceipt, gateReason string) *TestCounts {
	if len(tests) == 0 {
		return nil
	}
	c := &TestCounts{}
	for _, t := range tests {
		switch t.Outcome {
		case models.ScopeOutcomePassed:
			c.Passed++
		case models.ScopeOutcomeFailed:
			c.Failed++
		case models.ScopeOutcomeSkipped:
			c.Skipped++
		case models.ScopeOutcomeGated:
			c.Gated++
		default:
			c.NoVerdict++
		}
	}
	if c.Gated > 0 {
		c.GateReason = gateReason
	}
	return c
}

// TestsReceiptFile holds what each test did in the last replay of a set,
// next to its ReceiptFile and local and git-ignored like it. It is kept apart
// because it grows with the suite, and the receipt is read whole by every
// status check.
const TestsReceiptFile = "last-replay-tests.yaml"

// TestsReceipt is the content of TestsReceiptFile.
type TestsReceipt struct {
	Set string    `yaml:"set" json:"set"`
	At  time.Time `yaml:"at" json:"at"`
	// MocksDigest is the receipt's: which recording these results are about.
	MocksDigest string        `yaml:"mocksDigest" json:"mocksDigest"`
	Tests       []TestReceipt `yaml:"tests" json:"tests"`
	// Refusals are, in a record's file, what keploy refused during it
	// (Receipt.Refusals): a record that reported no test says why.
	Refusals []models.RunRefusal `yaml:"refusals,omitempty" json:"refusals,omitempty"`
	// Live and Mocked are, under the proven switch, where the run's calls to
	// local listeners went, each row naming the test open then (none:
	// outside every test): a listener of the run's own process tree, called
	// live (models.LiveRow), and any other local listener, recorded and
	// mocked (models.MockedRow). keploy's summary lists them.
	Live   []models.LiveRow   `yaml:"live,omitempty" json:"live,omitempty"`
	Mocked []models.MockedRow `yaml:"mocked,omitempty" json:"mocked,omitempty"`
	// Real is, under the proven switch, what the run saw of mock.real beyond
	// its tests (design v14 §4, §8.2 rule 4): the destinations it reached,
	// those it first reached outside a test, and the capture_incomplete
	// evidence charged to no test. nil without the switch.
	Real *models.RealRun `yaml:"real,omitempty" json:"real,omitempty"`
	// CannotCapture is, under the proven switch, what the run did that keploy
	// cannot capture at all (Receipt.CannotCapture): in a replay's file the
	// receipt's, in a record's what the recording lacks, so it is not taken
	// as fully captured.
	CannotCapture models.CaptureLosses `yaml:"cannotCapture,omitempty" json:"cannotCapture,omitempty"`
	// CaptureLosses is, in a record's file under the proven switch, what the
	// agent could not capture that no test was charged with (design v14
	// §3.2: a loss charges the test open when it happened and every later
	// one, so this is a loss after the last test): no recording holds what
	// it carried, so judging fails the run on it (§7.5, evidence charged to
	// no test). What keploy cannot capture at all is CannotCapture's, not this.
	CaptureLosses models.CaptureLosses `yaml:"captureLosses,omitempty" json:"captureLosses,omitempty"`
	// CaptureLossesUnread is, in a record's file under the proven switch, why
	// keploy could not read what its agent could not capture after the last
	// test: judging fails the run on it, as on CaptureLosses (a read that
	// failed is never "none").
	CaptureLossesUnread string `yaml:"captureLossesUnread,omitempty" json:"captureLossesUnread,omitempty"`
	// Learned is what the learn step of a record's full round wrote onto
	// mocks no test consumed (the set's start-up and pool), each field as
	// "<mock> <path> (<kind>)"; a test's own is on its line (design v14
	// §7.2 step 10).
	Learned []string `yaml:"learned,omitempty" json:"learned,omitempty"`
}

// empty reports whether the run reported no test and keploy refused
// nothing: such a run leaves no per-test file, whatever rows it saw. A
// record's file is what says the record reported its tests (judging reads a
// missing one as "the record reported no test"), so rows alone, which a
// suite without keploy's integration still makes, never write one; nor does
// what the run saw of mock.real (its agent logs each connection it could not
// capture).
func (r TestsReceipt) empty() bool {
	return len(r.Tests) == 0 && len(r.Refusals) == 0
}

// TestReceipt is one test's line in a TestsReceipt. Outcome is its latest
// run's verdict this replay (models.ScopeOutcome*); "" when it did not run or
// reported none. A count the agent did not report in full is -1.
type TestReceipt struct {
	Name     string `yaml:"name" json:"name"`
	Outcome  string `yaml:"outcome,omitempty" json:"outcome,omitempty"`
	Consumed int    `yaml:"consumed" json:"consumed"`
	Missed   int    `yaml:"missed" json:"missed"`
	// Evidence is, under the proven wire, what failed the test at its end:
	// its misses, the recorded calls of its mapping it never made, and the
	// connections keploy could not capture; or what left it not judged
	// (CannotCapture: a call keploy cannot capture at all, a Unix socket).
	Evidence *models.ScopeEvidence `yaml:"evidence,omitempty" json:"evidence,omitempty"`
	// Real is, under the proven wire, the mock.real destinations the test
	// used while it was open: its REAL rows (design v14 §4).
	Real []models.RealUse `yaml:"real,omitempty" json:"real,omitempty"`
	// RealListed is, in a replay's file, the mock.real destinations the
	// test's recording lists: the run must reach each (design v14 §8.2 rule
	// 4), and the test ran on the real clock (§3.4, F3).
	RealListed []string `yaml:"realListed,omitempty" json:"realListed,omitempty"`
	// Learned is what the learn step of a record's full round wrote onto the
	// test's recording, each field as "<mock> <path> (<kind>)" (design v14
	// §7.2 step 10). Only that round has any.
	Learned []string `yaml:"learned,omitempty" json:"learned,omitempty"`
}

// isolation decides whether a finished run proved isolation: the runner
// passed, a call the recording lacks could not reach a real service, the
// agent's miss list was read and was empty, and the agent served at least one
// recorded call -- the only evidence the test command's traffic went through
// keploy at all. A run keploy never intercepted also passes with nothing
// missed; on macOS that is what `npm test` does, because the shell npm runs
// scripts through drops the interception.
func isolation(runnerExit int, policy models.MissPolicy, counts replayCounts, bypass []string) (bool, string) {
	switch {
	case runnerExit != 0:
		return false, "the test command failed"
	case policy != models.MissFail:
		return false, fmt.Sprintf("--on-miss %s lets a call the recording does not have reach the real dependency", policy)
	case len(bypass) > 0:
		return false, "bypass rules send calls to " + strings.Join(bypass, ", ") + " to the real service"
	case counts.missed < 0:
		return false, "the agent could not report every call no recorded mock answered"
	case counts.missedData > 0:
		// Only a miss that counts against a test (models.UnmatchedCall.
		// CountsAgainstTest), as --strict reads it: a DNS lookup's miss is
		// the proxy's own answer, and a connection whose client sent
		// nothing made no call, so neither reached a real service.
		return false, fmt.Sprintf("%d call(s) matched nothing in the recording", counts.missedData)
	case counts.consumed < 0:
		return false, "the agent never reported which recorded calls it served"
	case counts.consumed == 0:
		return false, "no recorded call was served, so nothing shows the test command's traffic went through keploy"
	}
	return true, ""
}

// bypassList renders the bypass rules as destinations.
func bypassList(rules []models.BypassRule) []string {
	var out []string
	for _, r := range rules {
		d := r.Host
		if r.Port != 0 {
			d += fmt.Sprintf(":%d", r.Port)
		}
		d += r.Path
		if d != "" {
			out = append(out, d)
		}
	}
	return out
}

// writeTestsReceipt writes TestsReceiptFile for a replay, or removes a stale
// one when this replay named no tests: it would describe an earlier run.
func writeTestsReceipt(logger *zap.Logger, keployDir string, r TestsReceipt) {
	dir := filepath.Join(keployDir, r.Set)
	if info, err := os.Lstat(dir); err != nil || !info.IsDir() {
		return
	}
	writeTestsFile(logger, dir, TestsReceiptFile, "# Written by `keploy mock replay`: what each test did in the last replay of this set.\n", "this replay", r)
}

// RecordTestsReceiptFile holds what each test did in the last record of a set:
// its verdict against real infra, which judging the recording reads (design
// v14 §7.4, the R0 round). Local and git-ignored like the replay's.
const RecordTestsReceiptFile = "last-record-tests.yaml"

// writeRecordTestsReceipt writes RecordTestsReceiptFile: the verdict each
// top-level test of the record reported as it ended, with the evidence the
// proven wire left on its window, whether or not the recording was kept. It makes the set's directory when the record kept
// nothing, since a test that failed against real infra is judged too.
func writeRecordTestsReceipt(logger *zap.Logger, keployDir string, r TestsReceipt, windows []models.ScopeWindow) {
	set := r.Set
	for _, w := range windows {
		if w.App || w.Suite || parentOf(windows, w.Name) != "" {
			continue
		}
		tr := TestReceipt{Name: w.Name, Outcome: w.Outcome, Real: w.Real}
		// What keploy could not capture while the test ran (a record's
		// window carries no misses): a verdict against real infra keploy
		// did not see whole is no basis for judging the recording.
		if ev := w.Evidence; ev != nil && (!ev.Empty() || ev.Reason != "") {
			tr.Evidence = ev
		}
		r.Tests = append(r.Tests, tr)
	}
	dir := filepath.Join(keployDir, set)
	if !r.empty() {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			logger.Warn("could not write what each test did in this record", zap.String("dir", dir), zap.Error(err))
			return
		}
	} else if info, err := os.Lstat(dir); err != nil || !info.IsDir() {
		return
	}
	writeTestsFile(logger, dir, RecordTestsReceiptFile, "# Written by `keploy mock record`: what each test did against real infra in the last record of this set.\n", "this record", r)
	if err := utils.AddToGitIgnore(logger, keployDir, "/*/"+RecordTestsReceiptFile); err != nil {
		logger.Debug("failed to git-ignore the record's per-test results", zap.Error(err))
	}
}

// ReadRecordTestsReceipt reads keploy/<set>/last-record-tests.yaml; (nil, nil)
// when there is none.
func ReadRecordTestsReceipt(keployDir, set string) (*TestsReceipt, error) {
	return readTestsFile(keployDir, set, RecordTestsReceiptFile, "a record's")
}

// writeTestsFile writes a per-test results file into dir, or removes a stale
// one when the run named no tests: it would describe an earlier run.
func writeTestsFile(logger *zap.Logger, dir, file, head, run string, r TestsReceipt) {
	path := filepath.Join(dir, file)
	if r.empty() {
		_ = os.Remove(path)
		return
	}
	body, err := yaml.Marshal(r)
	if err == nil {
		// The receipt's temporary prefix covers this file's too.
		err = writeLocalFile(logger, dir, file, receiptTempPrefix+"tests-", append([]byte(head), body...))
	}
	if err != nil {
		_ = os.Remove(path)
		logger.Warn("could not write what each test did in "+run, zap.String("dir", dir), zap.Error(err),
			zap.String("next_step", "check that "+dir+" is writable by the user running keploy"))
	}
}

// TestsReceiptBytes bounds a TestsReceiptFile read: tens of thousands of tests.
const TestsReceiptBytes = 64 << 20

// ReadTestsReceipt reads keploy/<set>/last-replay-tests.yaml; (nil, nil) when
// there is none. Read through safeyaml for the reasons ReadReceipt is.
func ReadTestsReceipt(keployDir, set string) (*TestsReceipt, error) {
	return readTestsFile(keployDir, set, TestsReceiptFile, "a replay's")
}

func readTestsFile(keployDir, set, file, whose string) (*TestsReceipt, error) {
	raw, err := safeyaml.ReadFile(filepath.Join(keployDir, set, file), TestsReceiptBytes)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("%s: %w", filepath.Join(set, file), safeyaml.StripPath(err))
	}
	var r TestsReceipt
	if err := yaml.Unmarshal(raw, &r); err != nil {
		return nil, fmt.Errorf("%s: %w", filepath.Join(set, file), err)
	}
	// An empty or foreign document unmarshals into a zero TestsReceipt with
	// no error, as for the receipt.
	if r.Set == "" {
		return nil, fmt.Errorf("%s is not %s per-test results", filepath.Join(set, file), whose)
	}
	return &r, nil
}

// ReceiptBytes bounds a receipt read. A receipt is a handful of scalar fields
// and a coverage summary; the largest keploy writes is well under a kilobyte.
const ReceiptBytes = 1 << 20

// ReadReceipt reads keploy/<set>/last-replay.yaml. A missing file is
// (nil, nil): the set has simply never been replayed here.
//
// The set directory comes from the repository keploy runs in, so the receipt
// can be a symlink to /dev/zero or a FIFO in a cloned repo. It is read through
// safeyaml, which refuses anything but a regular file of at most ReceiptBytes,
// rather than reading to EOF as it once did.
func ReadReceipt(keployDir, set string) (*Receipt, error) {
	raw, err := safeyaml.ReadFile(filepath.Join(keployDir, set, ReceiptFile), ReceiptBytes)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("%s: %w", filepath.Join(set, ReceiptFile), safeyaml.StripPath(err))
	}
	var r Receipt
	if err := yaml.Unmarshal(raw, &r); err != nil {
		return nil, fmt.Errorf("%s: %w", filepath.Join(set, ReceiptFile), err)
	}
	// An empty or foreign document unmarshals into a zero Receipt with no
	// error -- which reads as a replay that passed with nothing missed.
	if r.Set == "" {
		return nil, fmt.Errorf("%s is not a replay receipt", filepath.Join(set, ReceiptFile))
	}
	return &r, nil
}

// MocksFile returns the set's mocks file -- in the order the loader reads
// them: mocks.gob first, then mocks.yaml or mocks.json -- or "" when the set
// has none.
func MocksFile(keployDir, set string) string {
	for _, name := range []string{"mocks.gob", "mocks.yaml", "mocks.json"} {
		p := filepath.Join(keployDir, set, name)
		if info, err := os.Stat(p); err == nil && info.Mode().IsRegular() {
			return p
		}
	}
	return ""
}

// MocksDigest is the sha256 of the set's mocks file, "" when it has none.
func MocksDigest(keployDir, set string) string {
	p := MocksFile(keployDir, set)
	if p == "" {
		return ""
	}
	f, err := os.Open(p)
	if err != nil {
		return ""
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return ""
	}
	return hex.EncodeToString(h.Sum(nil))
}

// writeReceipt records the run next to its set. Best-effort: a receipt that
// cannot be written costs a later reader its answer, never this run its exit
// code. A set directory that does not exist is not created -- there was
// nothing to replay, and a receipt would invent a set.
//
// Written to a new temporary file and renamed into place, so a reader -- an
// editor watching the directory, a second replay of the same set -- sees the
// old receipt or the new one, never a half-written file. The temporary file is
// created exclusively and never through a link, and the rename replaces
// whatever sits at the receipt's path, a link included, rather than writing
// through it.
func writeReceipt(logger *zap.Logger, keployDir string, r Receipt) {
	dir := filepath.Join(keployDir, r.Set)
	if info, err := os.Lstat(dir); err != nil || !info.IsDir() {
		return
	}
	body, err := yaml.Marshal(r)
	if err != nil {
		logger.Debug("failed to encode the replay receipt", zap.Error(err))
		return
	}
	head := []byte("# Written by `keploy mock replay`: what the last replay of this set proved\n" +
		"# on this machine. Local and git-ignored; tools read it instead of re-running the replay.\n")
	// One exit for every failure to write: say so, and take the previous
	// receipt with it. Leaving it is worse than losing it -- it is green,
	// isolated, and about a run that has just been superseded by one whose
	// result nobody can see.
	fail := func(what error) {
		path := filepath.Join(dir, ReceiptFile)
		logger.Warn("could not write the replay receipt, so this run leaves no record of what it proved",
			zap.String("dir", dir), zap.Error(what),
			zap.String("next_step", "check that "+dir+" is writable by the user running keploy"))
		// Take the previous receipt with it where that is possible. Often it
		// is not -- removing a file needs write permission on its directory,
		// which is usually why the write failed -- so when the old receipt is
		// still there, say so: it is green, isolated, and about a run that has
		// just been superseded by one whose result nobody can see.
		_ = os.Remove(path)
		if _, err := os.Lstat(path); err == nil {
			logger.Warn("the last replay receipt on disk is older than this run and no longer describes it",
				zap.String("path", path),
				zap.String("next_step", "delete it, or make its directory writable so keploy can keep it current"))
		}
	}
	if err := writeLocalFile(logger, dir, ReceiptFile, receiptTempPrefix, append(head, body...)); err != nil {
		fail(err)
		return
	}
	ignoreReceipts(logger, keployDir)
}

// writeLocalFile writes data to dir/name through a temporary file renamed into
// place, so a kill halfway leaves the previous file or none, never half of
// one. Under sudo the file belongs to the developer who ran keploy, not root.
func writeLocalFile(logger *zap.Logger, dir, name, tempPrefix string, data []byte) error {
	tmp, err := os.CreateTemp(dir, tempPrefix+"*.tmp")
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_ = os.Remove(tmp.Name())
		}
	}()
	_, werr := tmp.Write(data)
	if werr == nil {
		werr = tmp.Chmod(0o644)
	}
	// Ownership on the open descriptor, before it can be renamed into place.
	utils.RestoreFileOwnershipOf(logger, tmp, tmp.Name())
	if cerr := tmp.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Rename(tmp.Name(), filepath.Join(dir, name))
	}
	if werr != nil {
		return werr
	}
	committed = true
	return nil
}

// ignoreReceipts adds the receipt entries to keploy/.gitignore, and reads the
// file first so the common case -- they are already there -- writes nothing.
//
// The helper is an unlocked read-modify-write, so two replays starting at once
// in one repository can both decide to append. Reading first makes that a
// first-run-only race instead of one on every replay.
func ignoreReceipts(logger *zap.Logger, keployDir string) {
	existing, _ := os.ReadFile(filepath.Join(keployDir, ".gitignore"))
	lines := map[string]bool{}
	for _, l := range strings.Split(string(existing), "\n") {
		lines[strings.TrimSpace(l)] = true
	}
	for _, entry := range receiptIgnore {
		if lines[entry] {
			continue
		}
		if err := utils.AddToGitIgnore(logger, keployDir, entry); err != nil {
			logger.Debug("failed to git-ignore the replay receipt", zap.String("entry", entry), zap.Error(err))
		}
	}
}
