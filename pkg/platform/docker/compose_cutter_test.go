package docker

import (
	"os"
	"strings"
	"testing"

	"go.uber.org/zap/zapcore"
)

// plantedComposeValue is what the test cutter cuts; never a real secret.
const plantedComposeValue = "planted-compose-s3cr3t"

// composeCutter is the redactor the app-output tests install, so a test can
// assert that what a container prints is CUT before it is kept or printed.
//
// It lived in compose_log_cut_test.go beside the test of the compose library's
// own log consumer. That consumer went with the library; this helper did not,
// because the tail it serves is still read by the shell-out's output capture
// (ComposeChildOutput) and by apptail_test.
type composeCutter struct{}

func (composeCutter) RedactEntry(*zapcore.Entry)       {}
func (composeCutter) RedactField(*zapcore.Field)       {}
func (composeCutter) RedactEncoded(text string) string { return text }
func (composeCutter) CutsChildOutput() bool            { return true }
func (composeCutter) HoldChildOutput(string) int       { return 0 }
func (composeCutter) RedactChildOutput(line string) string {
	return strings.ReplaceAll(line, plantedComposeValue, "***")
}

// unsetEnv removes keys for one test and restores them after, which t.Setenv
// alone cannot do: it can only set a key to "", and compose tells an unset
// variable apart from an empty one.
//
// It lived in composelib_env_test.go, which went with the library; the
// compose-child output tests still need it to drive the project-name
// precedence with nothing inherited from the runner's environment.
func unsetEnv(t *testing.T, keys ...string) {
	t.Helper()
	for _, k := range keys {
		t.Setenv(k, "")
		if err := os.Unsetenv(k); err != nil {
			t.Fatal(err)
		}
	}
}
