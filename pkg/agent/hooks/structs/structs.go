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
)

type BpfSpinLock struct{ Val uint32 }

type DestInfo struct {
	IPVersion uint32
	DestIP4   uint32
	DestIP6   [4]uint32
	DestPort  uint32
	KernelPid uint32
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
