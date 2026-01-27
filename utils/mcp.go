package utils

import "sync/atomic"

var mcpStdIO atomic.Bool

// SetMCPStdio marks the process as running in MCP stdio mode.
func SetMCPStdio(enabled bool) {
	mcpStdIO.Store(enabled)
}

// IsMCPStdio reports whether the process should avoid writing to stdout.
func IsMCPStdio() bool {
	return mcpStdIO.Load()
}
