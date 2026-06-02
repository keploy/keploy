package proxy

// Helpers shared by v2_integration_test.go, split out so the main
// test file stays focused on the scenarios themselves.

import (
	"net"

	"go.keploy.io/server/v3/pkg/agent/proxy/util"
)

// netPipe is a test-only alias for net.Pipe to keep the import list
// in the test file minimal.
func netPipe() (net.Conn, net.Conn) { return net.Pipe() }

// localKillSwitch is a standalone KillSwitch for tests that must not
// pollute util.DefaultKillSwitch. It returns a fresh instance.
type localKillSwitch struct {
	ks *util.KillSwitch
}

func newLocalKillSwitch() *localKillSwitch { return &localKillSwitch{ks: util.New()} }
func (l *localKillSwitch) Enabled() bool   { return l.ks.Enabled() }
func (l *localKillSwitch) Trip()           { l.ks.Trip() }
func (l *localKillSwitch) Reset()          { l.ks.Reset() }
