//go:build linux

// Package structs provides data structures for hooks.
package structs

type BpfSpinLock struct{ Val uint32 }

type DestInfo struct {
	IPVersion uint32
	DestIP4   uint32
	DestIP6   [4]uint32
	DestPort  uint32
	KernelPid uint32
	ClientID  uint64
}

type ProxyInfo struct {
	IP4  uint32
	IP6  [4]uint32
	Port uint32
}

type DockerAppInfo struct {
	AppInode uint64
	ClientID uint64
}

type ClientInfo struct {
	KeployClientInode        uint64
	KeployClientNsPid        uint32
	Mode                     uint32
	IsDockerApp              uint32
	IsKeployClientRegistered uint32
	PassThroughPorts         [10]int32
	AppInode                 uint64
}

type AgentInfo struct {
	KeployAgentNsPid uint32
	DNSPort          int32
	KeployAgentInode uint64
}

type TestBenchInfo struct {
	KTestClientPID     uint32
	KTestAgentPID      uint32
	KRecordAgentPID    uint32
	KTestAgentClientId uint64
	_                  uint32 // Padding to ensure total size is 24 bytes
}
