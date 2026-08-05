package provider

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func cmdWithCmdTypeFlag(explicit string) *cobra.Command {
	cmd := &cobra.Command{Use: "test"}
	cmd.Flags().String("cmd-type", "", "")
	if explicit != "" {
		_ = cmd.Flags().Set("cmd-type", explicit)
	}
	return cmd
}

// TestResolveCommandType_ExplicitValid asserts that an explicitly-provided,
// valid --cmd-type wins over whatever auto-detection would have said for the
// given command string.
func TestResolveCommandType_ExplicitValid(t *testing.T) {
	cmd := cmdWithCmdTypeFlag("docker-compose")

	got, err := resolveCommandType(cmd, "python app.py", "docker-compose")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "docker-compose" {
		t.Errorf("got %q, want %q", got, "docker-compose")
	}
}

// TestResolveCommandType_ExplicitInvalid asserts that an explicit but
// unrecognized --cmd-type value (including the old, never-valid "docker")
// is rejected with a clear error rather than silently falling back to
// auto-detection.
func TestResolveCommandType_ExplicitInvalid(t *testing.T) {
	cmd := cmdWithCmdTypeFlag("docker")

	_, err := resolveCommandType(cmd, "python app.py", "docker")
	if err == nil {
		t.Fatal("expected an error for an invalid --cmd-type value, got nil")
	}
	if !strings.Contains(err.Error(), "docker") {
		t.Errorf("error %q does not mention the invalid value", err.Error())
	}
}

// TestResolveCommandType_NotProvided_FallsBackToAutoDetect is the regression
// guard: when the user never passes --cmd-type, behavior must be identical
// to today's auto-detection.
func TestResolveCommandType_NotProvided_FallsBackToAutoDetect(t *testing.T) {
	cmd := cmdWithCmdTypeFlag("")

	got, err := resolveCommandType(cmd, "python app.py", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "native" {
		t.Errorf("got %q, want %q (auto-detected from a plain command string)", got, "native")
	}
}
