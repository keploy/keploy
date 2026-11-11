package utils

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// A word a POSIX shell would change is single-quoted, a ' in it closed,
// escaped and reopened; a plain one stands as it is.
func TestShellWordQuotesWhatAShellWouldChange(t *testing.T) {
	for in, want := range map[string]string{
		"test-set-0":           "test-set-0",
		"/home/u/app/keploy":   "/home/u/app/keploy",
		"name=test-set-0":      "name=test-set-0",
		"":                     "''",
		"go run ./cmd/api":     "'go run ./cmd/api'",
		`bash -c "echo $PORT"`: `'bash -c "echo $PORT"'`,
		"it's":                 `'it'\''s'`,
		"a`b`":                 "'a`b`'",
		`a\tb`:                 `'a\tb'`,
		"$PORT":                "'$PORT'",
	} {
		if got := ShellWord(in); got != want {
			t.Errorf("ShellWord(%q) = %q; want %q", in, got, want)
		}
	}
}

// The command moves the set's folder aside, into its own new folder of
// keploy-replaced beside the keploy folder, and records under the set's name,
// with the app's command, and with --path and --config-path only where they
// are not the folder the command runs from.
func TestRecordSetAgainRecordsTheSetInItsPlace(t *testing.T) {
	for _, c := range []struct {
		name                           string
		keployDir, configDir, cwd, app string
		want, replaced                 string
	}{
		{"in the cwd", "/w/app/keploy", "/w/app", "/w/app", "go run ./cmd/api",
			"mkdir -p -- /w/app/keploy-replaced && mkdir -- /w/app/keploy-replaced/test-set-0-test-run-3 && mv -- /w/app/keploy/test-set-0 /w/app/keploy-replaced/test-set-0-test-run-3/test-set-0 && keploy record -c 'go run ./cmd/api' --metadata name=test-set-0",
			"/w/app/keploy-replaced/test-set-0-test-run-3"},
		{"another path and config", "/w/my tests/keploy", "/w/conf", "/w/app", `bash -c "echo $PORT"`,
			`mkdir -p -- '/w/my tests/keploy-replaced' && mkdir -- '/w/my tests/keploy-replaced/test-set-0-test-run-3' && mv -- '/w/my tests/keploy/test-set-0' '/w/my tests/keploy-replaced/test-set-0-test-run-3/test-set-0' && keploy record -c 'bash -c "echo $PORT"' --path '/w/my tests' --config-path /w/conf --metadata name=test-set-0`,
			"/w/my tests/keploy-replaced/test-set-0-test-run-3"},
		{"no config path", "/w/t/keploy", "", "/w/app", "./app",
			"mkdir -p -- /w/t/keploy-replaced && mkdir -- /w/t/keploy-replaced/test-set-0-test-run-3 && mv -- /w/t/keploy/test-set-0 /w/t/keploy-replaced/test-set-0-test-run-3/test-set-0 && keploy record -c ./app --path /w/t --metadata name=test-set-0",
			"/w/t/keploy-replaced/test-set-0-test-run-3"},
		{"no app command", "/w/app/keploy", "/w/app", "/w/app", "  ",
			"mkdir -p -- /w/app/keploy-replaced && mkdir -- /w/app/keploy-replaced/test-set-0-test-run-3 && mv -- /w/app/keploy/test-set-0 /w/app/keploy-replaced/test-set-0-test-run-3/test-set-0 && keploy record --metadata name=test-set-0",
			"/w/app/keploy-replaced/test-set-0-test-run-3"},
		{"a cwd with a trailing slash", "/w/app/keploy", "/w/app", "/w/app/", "./app",
			"mkdir -p -- /w/app/keploy-replaced && mkdir -- /w/app/keploy-replaced/test-set-0-test-run-3 && mv -- /w/app/keploy/test-set-0 /w/app/keploy-replaced/test-set-0-test-run-3/test-set-0 && keploy record -c ./app --metadata name=test-set-0",
			"/w/app/keploy-replaced/test-set-0-test-run-3"},
		{"a trailing slash", "/w/app/keploy/", "/w/app/", "/w/app", "./app",
			"mkdir -p -- /w/app/keploy-replaced && mkdir -- /w/app/keploy-replaced/test-set-0-test-run-3 && mv -- /w/app/keploy/test-set-0 /w/app/keploy-replaced/test-set-0-test-run-3/test-set-0 && keploy record -c ./app --metadata name=test-set-0",
			"/w/app/keploy-replaced/test-set-0-test-run-3"},
	} {
		got := RecordSetAgain(c.keployDir, c.configDir, c.cwd, "test-set-0", "test-run-3", c.app)
		if got.Command != c.want || got.Replaced != c.replaced {
			t.Errorf("%s:\n got %+v\nwant %s in %s", c.name, got, c.want, c.replaced)
		}
	}
	a := RecordAgain{Command: "x", Replaced: "/w/app/keploy-replaced/s-r"}
	if got, want := a.Text(), "x (it first moves the set's recordings out of the keploy folder, into /w/app/keploy-replaced/s-r; delete that folder once the set recorded again passes)"; got != want {
		t.Errorf("Text:\n got %s\nwant %s", got, want)
	}
	if got := (RecordAgain{Command: "keploy mock record --name s"}).Text(); got != "keploy mock record --name s" {
		t.Errorf("Text of a command that moves nothing: %q", got)
	}
	if got := (RecordAgain{Replaced: "/w"}).Text(); got != "" {
		t.Errorf("Text of no command: %q", got)
	}
}

