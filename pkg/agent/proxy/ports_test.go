package proxy

import (
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"

	"go.uber.org/zap"

	"go.keploy.io/server/v3/utils"
)

// A port another socket took after the CLI handed it over fails the start of
// an agent whose proxy may not move (no SetPortMover: a native agent off
// Linux) as utils.ErrPortTaken, naming the port: the CLI starts the agent
// again at once, on ports it picks again. Forced here: the port is held by a
// real listener before the proxy binds it.
func TestATakenPortThatMayNotMoveFailsAsOneToStartAgain(t *testing.T) {
	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = held.Close() })
	port := uint32(held.Addr().(*net.TCPAddr).Port)
	p := &Proxy{logger: zap.NewNop(), Port: port}
	err = p.bindOrMove("proxy", &p.Port, func(port uint32) error {
		ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err == nil {
			_ = ln.Close()
		}
		return err
	}, func() {})
	if !errors.Is(err, utils.ErrPortTaken) || !utils.IsAddrInUse(err) || !strings.Contains(err.Error(), fmt.Sprintf("the proxy's port %d is taken", port)) {
		t.Fatalf("bindOrMove on a held port: %v; want utils.ErrPortTaken naming port %d", err, port)
	}
	// Any other failure of the bind is not one a fresh start would change.
	other := errors.New("bind refused")
	if err := p.bindOrMove("proxy", &p.Port, func(uint32) error { return other }, func() {}); !errors.Is(err, other) || errors.Is(err, utils.ErrPortTaken) {
		t.Fatalf("bindOrMove: %v; want the bind's own error, untagged", err)
	}
}
