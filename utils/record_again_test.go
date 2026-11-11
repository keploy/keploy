package utils

import "testing"

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
	} {
		if got := ShellWord(in); got != want {
			t.Errorf("ShellWord(%q) = %q; want %q", in, got, want)
		}
	}
}

// The command removes the set's folder and records under its name, with the
// app's command, and with --path and --config-path only where they are not
// the folder the command runs from.
func TestRecordSetAgainCommandRecordsTheSetInItsPlace(t *testing.T) {
	for _, c := range []struct {
		name                           string
		keployDir, configDir, cwd, app string
		want                           string
	}{
		{"in the cwd", "/w/app/keploy", "/w/app", "/w/app", "go run ./cmd/api",
			"rm -rf -- /w/app/keploy/test-set-0 && keploy record -c 'go run ./cmd/api' --metadata name=test-set-0"},
		{"another path and config", "/w/my tests/keploy", "/w/conf", "/w/app", `bash -c "echo $PORT"`,
			`rm -rf -- '/w/my tests/keploy/test-set-0' && keploy record -c 'bash -c "echo $PORT"' --path '/w/my tests' --config-path /w/conf --metadata name=test-set-0`},
		{"no config path", "/w/t/keploy", "", "/w/app", "./app",
			"rm -rf -- /w/t/keploy/test-set-0 && keploy record -c ./app --path /w/t --metadata name=test-set-0"},
		{"no app command", "/w/app/keploy", "/w/app", "/w/app", "  ",
			"rm -rf -- /w/app/keploy/test-set-0 && keploy record --metadata name=test-set-0"},
		{"a trailing slash", "/w/app/keploy/", "/w/app/", "/w/app", "./app",
			"rm -rf -- /w/app/keploy/test-set-0 && keploy record -c ./app --metadata name=test-set-0"},
	} {
		if got := RecordSetAgainCommand(c.keployDir, c.configDir, c.cwd, "test-set-0", c.app); got != c.want {
			t.Errorf("%s:\n got %s\nwant %s", c.name, got, c.want)
		}
	}
}

// keploy prints no command it cannot name the set's folder by, nor one that
// would record the set under another name: such a command would remove
// another folder, or leave the set as it was.
func TestRecordSetAgainCommandFailsClosedOnASetItCannotName(t *testing.T) {
	for _, set := range []string{"", ".", "..", "a/b", `a\b`, "../x", "a,b", "123", "0", "true", "null", "x, y"} {
		if got := RecordSetAgainCommand("/w/app/keploy", "/w/app", "/w/app", set, "./app"); got != "" {
			t.Errorf("set %q: got %q; want no command", set, got)
		}
	}
	if got := RecordSetAgainCommand("", "/w/app", "/w/app", "test-set-0", "./app"); got != "" {
		t.Errorf("no keploy folder: got %q; want no command", got)
	}
	if got := RecordSetAgainCommand("/w/app/keploy", "/w/app", "/w/app", "orders 2", "./app"); got != "rm -rf -- '/w/app/keploy/orders 2' && keploy record -c ./app --metadata 'name=orders 2'" {
		t.Errorf("a name with a space: got %q", got)
	}
}
