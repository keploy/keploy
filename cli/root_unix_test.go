//go:build unix

package cli

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/spf13/cobra"
	ptls "go.keploy.io/server/v3/pkg/agent/proxy/tls"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// caSignalHelperEnv makes TestRoot_RemovesTheCATrustFileOnASignal's helper
// run as the keploy process: the directory it reports to.
const caSignalHelperEnv = "KEPLOY_TEST_CA_SIGNAL_DIR"

// TestRoot_RemovesTheCATrustFileOnASignal: Ctrl-C (SIGINT) and SIGTERM end a
// run by cancelling its context (utils.NewCtx), and the command returns. The
// app's CA trust file must be gone then too. Run in a process of its own, as
// keploy runs: NewCtx's signal handling is process-wide.
func TestRoot_RemovesTheCATrustFileOnASignal(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGINT, syscall.SIGTERM} {
		t.Run(sig.String(), func(t *testing.T) {
			dir := t.TempDir()
			tmp := filepath.Join(dir, "tmp")
			if err := os.Mkdir(tmp, 0o700); err != nil {
				t.Fatal(err)
			}
			keploy := exec.Command(os.Args[0], "-test.run=^TestRoot_CATrustFileSignalHelper$")
			keploy.Env = append(os.Environ(), caSignalHelperEnv+"="+dir, "TMPDIR="+tmp)
			out := &bytes.Buffer{}
			keploy.Stdout, keploy.Stderr = out, out
			if err := keploy.Start(); err != nil {
				t.Fatal(err)
			}
			exited := make(chan error, 1)
			go func() { exited <- keploy.Wait() }()
			t.Cleanup(func() {
				// It starts nothing of its own, and Kill signals nothing once
				// it has been waited for.
				_ = keploy.Process.Kill()
				select {
				case <-exited:
				default:
				}
			})

			waitForMark(t, dir, "ready", exited, out)
			if left := caTrustFilesIn(t, tmp); len(left) != 1 {
				t.Fatalf("want the app's one trust file while the run is live, have %v\n%s", left, out.String())
			}
			if err := keploy.Process.Signal(sig); err != nil {
				t.Fatal(err)
			}
			if st := awaitAgentEnd(t, keploy, exited, out); !st.Exited() || st.ExitStatus() != 0 {
				t.Fatalf("keploy did not end cleanly on %s, it ended %v\n%s", sig, st, out.String())
			}
			if left := caTrustFilesIn(t, tmp); len(left) != 0 {
				t.Errorf("the app's CA trust file outlived a run ended by %s: %v\n%s", sig, left, out.String())
			}
		})
	}
}

// TestRoot_CATrustFileSignalHelper is the keploy process of
// TestRoot_RemovesTheCATrustFileOnASignal: it sets up the app's trust file,
// says so, and runs until a signal cancels its context.
func TestRoot_CATrustFileSignalHelper(t *testing.T) {
	dir := os.Getenv(caSignalHelperEnv)
	if dir == "" {
		t.Skip("only runs as TestRoot_RemovesTheCATrustFileOnASignal's keploy process")
	}
	ctx := utils.NewCtx()
	root := Root(ctx, zap.NewNop(), agentSvcFactory{}, noFlags{})
	root.AddCommand(&cobra.Command{
		Use: "run-an-app",
		RunE: func(*cobra.Command, []string) error {
			if err := ptls.SetupCaCertEnv(zap.NewNop(), testCACertPEM(t)); err != nil {
				return err
			}
			if err := os.WriteFile(filepath.Join(dir, "ready"), nil, 0o600); err != nil {
				return err
			}
			// Bounded, should the test that runs it die before it signals.
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Minute):
				return errors.New("no signal came")
			}
		},
	})
	root.SetArgs([]string{"run-an-app"})
	root.SilenceErrors, root.SilenceUsage = true, true
	_ = root.Execute()
}
