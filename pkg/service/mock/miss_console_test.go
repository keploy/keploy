package mock

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
	"gopkg.in/yaml.v3"
)

// keploy mock replay's line for a call no mock answered names it by its
// shape and the path tag the agent made for it, and its advice points to
// the console's Hint by that tag; the console names the call whole beside
// the same tag, with the advice whole under it.
func TestAMockReplayMissIsNamedWholeOnTheConsoleByItsTag(t *testing.T) {
	const tag = "#1f2e3d4c5b6a"
	miss := models.UnmatchedCall{
		Protocol: "HTTP", ActualSummary: "POST /oauth/token", Destination: "127.0.0.1:9096", PathTag: tag,
		NextSteps: `declare it (sessionCalls: [{host: "127.0.0.1:9096", method: "POST", pathPrefix: "/oauth/token"}])`,
	}
	core, logs := observer.New(zapcore.DebugLevel)
	logMiss(zap.New(core), miss)
	var console bytes.Buffer
	sayMisses(&console, []models.UnmatchedCall{miss}, "")

	want := "  Mock mismatch: [HTTP] POST /oauth/token [path tag " + tag + "]  →  127.0.0.1:9096\n" +
		`  Hint: declare it (sessionCalls: [{host: "127.0.0.1:9096", method: "POST", pathPrefix: "/oauth/token"}])` + "\n"
	if console.String() != want {
		t.Errorf("the console said\n%q, want\n%q", console.String(), want)
	}
	lines := logs.All()
	if len(lines) != 1 {
		t.Fatalf("logged %d lines, want 1", len(lines))
	}
	f := lines[0].ContextMap()
	next, _ := f["next_step"].(string)
	if f["call"] != "POST /{}/{}" || f["path_tag"] != tag || next != `declare it (sessionCalls: [{host: "127.0.0.1:9096", method: "POST", pathPrefix: <the path keploy's console Hint and report name for [path tag `+tag+`]>}])` {
		t.Errorf("the line is %v, want the call's shape, its tag and the advice by the tag", f)
	}
	for k, v := range f {
		if s, ok := v.(string); ok && strings.Contains(s, "oauth") {
			t.Errorf("the line's %s names the call's path: %q", k, s)
		}
	}
}

// A replay that misses many calls does not bury its console: each call is
// named once, with how many times the replay made it, and only the first
// missConsoleMax of them; the console then says how many more there are and
// names the file that holds every one whole beside its tag (MissesFile), so
// the call behind each tag the log names is still found. A connection that
// ended with nothing sent made no call, and is no mismatch on the console:
// its line says what it was. A replay whose file could not be written names
// every call on its console.
func TestAMockReplaysConsoleNamesEachCallOnceAndAtMostSoMany(t *testing.T) {
	var misses []models.UnmatchedCall
	call := func(i int) models.UnmatchedCall {
		return models.UnmatchedCall{Protocol: "HTTP", ActualSummary: fmt.Sprintf("GET /v1/users/%d", i), Destination: "api:80",
			PathTag: fmt.Sprintf("#%012d", i), NextSteps: "record it"}
	}
	for i := 0; i < missConsoleMax+5; i++ {
		misses = append(misses, call(i))
	}
	// The first call twice more, and a connection that sent nothing.
	misses = append(misses, call(0), call(0), models.UnmatchedCall{Protocol: "Generic", Destination: "db:5432", MatchPhase: models.MatchPhaseNoRequest})

	keployDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(keployDir, "set"), 0o755); err != nil {
		t.Fatal(err)
	}
	file := writeMissesFile(zap.NewNop(), keployDir, "set", misses)
	if file != filepath.Join(keployDir, "set", MissesFile) {
		t.Fatalf("the misses file is %q", file)
	}
	var console bytes.Buffer
	sayMisses(&console, misses, file)
	out := console.String()
	if n := strings.Count(out, "  Mock mismatch: "); n != missConsoleMax {
		t.Errorf("the console named %d calls, want %d:\n%s", n, missConsoleMax, out)
	}
	if !strings.Contains(out, "  Mock mismatch: [HTTP] GET /v1/users/0 [path tag #000000000000]  →  api:80   (3 calls)\n") {
		t.Errorf("the console did not name the repeated call once with its count:\n%s", out)
	}
	if want := "  and 5 more calls no mock answered (5 mismatches): " + file + " names each whole, beside its path tag\n"; !strings.HasSuffix(out, want) {
		t.Errorf("the console ends\n%s\nwant %q", out[max(0, len(out)-300):], want)
	}
	if strings.Contains(out, "db:5432") || strings.Contains(out, "[Generic]") {
		t.Errorf("a connection that sent nothing is named as a mismatch:\n%s", out)
	}
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var got missesReport
	if err := yaml.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Misses) != missConsoleMax+5 || got.Misses[0].Count != 3 || got.Misses[0].Call != "GET /v1/users/0" || got.Misses[0].PathTag != "#000000000000" ||
		got.Misses[missConsoleMax+4].Call != fmt.Sprintf("GET /v1/users/%d", missConsoleMax+4) || got.Misses[missConsoleMax+4].PathTag != fmt.Sprintf("#%012d", missConsoleMax+4) {
		t.Errorf("the misses file holds %+v, want every call once, whole, beside its tag", got.Misses)
	}

	// The file holds the calls whole: keploy/.gitignore ignores it, as it
	// ignores the receipts.
	if gi, err := os.ReadFile(filepath.Join(keployDir, ".gitignore")); err != nil || !strings.Contains(string(gi), "/*/"+MissesFile+"\n") {
		t.Errorf("keploy/.gitignore does not ignore the misses file: %q (%v)", gi, err)
	}

	// No file: every call on the console.
	console.Reset()
	sayMisses(&console, misses, "")
	if n := strings.Count(console.String(), "  Mock mismatch: "); n != missConsoleMax+5 || strings.Contains(console.String(), "more calls") {
		t.Errorf("with no file the console named %d calls, want all %d", n, missConsoleMax+5)
	}

	// The next replay's file stands in place of the last one's, never
	// added to it.
	if writeMissesFile(zap.NewNop(), keployDir, "set", []models.UnmatchedCall{call(7)}) != file {
		t.Fatal("the next replay wrote no misses file")
	}
	b, err = os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	got = missesReport{}
	if err := yaml.Unmarshal(b, &got); err != nil || len(got.Misses) != 1 || got.Misses[0].Call != "GET /v1/users/7" || strings.Count(string(b), "# Written by") != 1 {
		t.Errorf("the next replay's misses file holds %+v (%v), want its one call alone", got.Misses, err)
	}

	// A replay that missed nothing removes the last one's file.
	if writeMissesFile(zap.NewNop(), keployDir, "set", nil) != "" {
		t.Error("a replay with no miss pointed to a misses file")
	}
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Errorf("a replay with no miss left the last one's misses file: %v", err)
	}
}

