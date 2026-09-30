package utils

import (
	"errors"
	"fmt"
	"syscall"
	"testing"
)

// The tag a failure carries must decide the code through every wrap it
// crosses on its way to the exit: the agent's hook load wraps it, Setup wraps
// that, and the CLI that launched the agent wraps it again.
func TestExitCodeFor(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want int
	}{
		{"privileges", fmt.Errorf("%w: failed to set memlock rlimit: %w", ErrPrivilegeRequired, syscall.EPERM), ExitPrivilegeRequired},
		{"privileges, wrapped again", fmt.Errorf("failed setting up the environment: %w", fmt.Errorf("failed to hook into the app: %w", ErrPrivilegeRequired)), ExitPrivilegeRequired},
		{"environment", fmt.Errorf("%w: neither debugfs nor tracefs are mounted", ErrEnvironmentUnsupported), ExitEnvironmentUnsupported},
		{"environment, wrapped again", fmt.Errorf("failed to hook into the app: %w", fmt.Errorf("%w: could not find a non-loopback IP for the container", ErrEnvironmentUnsupported)), ExitEnvironmentUnsupported},
		// A bare permission error is NOT a privilege failure by itself: a
		// file keploy cannot write is not a capability to grant. Only the
		// code that knows the refused operation was a privileged kernel one
		// tags it.
		{"an untagged permission error", fmt.Errorf("open keploy.yml: %w", syscall.EACCES), ExitKeployError},
		{"anything else", errors.New("address already in use"), ExitKeployError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ExitCodeFor(tc.err); got != tc.want {
				t.Fatalf("ExitCodeFor(%q) = %d, want %d", tc.err, got, tc.want)
			}
		})
	}
}

// Callers switch on these codes (the VS Code extension does), so each must
// mean one thing: distinct from the others, from the enterprise build's 5, and
// from what the shell and signals already mean.
func TestKeploysExitCodesAreDistinct(t *testing.T) {
	taken := map[int]string{
		0:   "success",
		5:   "enterprise: a session is required",
		126: "shell: not executable",
		127: "shell: command not found",
	}
	for code, name := range map[int]string{
		ExitKeployError:            "ExitKeployError",
		ExitPrivilegeRequired:      "ExitPrivilegeRequired",
		ExitUnsupportedPlatform:    "ExitUnsupportedPlatform",
		ExitEnvironmentUnsupported: "ExitEnvironmentUnsupported",
	} {
		if other, clash := taken[code]; clash {
			t.Fatalf("%s = %d collides with %s", name, code, other)
		}
		if code >= 128 {
			t.Fatalf("%s = %d reads as a death by signal", name, code)
		}
		taken[code] = name
	}
}
