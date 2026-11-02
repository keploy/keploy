//go:build windows

package testnet

import (
	"os"
	"os/exec"
	"testing"

	"github.com/stretchr/testify/require"
)

// requireReserved checks that port lies below the dynamic port range that
// netsh reports, so no bind to port 0 and no connect is given it, and that
// it is claimed in os.TempDir, so no other test is given it.
func requireReserved(t *testing.T, port int) {
	t.Helper()
	var outs []string
	for _, family := range []string{"ipv4", "ipv6"} {
		out, err := exec.Command("netsh", "interface", family, "show", "dynamicportrange", "protocol=tcp").Output()
		require.NoError(t, err)
		outs = append(outs, string(out))
	}
	start, err := dynamicPortStart(outs...)
	require.NoError(t, err)
	require.Less(t, port, start, "the reserved port")
	require.GreaterOrEqual(t, port, 1024, "the reserved port")
	require.FileExists(t, claimFile(os.TempDir(), port), "the reserved port's claim")
}

// requireShared checks that port is claimed below the dynamic port range
// (requireReserved), and that no socket is at it: a listener that sets no
// option, as Go's here do, binds it at every address. On Windows SharedPort
// holds no socket at the port; it claims it.
func requireShared(t *testing.T, port int) {
	t.Helper()
	requireReserved(t, port)
	require.NoError(t, listenFree(port), "listen at the shared port %d at every address", port)
}
