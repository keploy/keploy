package utils

// AgentNeedsElevation reports whether the native agent must run as root on the
// given platform.
//
// Windows does not either — it has its own command builder that never sudos.
//
// Linux does: the agent loads eBPF programs, attaches cgroup hooks and mounts
// bpffs, none of which an unprivileged process can do.
//
// Darwin does not. There is no eBPF there; a build shipping a macOS
// interception backend does it from userspace inside the application's own
// process, and everything the agent binds — the proxy, the DNS server, its HTTP
// control plane — is an unprivileged port.
//
// Elevating anyway is not free. `sudo` prompts for a password on a machine
// where nothing needs one, and it splits the run across two users: the client
// and the application stay as the invoking user while the agent becomes root.
// That asymmetry breaks things that look unrelated — StopCommand's
// kill(-pgid) returns EPERM against a root process group, and $TMPDIR is
// per-user on macOS, so the two halves stop agreeing on where temporary files
// live.
//
// goos is a parameter rather than a read of runtime.GOOS so the policy can be
// exercised for every platform from any host. Asserting against the host would
// only ever cover the one row CI happens to run on, and could not catch an
// inverted platform check — the same reason cli/provider.DefaultNativeCommandSupported
// is shaped this way.
func AgentNeedsElevation(goos string) bool {
	return goos == "linux"
}