// The misses file holds the calls whole, so it is never written where git
// would not ignore it: a keploy/.gitignore keploy cannot add its entry to
// (here a folder stands at its path) leaves no file, and the console names
// every call.
func TestTheMissesFileIsNeverWrittenUnignored(t *testing.T) {
	keployDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(keployDir, "set"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(keployDir, ".gitignore"), 0o755); err != nil {
		t.Fatal(err)
	}
	miss := models.UnmatchedCall{Protocol: "HTTP", ActualSummary: "GET /v1/users/1", PathTag: "#000000000001"}
	if file := writeMissesFile(zap.NewNop(), keployDir, "set", []models.UnmatchedCall{miss}); file != "" {
		t.Fatalf("the misses file was written at %s with no .gitignore entry for it", file)
	}
	if _, err := os.Stat(filepath.Join(keployDir, "set", MissesFile)); !os.IsNotExist(err) {
		t.Fatalf("a misses file git would not ignore is on disk: %v", err)
	}
}

// gitRepo is dir made a git work tree (git init), for git to say what it
// ignores there. Git is needed: keploy asks it whether the files that hold
// calls whole are ignored (utils.GitIgnores).
func gitRepo(t *testing.T, dir string) {
	t.Helper()
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatalf("this test needs git, to read what git ignores: %v", err)
	}
	if out, err := exec.Command(git, "init", "-q", dir).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
}

