package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/spf13/pflag"

	"go.keploy.io/server/v3/utils"
)

// TestExitCodeForCmdErr pins the CLI's exit-code contract: any error out of
// rootCmd.Execute() has to produce a non-zero exit. Before this was
// centralised, only "unknown command"/"unknown shorthand" did, so every
// flag-parsing error left utils.ErrCode at 0 and `keploy test --typo`
// reported success to the shell and to CI while printing a red error.
// An unknown command/verb is further distinguished as a usage error
// (utils.ExitUsageError, 8) so CI can tell a mistyped invocation from a real failure
// (1) — this is what stops `keploy mock bogus` exiting 0 (design §P0b).
func TestExitCodeForCmdErr(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		wantCode int
		wantHint bool
		// wantSaid: the error itself is printed here, as nothing printed it
		// where it was raised (a flag error's flag-error func prints its own).
		wantSaid bool
	}{
		{
			name:     "success",
			err:      nil,
			wantCode: 0,
		},
		{
			name:     "unknown top-level command is a usage error (8) and hints",
			err:      errors.New(`unknown command "recrd" for "keploy"`),
			wantCode: utils.ExitUsageError,
			wantHint: true,
			wantSaid: true,
		},
		{
			name:     "unknown mock verb is a usage error (8) and hints",
			err:      errors.New(`unknown command "bogus" for "keploy mock"`),
			wantCode: utils.ExitUsageError,
			wantHint: true,
			wantSaid: true,
		},
		{
			name:     "a flag error tagged where it was parsed is a usage error and hints",
			err:      utils.UsageError(&pflag.NotExistError{}),
			wantCode: utils.ExitUsageError,
			wantHint: true,
		},
		{
			name:     "an argument-count error tagged where it was raised is a usage error, said and hinted",
			err:      utils.UsageError(errors.New("accepts at most 2 arg(s), received 3")),
			wantCode: utils.ExitUsageError,
			wantHint: true,
			wantSaid: true,
		},
		{
			// keploy reading a flag it never defined: pflag's type, not tagged.
			name:     "an untagged flag error is keploy's own failure",
			err:      &pflag.NotExistError{},
			wantCode: 1,
		},
		{
			name:     "arbitrary command failure exits non-zero",
			err:      errors.New("failed to record: something went wrong"),
			wantCode: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out bytes.Buffer
			got := exitCodeForCmdErr(tt.err, &out)
			if got != tt.wantCode {
				t.Errorf("exit code = %d, want %d", got, tt.wantCode)
			}
			hinted := strings.Contains(out.String(), "Run 'keploy --help' for usage.")
			if hinted != tt.wantHint {
				t.Errorf("usage hint printed = %v, want %v (output: %q)", hinted, tt.wantHint, out.String())
			}
			said := strings.Contains(out.String(), "Error: ")
			if said != tt.wantSaid {
				t.Errorf("error printed = %v, want %v (output: %q)", said, tt.wantSaid, out.String())
			}
		})
	}
}

// A command that mirrors the wrapped runner's exit code AND returns an error
// keeps the runner's code: it is already non-zero, and it says more than 1.
// The two used to race -- a compose project that crashed reported 7 or 1
// depending on which path noticed first.
func TestFinalExitCodeKeepsAMirroredRunnerCode(t *testing.T) {
	var out bytes.Buffer
	for _, tc := range []struct {
		name    string
		err     error
		current int
		want    int
	}{
		{"no error, nothing mirrored", nil, 0, 0},
		{"no error, runner mirrored", nil, 7, 7},
		{"error, nothing mirrored", errors.New("failed to bring up the compose project"), 0, 1},
		{"error, runner mirrored", errors.New("failed to bring up the compose project"), 7, 7},
		{"error, runner mirrored 1", errors.New("boom"), 1, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := finalExitCode(tc.err, tc.current, &out); got != tc.want {
				t.Fatalf("finalExitCode(%v, %d) = %d, want %d", tc.err, tc.current, got, tc.want)
			}
		})
	}
}
