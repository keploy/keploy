package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

// TestExitCodeForCmdErr pins the CLI's exit-code contract: any error out of
// rootCmd.Execute() has to produce a non-zero exit. Before this was
// centralised, only "unknown command"/"unknown shorthand" did, so every
// flag-parsing error left utils.ErrCode at 0 and `keploy test --typo`
// reported success to the shell and to CI while printing a red error.
func TestExitCodeForCmdErr(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		wantCode int
		wantHint bool
	}{
		{
			name:     "success",
			err:      nil,
			wantCode: 0,
		},
		{
			name:     "unknown command still exits non-zero and hints",
			err:      errors.New(`unknown command "recrd" for "keploy"`),
			wantCode: 1,
			wantHint: true,
		},
		{
			name:     "unknown shorthand still exits non-zero and hints",
			err:      errors.New(`unknown shorthand flag: 'z' in -z`),
			wantCode: 1,
			wantHint: true,
		},
		{
			name:     "unknown flag exits non-zero",
			err:      errors.New(`unknown flag: --nope`),
			wantCode: 1,
		},
		{
			name:     "invalid flag value exits non-zero",
			err:      errors.New(`invalid argument "abc" for "--delay" flag: strconv.ParseUint: parsing "abc": invalid syntax`),
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
		})
	}
}
