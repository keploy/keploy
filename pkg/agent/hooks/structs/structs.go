// Package structs provides data structures for hooks.
package structs

// Bits of AgentInfo.Flags, mirrored in the BPF program as
// KEPLOY_FLAG_* in headers/k_helpers.h. Keep the two in sync.
const (
	// FlagObserveOnly (bit 0) tells the cgroup hooks to skip the proxy
	// port-remap path so traffic flows unmodified. Set by downstream
	// builds that observe traffic over a separate transport.
	FlagObserveOnly uint32 = 1 << 0

	// FlagKernelPortAlloc (bit 1) tells cgroup/bind4|6 to relocate the
	// application by setting user_port = 0 — letting the kernel's own
	// allocator pick the port — instead of guessing one in BPF. It is set
	// only once cgroup/post_bind4 AND cgroup/post_bind6 are ATTACHED,
	// because those hooks are what report the kernel's choice back to user
	// space; without them the relocated port would never reach the ingress
	// forwarder. Unset, the hooks keep their previous guess-and-check
	// behaviour, so a cgroup setup that rejects the post_bind attach
	// degrades to exactly what shipped before. (A LOAD failure is not
	// covered and cannot be: LoadAndAssign fails the whole object, as it
	// already does for every other program in it.)
	FlagKernelPortAlloc uint32 = 1 << 1
	// FlagProvenUDP (bit 2), set under the proven switch with the UDP
	// watch's programs attached (udpwatch.go): the cgroup hooks register a
	// member's non-DNS UDP sockets, report the first reply each gets in each
	// of the watch's epochs, and refuse datagrams to UDP port 443 (QUIC), so
	// clients fall back to TCP.
	FlagProvenUDP uint32 = 1 << 2
	// FlagProvenReady (bit 3), set under the proven switch with the
	// readiness watch's programs attached (readywatch.go): the hooks report a
	// member's TCP listen, and count its listens, connects, process forks and
	// execs, the evidence keploy test waits on before its first test.
	FlagProvenReady uint32 = 1 << 3
	// FlagClassifies (bit 4), set where the agent attaches sys_enter_socket
	// and k_sock_create, which classify processes into target_namespace_pids
	// and excluded_pids: k_sock_create runs only then. Never where something
	// else owns those maps (DaemonSet, per-container targeting).
	FlagClassifies uint32 = 1 << 4
)

type BpfSpinLock struct{ Val uint32 }

// DestInfo mirrors struct dest_info_t (keploy/ebpf k_structs.h): a
// redirected connection's original destination and the process that made it.
type DestInfo struct {
	IPVersion uint32
	DestIP4   uint32
	DestIP6   [4]uint32
	DestPort  uint32
	// KernelPid is the connecting process's TGID, the kernel's (the initial
	// PID namespace's).
	KernelPid uint32
	// AgentNsPid is that process's TGID in the agent's PID namespace, by
	// which the agent finds it in its own /proc: the kernel finds it by the
	// namespace's inode the agent registered (KeployAgentInode). Equal to
	// KernelPid for an agent in the initial namespace; 0 for a process outside
	// the agent's namespace, which the agent then cannot name.
	AgentNsPid uint32
	_          uint32 // dest_info_t's _pad: 8-byte sized
}

type ProxyInfo struct {
	IP4  uint32
	IP6  [4]uint32
	Port uint32
}

type ClientInfo struct {
	Mode             uint32 // 4 bytes
	ClientNSPID      uint32
	PassThroughPorts [10]int32 // 40 bytes
}

type AgentInfo struct {
	KeployAgentNsPid   uint32
	DNSPort            int32
	KeployAgentInode   uint64
	KeployAgentDev     uint64
	IsDocker           uint32
	Proxy              ProxyInfo
	Flags              uint32 // extensible flag slot consumed by the BPF cgroup hooks; set via AgentInfoCustomizer and FlagKernelPortAlloc. Matches agent_info_t.flags at offset 44 (ebpf#96).
	RecordingStartTime uint64 // boot-time NS when recording started; pre-existing processes are auto-excluded by eBPF
}
