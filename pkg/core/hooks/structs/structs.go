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
