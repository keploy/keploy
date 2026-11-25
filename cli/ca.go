package cli

import (
	"context"
	"runtime"

	"github.com/spf13/cobra"
	"go.keploy.io/server/v3/config"
	ptls "go.keploy.io/server/v3/pkg/agent/proxy/tls"
	"go.uber.org/zap"
)

func init() {
	Register("ca", CA)
}

// CA groups commands that manage Keploy's MITM certificate authority on this
// host. Today it carries one subcommand, `clean`, which removes the retired
// static CA that older keploy versions (and the old "bake it into your image"
// recipe) left trusted machine-wide.
func CA(ctx context.Context, logger *zap.Logger, _ *config.Config, _ ServiceFactory, _ CmdConfigurator) *cobra.Command {
	caCmd := &cobra.Command{
		Use:   "ca",
		Short: "Manage Keploy's MITM certificate authority on this host",
	}
	caCmd.AddCommand(caCleanCmd(ctx, logger))
	return caCmd
}

// caCleanCmd removes every trace of the retired static MITM CA (CN="My Custom
// CA", whose private key was public) from this host's trust store (on Windows,
// the user's and the machine's ROOT stores), JDK keystore and leftover temp
// files. It matches strictly by the retired CA's fingerprint,
// so it never touches a user's own certificate. Modern keploy runs generate a
// fresh CA per run and clean it up at teardown; this command is for hosts that
// still carry the old one.
func caCleanCmd(ctx context.Context, logger *zap.Logger) *cobra.Command {
	var fresh bool
	example, done := "sudo keploy ca clean --fresh", "done. If anything could not be removed, re-run with sudo so the system trust store is writable."
	if runtime.GOOS == "windows" {
		// No sudo: the machine's ROOT store is written from an administrator's prompt.
		example, done = "keploy ca clean   (from an administrator's prompt, to clean the machine's ROOT store too)", "done. The machine's ROOT store is changed only by keploy run from an administrator's prompt."
	}
	cleanCmd := &cobra.Command{
		Use:     "clean",
		Short:   "Remove the retired static Keploy MITM CA from this host's trust store",
		Example: example,
		RunE: func(_ *cobra.Command, _ []string) error {
			logger.Info("removing the retired static Keploy MITM CA from this host (matched by fingerprint)")
			ptls.SweepLegacyCAAggressive(ctx, logger, fresh)
			logger.Info(done)
			return nil
		},
	}
	cleanCmd.Flags().BoolVar(&fresh, "fresh", false, "rebuild the system trust bundle from scratch after removal (update-ca-certificates --fresh on Debian/Ubuntu)")
	return cleanCmd
}
