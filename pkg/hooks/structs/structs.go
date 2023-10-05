package structs

// ConnID is a conversion of the following C-Struct into GO.
type ConnID struct {
	TGID uint32
	FD   int32
	TsID uint64
}

// SockAddrIn is a conversion of the following C-Struct into GO.
type SockAddrIn struct {
	SinFamily uint16
	SinPort   uint16
	SinAddr   uint32
	SinZero   [8]byte
}

const (
	EventBodyMaxSize = 16384 // 16 KB
)


// SocketDataEvent is a conversion of the following C-Struct into GO.
type SocketDataEvent struct {
	TimestampNano        uint64
	ConnID               ConnID
	Direction            TrafficDirectionEnum
	MsgSize              uint32
	Pos                  uint64
	Msg                  [EventBodyMaxSize]byte
	ValidateReadBytes    int64
	ValidateWrittenBytes int64
}

// SocketOpenEvent is a conversion of the following C-Struct into GO.
type SocketOpenEvent struct {
	TimestampNano uint64
	ConnID        ConnID
	Addr          SockAddrIn
}

// SocketCloseEvent is a conversion of the following C-Struct into GO.
type SocketCloseEvent struct {
	TimestampNano uint64
	ConnID        ConnID
	WrittenBytes  int64
	ReadBytes     int64
}

type Bpf_spin_lock struct{ Val uint32 }


type DestInfo struct {
	IpVersion uint32
	DestIp4   uint32
	DestIp6   [4]uint32
	DestPort  uint32
	KernelPid uint32
}


type ProxyInfo struct {
	IP4  uint32
	Ip6  [4]uint32
	Port uint32
}
