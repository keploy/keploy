//go:build !linux && !darwin && !windows

package relay

import (
	"context"
	"net"
	"time"
)

// resetClientOnClose: off Linux, macOS and Windows the client socket is not asked what it holds,
// so its close stays a FIN, as before.
func resetClientOnClose(context.Context, net.Conn, time.Duration) bool { return false }
