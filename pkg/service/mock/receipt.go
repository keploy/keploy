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
var receiptIgnore = []string{"/*/" + ReceiptFile, "/*/" + receiptTempPrefix + "*"}

// receiptTempPrefix names the temporary file. Deliberately NOT *.yaml: an
// editor watching keploy/*/*.yaml would pick the transient file up as though
// it were a receipt.
const receiptTempPrefix = ".last-replay-"

// Why a replay failed, when it did.
const (
	FailedBySetup       = "setup"        // keploy could not start the run; the tests never ran
	FailedByRunner      = "runner"       // the test command itself failed
	FailedByStrict      = "strict"       // --strict: a call was missed, or the miss list was unreadable
	FailedByMinCoverage = "min-coverage" // --min-coverage: the run covered too little, or wrote no report
	FailedByKeploy      = "keploy"       // keploy itself did not complete the run
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
	// Bypass lists the destinations configured to skip keploy (bypassRules,
	// --pass-through-ports): calls to them reach the real service.
	Bypass []string `yaml:"bypass,omitempty" json:"bypass,omitempty"`
	// Isolated says the run proved the suite passes with no dependency
	// reachable, and IsolationNote says why not when it did not. Decided once,
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
		return false, "the agent never reported which calls were missed"
	case counts.missed > 0:
		return false, fmt.Sprintf("%d call(s) matched nothing in the recording", counts.missed)
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

// ReadReceipt reads keploy/<set>/last-replay.yaml. A missing file is
// (nil, nil): the set has simply never been replayed here.
func ReadReceipt(keployDir, set string) (*Receipt, error) {
	raw, err := os.ReadFile(filepath.Join(keployDir, set, ReceiptFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
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
	tmp, err := os.CreateTemp(dir, receiptTempPrefix+"*.tmp")
	if err != nil {
		fail(err)
		return
	}
	committed := false
	defer func() {
		if !committed {
			_ = os.Remove(tmp.Name())
		}
	}()
	_, werr := tmp.Write(append(head, body...))
	if werr == nil {
		werr = tmp.Chmod(0o644)
	}
	// Ownership on the open descriptor: under sudo the receipt belongs to the
	// developer who ran keploy, not to root.
	utils.RestoreFileOwnershipOf(logger, tmp, tmp.Name())
	if cerr := tmp.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Rename(tmp.Name(), filepath.Join(dir, ReceiptFile))
	}
	if werr != nil {
		fail(werr)
		return
	}
	committed = true
	ignoreReceipts(logger, keployDir)
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
