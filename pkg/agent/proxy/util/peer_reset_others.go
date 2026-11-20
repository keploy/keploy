//go:build !linux && !darwin

package util

import (
	"context"
	"net"
)

// AwaitPeerReset cannot ask a socket here: it waits on ctx alone.
func AwaitPeerReset(ctx context.Context, _ net.Conn) { <-ctx.Done() }
