package agent

// ProcStart is process pid's start time as keploy's agent records it for the
// process that claims an instance: clock ticks since boot on Linux
// (/proc/<pid>/stat field 22), microseconds since the epoch on macOS. pid is
// in the reader's PID namespace, as routes.CallerPID returns it. It is false
// when the process has exited (a zombie has) or does not exist, and on
// platforms other than Linux and macOS.
func ProcStart(pid int) (uint64, bool) {
	return procStart(pid)
}

// ProcAlive reports whether process pid is still the one that started at
// start (a ProcStart value) and has not exited: a pid the kernel gave to
// another process has another start time. It is how keploy's agent decides
// that an instance's claimer still runs, and so refuses a claim from another
// process; a server that answers the agent's routes in its place (such as a
// conformance check of a test-framework integration) decides the same way.
// It is false on platforms other than Linux and macOS.
func ProcAlive(pid int, start uint64) bool {
	return procAlive(pid, start)
}
