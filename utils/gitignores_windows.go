//go:build windows

package utils

import "syscall"

// asInvokingUser is how GitIgnores runs git: as keploy runs, on Windows,
// which has no sudo.
func asInvokingUser() *syscall.SysProcAttr { return nil }
