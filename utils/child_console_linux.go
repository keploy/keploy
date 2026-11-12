//go:build linux

package utils

import "golang.org/x/sys/unix"

// fionread asks a pipe how many bytes it holds (FIONREAD).
const fionread = unix.TIOCINQ
