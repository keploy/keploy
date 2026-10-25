package cli

import (
	"context"

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
// CA", whose private key was public) from this host's trust store, JDK keystore
// and leftover temp files. It matches those strictly by the retired CA's
// fingerprint, so it never touches a user's own certificate. Modern keploy runs
// generate a fresh CA per run and clean it up at teardown; this command is for
// hosts that still carry the old one. It also removes, by their exact keploy
// names, the trust files keploy runs left in the temp dir (caCleanLong).
func caCleanCmd(ctx context.Context, logger *zap.Logger) *cobra.Command {
	var fresh bool
	cleanCmd := &cobra.Command{
		Use:     "clean",
		Short:   "Remove the retired static Keploy MITM CA from this host's trust store",
		Long:    caCleanLong,
		Example: "sudo keploy ca clean --fresh",
		RunE: func(_ *cobra.Command, _ []string) error {
			logger.Info("removing the retired static Keploy MITM CA (matched by fingerprint) and keploy's leftover trust files from this host")
			ptls.SweepLegacyCAAggressive(ctx, logger, fresh)
			logger.Info("done. If anything could not be removed, re-run with sudo so the system trust store is writable.")
			return nil
		},
	}
	cleanCmd.Flags().BoolVar(&fresh, "fresh", false, "rebuild the system trust bundle from scratch after removal (update-ca-certificates --fresh on Debian/Ubuntu)")
	return cleanCmd
}

// caCleanLong is `keploy ca clean --help`. It names the temp files the command
// removes without a fingerprint check, and the one risk that carries.
const caCleanLong = `Remove the retired static Keploy MITM CA from this host's trust store.

It removes the retired CA from where keploy put it: the system trust store's
anchor files, the JDK keystore's keployCA alias and the temp dir's ca.crt*
files. Each is removed only if its fingerprint is the retired CA's, never on a
name alone.

It also removes, by their exact names, the trust files keploy runs leave in
the temp dir of the user it runs as ($TMPDIR, else /tmp; sudo resets TMPDIR,
so for another dir run sudo env TMPDIR=<dir> keploy ca clean):
  - those of runs that ended without removing them (killed, crashed), in that
    user's own dir keploy-<uid> in the temp dir (keploy-<SID> on Windows).
    Each running keploy holds a lock on its own files, and those are never
    taken. A run that found that dir unusable used a dir of its own,
    keploy-<uid>-<N>, and warned: remove one a killed run left by hand;
  - those older keploy versions wrote under names without a lock, in the temp
    dir itself: keploy-ca-<N>.crt, keploy-java-cas-<N>.pem,
    keploy-java-truststore-<N>.jks and keploy-java-truststore.jks; and the
    same names in keploy-<uid>, which a run that could not lock its files
    writes. An older keploy, or one that could not lock its files, that is
    running at the same time loses its files, so run this only while no
    other keploy is running.`
