//go:build linux

// Package structs provides data structures for hooks.
package structs

type BpfSpinLock struct{ Val uint32 }

// struct dest_info_t
// {
//     u32 ip_version;
//     u32 dest_ip4;
//     u32 dest_ip6[4];
//     u32 dest_port;
//     u32 kernelPid;
// };

type DestInfo struct {
	IPVersion uint32
	DestIP4   uint32
	DestIP6   [4]uint32
	DestPort  uint32
	KernelPid uint32
	ClientID  uint64
}

// struct proxy_info
// {
//     u32 ip4;
//     u32 ip6[4];
//     u32 port;
// };

type ProxyInfo struct {
	IP4  uint32
	IP6  [4]uint32
	Port uint32
}

type DockerAppInfo struct {
	AppInode uint64
	ClientID uint64
}

// struct app_info
// {
//     u32 keploy_client_ns_pid;
//     u64 keploy_client_inode;
//     u64 app_inode;
//     u32 mode;
//     u32 is_docker_app;
//     u32 is_keploy_client_registered; // whether the client is registered or not
//     s32 pass_through_ports[PASS_THROUGH_ARRAY_SIZE];
// };

type ClientInfo struct {
	KeployClientInode uint64 // 8 bytes
	KeployClientNsPid uint32 // 4 bytes
	Mode              uint32 // 4 bytes
	IsDockerApp       uint32 // 4 bytes

	IsKeployClientRegistered uint32    // 4 bytes
	PassThroughPorts         [10]int32 // 40 bytes
}

// struct agent_info
// {
//     u32 keploy_agent_ns_pid;
//     u32 keploy_agent_inode;
//     struct proxy_info proxy_info;
//     s32 dns_port;
// };

type AgentInfo struct {
	KeployAgentNsPid uint32
	DNSPort          int32
	KeployAgentInode uint64
	ProxyInfo        ProxyInfo
}
