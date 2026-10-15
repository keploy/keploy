//go:build !linux

package proxy

import "syscall"

// socketUnread is 0 off Linux: the end count runs on Linux only (release 1),
// and elsewhere the agent cannot count the connections waiting for its proxy
// at all (models.ErrProxyConnsUnsupported), so no test is judged settled by
// it.
func socketUnread(syscall.RawConn) int { return 0 }
