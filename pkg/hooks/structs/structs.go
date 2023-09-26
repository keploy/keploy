package structs

type ConnID struct {
	TGID uint32
	FD   int32
	TsID uint64
}

type SockAddrIn struct {
	SinFamily uint16
	SinPort   uint16
	SinAddr   uint32
	SinZero   [8]byte
}

const (
	EventBodyMaxSize = 16384 // 16 KB
)


// Socket Data Event .....
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

type SocketOpenEvent struct {
	TimestampNano uint64
	ConnID        ConnID
	Addr          SockAddrIn
}

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
