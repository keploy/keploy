//go:build linux

package proxy

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/internal/nsswitchtest"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// fedoraNsswitch is Fedora's hosts line, whose resolve module has glibc ask
// systemd-resolved, out of keploy's sight.
const fedoraNsswitch = "passwd:     files systemd\nhosts:      files myhostname resolve [!UNAVAIL=return] dns\n"

// The agent leaves the host's /etc/nsswitch.conf as it is: a replay's Mock
// writes nothing to it, and an agent killed with SIGKILL leaves nothing of
// keploy's in it. The agent used to rewrite the hosts line of the host's file
// to "files dns" at Mock and restore it at its stop, so every process on the
// host lost systemd-resolved while keploy test ran, and a killed agent left the
// host's file rewritten for good. The application keploy starts sees a view of
// its own instead (utils/appns).
//
// As root, on a host of the test's own (nsswitchtest): the machine's file is
// not touched either way.
func TestMockLeavesTheHostsNsswitchConfAsItIs(t *testing.T) {
	if nsswitchtest.Role() == "agent" {
		cfg := config.New()
		cfg.Agent.Mode = models.MODE_TEST
		p := New(zap.NewNop(), fakeDestInfo{}, cfg)
		if err := p.Mock(context.Background(), models.OutgoingOptions{Mocking: true}); err != nil {
			t.Fatalf("Mock: %v", err)
		}
		if err := os.WriteFile(os.Getenv("MOCKED"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, os.Stdin) // until the host kills it
		return
	}
	nsswitchtest.Host(t, []byte(fedoraNsswitch), func(t *testing.T) {
		mocked := filepath.Join(t.TempDir(), "mocked")
		agent := nsswitchtest.Spawn(t, "agent", "MOCKED="+mocked)
		nsswitchtest.Await(t, "the agent's Mock", func() bool {
			select {
			case <-agent.Done:
				t.Fatalf("the agent exited before its Mock returned (%v):\n%s", agent.Err, agent.Output())
			default:
			}
			_, err := os.Stat(mocked)
			return err == nil
		})
		if got := nsswitchtest.Read(nsswitchtest.Path); got != fedoraNsswitch {
			t.Errorf("the agent's Mock wrote the host's %s:\n got %q\nwant %q", nsswitchtest.Path, got, fedoraNsswitch)
		}
		if err := syscall.Kill(agent.Cmd.Process.Pid, syscall.SIGKILL); err != nil {
			t.Fatal(err)
		}
		agent.Exited(t)
		if got := nsswitchtest.Read(nsswitchtest.Path); got != fedoraNsswitch {
			t.Errorf("the host's %s is left rewritten once the agent is killed:\n got %q\nwant %q", nsswitchtest.Path, got, fedoraNsswitch)
		}
	})
}
