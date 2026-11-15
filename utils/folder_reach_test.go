package utils

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestShellWordQuotesOnlyWhatAShellWouldSplitOrExpand(t *testing.T) {
	for in, want := range map[string]string{
		"/Users/ana/app/keploy":      "/Users/ana/app/keploy",
		"ana.lee@corp":               "ana.lee@corp",
		"/Users/ana/my app/keploy":   "'/Users/ana/my app/keploy'",
		"/Users/ana/it's/keploy":     `'/Users/ana/it'\''s/keploy'`,
		"/tmp/$(touch x)":            "'/tmp/$(touch x)'",
		"":                           "''",
		"/Users/ana/a+b%c:d,e=f_g-h": "/Users/ana/a+b%c:d,e=f_g-h",
	} {
		if got := shellWord(in); got != want {
			t.Errorf("shellWord(%q) = %s, want %s", in, got, want)
		}
	}
}

// The command a folder out of reach is refused with is the one the person
// runs: each word quoted where a shell would split or expand it, and $USER
// for a user keploy cannot name, which the shell fills in.
func TestAFolderOutOfReachNamesTheCommandThatGivesItBack(t *testing.T) {
	for _, tc := range []struct {
		name string
		e    FolderOutOfReachError
		want string
	}{
		{"plain", FolderOutOfReachError{Folder: "/Users/ana/app/keploy", User: "ana", Paths: 2}, "sudo chown -R ana /Users/ana/app/keploy"},
		{"a space and a quote", FolderOutOfReachError{Folder: "/Users/ana/it's mine/keploy", User: "ana lee", Paths: 1}, `sudo chown -R 'ana lee' '/Users/ana/it'\''s mine/keploy'`},
		{"no user", FolderOutOfReachError{Folder: "/Users/ana/app/keploy", Paths: 1}, "sudo chown -R $USER /Users/ana/app/keploy"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.e.FixCommand(); got != tc.want {
				t.Fatalf("FixCommand() = %q, want %q", got, tc.want)
			}
			msg := tc.e.Error()
			if !strings.HasSuffix(msg, "then run keploy again: "+tc.want) {
				t.Errorf("the error does not end with the command:\n%s", msg)
			}
			if !strings.HasPrefix(msg, fmt.Sprintf("%d path(s) in the keploy folder %s ", tc.e.Paths, tc.e.Folder)) {
				t.Errorf("the error does not say how many paths of which folder:\n%s", msg)
			}
		})
	}
}

// A caller tells the refusal apart from keploy's other errors through any
// wrapping, by its type.
func TestAFolderOutOfReachIsToldApartWhenWrapped(t *testing.T) {
	err := fmt.Errorf("record: %w", &FolderOutOfReachError{Folder: "/p/keploy", User: "ana", Paths: 1})
	var refusal *FolderOutOfReachError
	if !errors.As(err, &refusal) || refusal.Folder != "/p/keploy" {
		t.Fatalf("errors.As did not find the refusal in %v", err)
	}
}
