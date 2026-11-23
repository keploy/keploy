package agent

// ProcStart is process pid's start time as keploy's agent records it for the
// process that claims an instance. The value is opaque: compare it only
// through ProcAlive, on the same host and by a reader that sees the same
// /proc. pid is in the PID namespace of the /proc read, as routes.CallerPID
// returns it.
//
// It is false when the process has exited or does not exist, and also when
// this reader cannot see it (a process in another PID namespace, /proc
// mounted with hidepid, no /proc at all) and on platforms other than Linux
// and macOS: false alone never proves the process exited. A pid that
// routes.CallerPID read in the same process is one this reader saw (CallerPID
// read its /proc entry), so for such a pid false means it exited.
//
// An exited process is one with no thread left running. On macOS that is a
// zombie. On Linux a process whose stat reads Z while another of its threads
// still runs (its main thread exited first, as in a Go program that execs
// from another thread) is alive, and so is a zombie whose threads cannot be
// read: the read fails toward alive.
func ProcStart(pid int) (uint64, bool) {
	return procStart(pid)
}

// ProcAlive reports whether process pid is still the one that started at
// start (a ProcStart value) and has not exited: a pid the kernel gave to
// another process has another start time. It is how keploy's agent decides
// that an instance's claimer still runs, and so refuses a claim from another
// process; a server that answers the agent's routes in its place (such as a
// conformance check of a test-framework integration) decides the same way.
// It is false wherever ProcStart is: also for a process this reader cannot
// see, and on platforms other than Linux and macOS.
func ProcAlive(pid int, start uint64) bool {
	return procAlive(pid, start)
}
