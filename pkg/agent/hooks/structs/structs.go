// Package structs provides data structures for hooks.
package structs

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
	KeployAgentNsPid uint32
	DNSPort          int32
	KeployAgentInode uint64
	IsDocker         uint32
	Proxy            ProxyInfo
	_                [4]byte
}
