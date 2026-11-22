//go:build linux

package utils

import "syscall"

// helperProcAttr runs a helper in a process group of its own, killed by the
// kernel when the thread that started it ends (startHelper).
func helperProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
}

// orphanHelpers is every helper TestHelperOrphanParent starts: the
// escalation tests' agents and the starved boot test's, by the test that is
// each and the variable that gives it its directory.
func orphanHelpers() []struct{ name, env string } {
	return append(unixOrphanHelpers(),
		struct{ name, env string }{"TestEscalationWhileWedgedHelper", "KEPLOY_TEST_ESCALATION_WEDGED"},
		struct{ name, env string }{"TestEscalationWhilePausingHelper", "KEPLOY_TEST_ESCALATION_PAUSING"},
		struct{ name, env string }{"TestBootStarvedHelper", "KEPLOY_TEST_BOOT_STARVED"})
}
