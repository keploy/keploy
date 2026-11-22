package provider

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// mockCmd carries the --from-container flag; recordCmd does not, which is the
// difference the guard under test turns on.
func mockCmd(fromContainerPassed bool) *cobra.Command {
	cmd := &cobra.Command{Use: "record"}
	cmd.Flags().String("from-container", "", "")
	cmd.Flags().String("cmd-type", "", "")
	if fromContainerPassed {
		_ = cmd.Flags().Set("from-container", "api")
	}
	_ = cmd.Flags().Set("cmd-type", string(utils.FromContainer))
	return cmd
}

func recordCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "record"}
	cmd.Flags().String("cmd-type", "", "")
	_ = cmd.Flags().Set("cmd-type", string(utils.FromContainer))
	return cmd
}

// --from-container is registered on the mock subcommands only. Accepting the
// kind on `keploy record`/`keploy test` would resolve to a mode the caller has
// no way to name a container for, and the run fails later without saying why.
func TestFromContainerCmdTypeIsRefusedWhereTheFlagDoesNotExist(t *testing.T) {
	if _, err := resolveCommandType(zap.NewNop(), recordCmd(), "node app.js", string(utils.FromContainer)); err == nil {
		t.Fatal("accepted --cmd-type from-container on a command that has no --from-container flag")
	} else if !strings.Contains(err.Error(), "keploy mock") {
		t.Errorf("error does not say where the mode is supported: %v", err)
	}

	got, err := resolveCommandType(zap.NewNop(), mockCmd(true), "", string(utils.FromContainer))
	if err != nil {
		t.Fatalf("refused it on the mock path, where it is supported: %v", err)
	}
	if got != string(utils.FromContainer) {
		t.Errorf("resolved to %q, want %q", got, utils.FromContainer)
	}
}

// The wrapper-rewrite guard exists because docker-run/docker-start splice flags
// into the command. --from-container rewrites nothing, so it must not be caught
// by it - there is not even a command to inspect.
func TestFromContainerIsNotCaughtByTheWrapperGuard(t *testing.T) {
	if _, err := resolveCommandType(zap.NewNop(), mockCmd(true), "make up", string(utils.FromContainer)); err != nil {
		t.Fatalf("the wrapper guard fired for a mode that rewrites nothing: %v", err)
	}
}
