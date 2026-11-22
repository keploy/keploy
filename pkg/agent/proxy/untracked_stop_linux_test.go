//go:build linux

package proxy

import (
	"io"
	"net"
	"strconv"
	"strings"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// The total of the connections the proxy could not route is said as the
// proxy really stops, through start() as the agent stops it: the 3rd
// untracked connection was only a DEBUG, so only the stop says 3.
func TestTheUntrackedTotalIsSaidAsTheProxyStops(t *testing.T) {
	core, logs := observer.New(zapcore.DebugLevel)
	p, stop := startListening(t, nil, func(p *Proxy) { p.logger = zap.New(core) })
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(p.Listener.Addr().(*net.TCPAddr).Port))
	for i := 0; i < 3; i++ {
		c, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		// plainDestInfo names no destination: the proxy closes the
		// connection once it has counted it.
		_, _ = io.Copy(io.Discard, c)
		_ = c.Close()
	}
	stop()
	for _, e := range logs.FilterLevelExact(zapcore.WarnLevel).All() {
		if strings.Contains(e.Message, "the total as the proxy stops") && e.ContextMap()["untracked_connections"] == uint64(3) {
			return
		}
	}
	t.Fatalf("no WARN said the total, 3, as the proxy stopped: %v", logs.All())
}
