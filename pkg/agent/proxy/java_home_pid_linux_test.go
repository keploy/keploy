//go:build linux

package proxy

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// The JAVA_HOME auto-detect reads /proc/<pid> of the CLI only: a pid the CLI
// said in another pid namespace (a CI step container's) names another
// process here, whose JDK is not the app's, so the detect reads none.
func TestTheJavaHomeDetectReadsOnlyTheCLIsProcess(t *testing.T) {
	own := utils.OwnPIDNamespace()
	ino, boot, ok := strings.Cut(own, "@")
	if !ok {
		t.Skipf("this process cannot say its pid namespace with a boot id (%q)", own)
	}
	n, err := strconv.ParseUint(ino, 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	pid := uint32(os.Getpid())
	var gave int
	prev := setupCAForApp
	setupCAForApp = func(_ context.Context, _ *zap.Logger, _ bool, appPID int, _ string) error {
		gave = appPID
		return nil
	}
	t.Cleanup(func() { setupCAForApp = prev })
	for ns, want := range map[string]uint32{
		own:                                      pid,
		"":                                       pid, // an older CLI: the number, as before
		strconv.FormatUint(n+1, 10) + "@" + boot: 0,
		ino + "@another-boot":                    0,
		utils.ClientPIDNSNone:                    0,
	} {
		cfg := &config.Config{}
		cfg.Agent.ClientNSPID, cfg.Agent.ClientPIDNS = pid, ns
		gave = -1
		if err := New(zap.NewNop(), nil, cfg).setupCA(context.Background()); err != nil {
			t.Fatal(err)
		}
		if gave != int(want) {
			t.Errorf("the CLI's pid %d said in %q: the CA install gives the detect pid %d, want %d", pid, ns, gave, want)
		}
	}
}