// keploy/.gitignore's entries for the misses file and its write's temporary
// file are not enough where a later line of the user's un-ignores either
// ("!*.yaml", "!.last-replay-*"): git, which `git add` reads by, would track
// it, so keploy asks git, and writes no file (the console then names every
// call), removing the last one. With the entries alone the file is written.
func TestTheMissesFileIsNotWrittenWhereGitWouldTrackIt(t *testing.T) {
	miss := models.UnmatchedCall{Protocol: "HTTP", ActualSummary: "GET /v1/reset/t0k3n", PathTag: "#000000000001"}
	for name, after := range map[string]string{
		"with the entries alone":                "",
		"un-ignoring every yaml file":           "!*.yaml\n",
		"un-ignoring the temporary file":        "!.last-replay-*\n",
		"un-ignoring the set's own misses file": "!/default/" + MissesFile + "\n",
	} {
		t.Run(name, func(t *testing.T) {
			repo := t.TempDir()
			gitRepo(t, repo)
			keployDir := filepath.Join(repo, "keploy")
			if err := os.MkdirAll(filepath.Join(keployDir, "default"), 0o755); err != nil {
				t.Fatal(err)
			}
			ignoreReceipts(zap.NewNop(), keployDir)
			f, err := os.OpenFile(filepath.Join(keployDir, ".gitignore"), os.O_APPEND|os.O_WRONLY, 0o644)
			if err != nil {
				t.Fatal(err)
			}
			_, err = f.WriteString(after)
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				t.Fatal(err)
			}
			stale := filepath.Join(keployDir, "default", MissesFile)
			if err := os.WriteFile(stale, []byte("misses: [an earlier run's]\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			core, logs := observer.New(zapcore.WarnLevel)
			file := writeMissesFile(zap.New(core), keployDir, "default", []models.UnmatchedCall{miss})
			if after == "" {
				if file != stale {
					t.Fatalf("the misses file was not written where git ignores it: %q", file)
				}
				b, _ := os.ReadFile(stale)
				if !strings.Contains(string(b), "GET /v1/reset/t0k3n") {
					t.Fatalf("the misses file holds\n%s\nwant this run's call", b)
				}
				return
			}
			if file != "" {
				t.Fatalf("the misses file was written at %s, which git does not ignore", file)
			}
			if _, err := os.Lstat(stale); !os.IsNotExist(err) {
				t.Fatalf("the last run's misses file stands where git would track it: %v", err)
			}
			entries, _ := os.ReadDir(filepath.Join(keployDir, "default"))
			if len(entries) != 0 {
				t.Fatalf("the set's folder holds %v, want nothing written", entries)
			}
			said := logs.FilterMessage("could not write the calls no mock answered in this replay; the console names each").All()
			if len(said) != 1 || !strings.Contains(said[0].ContextMap()["error"].(string), "git does not ignore") ||
				!strings.Contains(said[0].ContextMap()["next_step"].(string), "un-ignores it") {
				t.Fatalf("the replay said %+v, want why: git does not ignore it", said)
			}
		})
	}
}

// Both of keploy/.gitignore's entries, the misses file's and its write's
// temporary file's, are there before anything is written: a keploy/.gitignore
// that holds the misses file's entry and cannot take the temporary file's
// leaves no file, and no temporary file either (a kill between its creation
// and its rename would leave the calls whole in a file git does not ignore).
func TestTheMissesFileIsWrittenOnlyOnceBothEntriesAreIgnored(t *testing.T) {
	keployDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(keployDir, "set"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(keployDir, ".gitignore"), []byte("/*/"+MissesFile+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	was := addGitIgnore
	addGitIgnore = func(logger *zap.Logger, path, entry string) error {
		if entry == "/*/"+receiptTempPrefix+"*" {
			return fmt.Errorf("keploy/.gitignore is read-only")
		}
		return was(logger, path, entry)
	}
	t.Cleanup(func() { addGitIgnore = was })
	miss := models.UnmatchedCall{Protocol: "HTTP", ActualSummary: "GET /v1/reset/t0k3n", PathTag: "#000000000001"}
	if file := writeMissesFile(zap.NewNop(), keployDir, "set", []models.UnmatchedCall{miss}); file != "" {
		t.Fatalf("the misses file was written at %s with its temporary file's entry missing", file)
	}
	entries, err := os.ReadDir(filepath.Join(keployDir, "set"))
	if err != nil || len(entries) != 0 {
		t.Fatalf("the set's folder holds %v (%v), want nothing written", entries, err)
	}
}

// A replay that fails before keploy judged what its tests did (the agent's
// windows cannot be read) leaves no misses file: the last run's, which names
// that run's calls under that run's key, must not stand beside a receipt that
// says this run missed nothing it could count.
func TestAFailedReplayRemovesTheLastRunsMissesFile(t *testing.T) {
	provenOn(t)
	instr := newInstr(t, agentUpFromSetup, false, models.AppError{AppErrorType: models.ErrAppStopped})
	instr.windowsErr = errors.New("connection reset by peer")
	var stale string
	err := replayThrough(t, instr, instr, utils.Native, "", func(c *config.Config) {
		stale = filepath.Join(c.Path, "set", MissesFile)
		if err := os.WriteFile(stale, []byte("misses: [an earlier run's]\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	})
	require.ErrorContains(t, err, "keploy could not read what each test did from its agent")
	if _, err := os.Lstat(stale); !os.IsNotExist(err) {
		t.Fatalf("the failed replay left the last run's misses file: %v", err)
	}
}

// A mock service as New builds it has no console: it names a call no mock
// answered nowhere, and the misses file alone holds it. keploy's CLI sets
// os.Stdout (SetConsole).
func TestAMockServiceBuiltWithDefaultsNamesNoCallOnAConsole(t *testing.T) {
	m := New(zap.NewNop(), nil, nil, nil, nil, nil, &config.Config{}).(*mockService)
	if m.console != nil {
		t.Fatalf("a mock service built with defaults has console %v, want none", m.console)
	}
	var console bytes.Buffer
	m.SetConsole(&console)
	sayMisses(m.console, []models.UnmatchedCall{{Protocol: "HTTP", ActualSummary: "GET /v1/reset/t0k3n", PathTag: "#000000000001"}}, "")
	if !strings.Contains(console.String(), "  Mock mismatch: [HTTP] GET /v1/reset/t0k3n [path tag #000000000001]") {
		t.Fatalf("the console it was given said %q", console.String())
	}
}

// keploy mock replay names a call's destination whole beside the host tag
// the agent's line named it by, on its console and in its misses file
// (host_tag); its own line names the destination by its shape beside that
// tag (a host_tag field), never whole.
func TestAMockReplayMissNamesItsDestinationBesideItsHostTag(t *testing.T) {
	miss := models.UnmatchedCall{Protocol: "HTTP", ActualSummary: "GET /v1/items", Destination: "api.example.com:443", HostTag: "#3fa2c1d0e9b8", PathTag: "#1f2e3d4c5b6a", NextSteps: "record it"}
	var console bytes.Buffer
	sayMisses(&console, []models.UnmatchedCall{miss}, "")
	if !strings.Contains(console.String(), "  Mock mismatch: [HTTP] GET /v1/items [path tag #1f2e3d4c5b6a]  →  api.example.com:443 [host tag #3fa2c1d0e9b8]\n") {
		t.Errorf("the console said\n%s", console.String())
	}
	core, logs := observer.New(zapcore.DebugLevel)
	logMiss(zap.New(core), miss)
	f := logs.All()[0].ContextMap()
	if f["destination"] != "{}.{}.{}:443" || f["host_tag"] != "#3fa2c1d0e9b8" {
		t.Errorf("the line is %v, want the destination's shape beside its host tag", f)
	}
	keployDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(keployDir, "set"), 0o755); err != nil {
		t.Fatal(err)
	}
	file := writeMissesFile(zap.NewNop(), keployDir, "set", []models.UnmatchedCall{miss})
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var got missesReport
	if err := yaml.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Misses) != 1 || got.Misses[0].Destination != "api.example.com:443" || got.Misses[0].HostTag != "#3fa2c1d0e9b8" {
		t.Errorf("the misses file holds %+v, want the destination whole beside its host tag", got.Misses)
	}
}

// The misses file, and the temporary file a write of it goes through (a kill
// before its rename leaves one), hold the calls whole: HoldsCallsWhole names
// both, for anything that sends the keploy folder away to leave them out.
// The receipts beside them, which name calls by their shape, and the set's
// recorded files are not named.
func TestHoldsCallsWholeNamesTheMissesFileAndItsTemporaryFile(t *testing.T) {
	keployDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(keployDir, "set"), 0o755); err != nil {
		t.Fatal(err)
	}
	miss := models.UnmatchedCall{Protocol: "HTTP", ActualSummary: "GET /v1/reset/t0k3n", PathTag: "#000000000001"}
	file := writeMissesFile(zap.NewNop(), keployDir, "set", []models.UnmatchedCall{miss})
	if file == "" {
		t.Fatal("the misses file was not written")
	}
	writeTestsReceipt(zap.NewNop(), keployDir, TestsReceipt{Set: "set", Tests: []TestReceipt{{Name: "TestReset"}}})
	whole := 0
	err := filepath.WalkDir(keployDir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		holds := strings.Contains(string(b), "/v1/reset/t0k3n")
		if holds {
			whole++
		}
		if HoldsCallsWhole(d.Name()) != holds {
			t.Errorf("HoldsCallsWhole(%q) = %v, but the file holds the call whole: %v", d.Name(), !holds, holds)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if whole != 1 {
		t.Fatalf("%d files hold the call whole, want the misses file alone", whole)
	}
	// os.CreateTemp's name for the write's temporary file, and the receipts'.
	for name, want := range map[string]bool{
		".last-replay-misses-2716057188.tmp": true,
		".last-replay-2716057188.tmp":        false,
		ReceiptFile:                          false,
		TestsReceiptFile:                     false,
		RecordTestsReceiptFile:               false,
		"mocks.yaml":                         false,
	} {
		if got := HoldsCallsWhole(name); got != want {
			t.Errorf("HoldsCallsWhole(%q) = %v, want %v", name, got, want)
		}
	}
}
