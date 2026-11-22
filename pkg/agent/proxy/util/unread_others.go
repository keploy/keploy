//go:build !linux

package util

import (
	"net"
	"syscall"
)

// OwnSocket finds no socket here: nothing is read past its consumer.
func OwnSocket(net.Conn) (syscall.RawConn, bool) { return nil, false }

// Received has no count here.
func Received(syscall.RawConn) (int64, bool) { return 0, false }

// ReadNow is not used here (OwnSocket finds no socket): it reads nothing.
func ReadNow(uintptr, []byte) (n int, err error, wait bool) { return 0, nil, true }
