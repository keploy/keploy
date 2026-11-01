package http

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/agent/token"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	keployLog "go.keploy.io/server/v3/utils/log"
	"go.uber.org/zap"
)

// A natively started agent is told this run's log, by its absolute path, to
// append its lines to (keployLog.ClientLogFileFlag): one run, one file. A run
// that keeps no log tells it none, and the agent keeps its own.
func TestNativeAgentArgs_NameTheCLIsLog(t *testing.T) {
	t.Cleanup(func() { _ = token.Cleanup() })
	t.Setenv("DOCKER_HOST", "unix:///run/test-docker.sock")
	prev := utils.LogFile
	t.Cleanup(func() { utils.LogFile = prev })
	a := New(zap.NewNop(), nil, &config.Config{})
	opts := models.SetupOptions{AgentPort: 26795, ProxyPort: 26796, DnsPort: 26797, AgentURI: "http://localhost:26795/agent", Mode: models.MODE_RECORD}
	flag := "--" + keployLog.ClientLogFileFlag

	utils.LogFile = nil
	if args := a.nativeAgentArgs(opts); slices.ContainsFunc(args, func(s string) bool { return strings.HasPrefix(s, flag) }) {
		t.Errorf("a run that keeps no log names one to its agent: %v", args)
	}

	// A log named relative to the working directory, as a relative TMPDIR
	// makes it, is named to the agent by its absolute path.
	dir := t.TempDir()
	t.Chdir(dir)
	f, err := os.OpenFile("keploy-logs-1.txt", os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	utils.LogFile = f
	want := flag + "=" + filepath.Join(dir, "keploy-logs-1.txt")
	if args := a.nativeAgentArgs(opts); !slices.Contains(args, want) {
		t.Errorf("the native agent argv does not name this run's log as %q: %v", want, args)
	}
}
