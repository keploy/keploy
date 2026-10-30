//go:build !linux && !darwin

package proxy

import "syscall"

// socketReceived cannot be asked off Linux: the end count runs on Linux only
// (release 1), and elsewhere the agent cannot count the connections waiting
// for its proxy at all (models.ErrProxyConnsUnsupported), so no test is
// judged settled by it.
func socketReceived(syscall.RawConn) (received uint64, state uint8, ok bool) { return 0, 0, false }

// finReceived is never asked off Linux (socketReceived).
func finReceived(uint8, bool) bool { return false }

// clientGone: the kernel's TCP state is not read on this platform.
func clientGone(uint8) bool { return false }
