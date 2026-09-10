//go:build linux || darwin

package utils

import (
	"os"
	"runtime"
	"strings"
	"testing"
)

// The predicate is only useful if the command builders consult it. A test that
// merely called the predicate would still pass if someone re-inlined the old
// `os.Geteuid() != 0` condition at the call sites.
func TestAgentCommandElevatesOnlyWherePlatformRequiresIt(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root; both paths skip sudo and are indistinguishable")
	}

	elevates := AgentNeedsElevation(runtime.GOOS)

	cases := []struct {
		name string
		args []string
		want string // expected sudo flag, "" when not elevated
	}{
		{"NewAgentCommand", NewAgentCommand("/bin/true", []string{"agent"}, false).Args, ""},
		{"NewAgentCommand cached creds", NewAgentCommand("/bin/true", []string{"agent"}, true).Args, "-n"},
		{"NewAgentCommandForPTY", NewAgentCommandForPTY("/bin/true", []string{"agent"}).Args, ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if len(tc.args) == 0 {
				t.Fatal("no command built")
			}
			sudoed := strings.HasSuffix(tc.args[0], "sudo")
			if sudoed != elevates {
				t.Fatalf("sudo=%v on %s, want %v (args %v)", sudoed, runtime.GOOS, elevates, tc.args)
			}

			if !elevates {
				if tc.args[0] != "/bin/true" {
					t.Errorf("binary = %q, want the agent binary invoked directly", tc.args[0])
				}
				if len(tc.args) < 2 || tc.args[1] != "agent" {
					t.Errorf("args = %v, want the agent args passed through unchanged", tc.args)
				}
				return
			}

			// Elevated: the agent binary and its args must still survive, and
			// the cached-credentials variant must keep -n so it cannot prompt.
			rest := tc.args[1:]
			if tc.want != "" {
				if len(rest) == 0 || rest[0] != tc.want {
					t.Fatalf("args = %v, want %q immediately after sudo", tc.args, tc.want)
				}
				rest = rest[1:]
			}
			if len(rest) < 2 || rest[0] != "/bin/true" || rest[1] != "agent" {
				t.Errorf("args after sudo = %v, want the agent binary and its args", rest)
			}
		})
	}
}
