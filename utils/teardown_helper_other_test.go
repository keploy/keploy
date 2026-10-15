//go:build unix && !linux

package utils

import "syscall"

// helperProcAttr runs a helper in a process group of its own (startHelper);
// its stdin's end of file ends it with its test binary (no Pdeathsig here).
func helperProcAttr() *syscall.SysProcAttr { return &syscall.SysProcAttr{Setpgid: true} }

// orphanHelpers is every helper TestHelperOrphanParent starts.
func orphanHelpers() []struct{ name, env string } { return unixOrphanHelpers() }