// keploy prints no command it cannot name the set's folder, or a folder of
// its own for the run, by, nor one that would record the set under another
// name: such a command would move another folder, or leave the set as it was.
func TestRecordSetAgainFailsClosedOnASetItCannotName(t *testing.T) {
	// " lead": keploy record trims the space after name= (TrimSpaces) and
	// records "lead"; `a\`: no folder name on Windows.
	for _, set := range []string{"", ".", "..", "a/b", `a\b`, `a\`, `\`, "../x", "a,b", "123", "0", "true", "null", "x, y", " lead"} {
		if got := RecordSetAgain("/w/app/keploy", "/w/app", "/w/app", set, "test-run-1", "./app"); got != (RecordAgain{}) {
			t.Errorf("set %q: got %+v; want no command", set, got)
		}
	}
	for _, run := range []string{"", "a/b", `a\b`, "../x"} {
		if got := RecordSetAgain("/w/app/keploy", "/w/app", "/w/app", "test-set-0", run, "./app"); got != (RecordAgain{}) {
			t.Errorf("run %q: got %+v; want no command", run, got)
		}
	}
	if got := RecordSetAgain("", "/w/app", "/w/app", "test-set-0", "test-run-1", "./app"); got != (RecordAgain{}) {
		t.Errorf("no keploy folder: got %+v; want no command", got)
	}
	if got := RecordSetAgain("/w/app/keploy", "/w/app", "/w/app", "orders 2", "test-run-1", "./app").Command; got != "mkdir -p -- /w/app/keploy-replaced && mkdir -- '/w/app/keploy-replaced/orders 2-test-run-1' && mv -- '/w/app/keploy/orders 2' '/w/app/keploy-replaced/orders 2-test-run-1/orders 2' && keploy record -c ./app --metadata 'name=orders 2'" {
		t.Errorf("a name with a space: got %q", got)
	}
}

// The command, run by a POSIX shell, moves the set's recordings into the
// run's folder of keploy-replaced and runs keploy record with every word as
// keploy printed it; run again (the run's folder is there now), mkdir stops
// it, saying so, before it moves the set recorded again or touches the old
// recordings, and keploy record does not run.
func TestRecordSetAgainRunsInAShellAndNeverOverwrites(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("a POSIX shell command")
	}
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	root := filepath.Join(t.TempDir(), "my app's $HOME")
	set := "orders it's"
	keployDir := filepath.Join(root, "keploy")
	write := func(p, s string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(keployDir, set, "tests", "test-1.yaml"), "old")
	bin := filepath.Join(t.TempDir(), "bin")
	argsFile := filepath.Join(bin, "args")
	// keploy record, as far as the command needs it: it says its words, one
	// a line, and records the set (here: one file) under the name it got.
	write(filepath.Join(bin, "keploy"), "#!/bin/sh\nfor a in \"$@\"; do printf '%s\\n' \"$a\"; done >> '"+argsFile+"'\nmkdir -p \"$KEPLOY_SETS/$SET/tests\" && echo new > \"$KEPLOY_SETS/$SET/tests/test-1.yaml\"\n")
	if err := os.Chmod(filepath.Join(bin, "keploy"), 0o755); err != nil {
		t.Fatal(err)
	}
	a := RecordSetAgain(keployDir, root, "/", set, "test-run-7", `go run "./cmd/api"`)
	if a.Command == "" {
		t.Fatal("no command")
	}
	run := func() (string, error) {
		c := exec.Command(sh, "-c", a.Command)
		c.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"), "LC_ALL=C", "KEPLOY_SETS="+keployDir, "SET="+set)
		out, err := c.CombinedOutput()
		return string(out), err
	}
	read := func(p string) string {
		t.Helper()
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	if out, err := run(); err != nil {
		t.Fatalf("the command failed: %v: %s", err, out)
	}
	if got := read(filepath.Join(a.Replaced, set, "tests", "test-1.yaml")); got != "old" {
		t.Errorf("the old recordings in %s: %q", a.Replaced, got)
	}
	if got := read(filepath.Join(keployDir, set, "tests", "test-1.yaml")); got != "new\n" {
		t.Errorf("the set recorded again: %q", got)
	}
	if got, want := read(argsFile), strings.Join([]string{"record", "-c", `go run "./cmd/api"`, "--path", root, "--config-path", root, "--metadata", "name=" + set}, "\n")+"\n"; got != want {
		t.Errorf("keploy got:\n%s\nwant:\n%s", got, want)
	}
	if strings.HasPrefix(a.Replaced, keployDir+string(filepath.Separator)) || filepath.Dir(a.Replaced) != filepath.Join(root, ReplacedDir) {
		t.Errorf("the old recordings are not beside the keploy folder: %s", a.Replaced)
	}
	out, err := run()
	if err == nil {
		t.Fatalf("the command ran again over its own folder: %s", out)
	}
	if !strings.Contains(out, "File exists") {
		t.Errorf("mkdir did not say why it stopped: %q", out)
	}
	if got := read(filepath.Join(a.Replaced, set, "tests", "test-1.yaml")); got != "old" {
		t.Errorf("the old recordings were overwritten: %q", got)
	}
	if _, err := os.Stat(filepath.Join(a.Replaced, set, set)); !os.IsNotExist(err) {
		t.Errorf("the set recorded again was nested into the old one: %v", err)
	}
	if got := read(filepath.Join(keployDir, set, "tests", "test-1.yaml")); got != "new\n" {
		t.Errorf("the set recorded again was moved: %q", got)
	}
	if got := strings.Count(read(argsFile), "record\n"); got != 1 {
		t.Errorf("keploy record ran %d times; want once", got)
	}
}
