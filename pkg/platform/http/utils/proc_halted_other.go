//go:build !linux

package utils

// ProcessHalted says "" where the CLI never elevates an agent with sudo (the
// only agent it asks it of): it cannot tell.
func ProcessHalted(int) string { return "" }

// CommandUnderSudo returns 0 where the CLI never elevates an agent with sudo.
func CommandUnderSudo(int) int { return 0 }
