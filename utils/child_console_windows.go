//go:build windows

package utils

import (
	"io"
	"os"
)

// drainNow is never reached on Windows: its pipes take no deadline, so Drain
// returns at once and the copy goes on to the pipe's end.
func drainNow(_ *os.File, _ []byte, _ io.Writer) {}
