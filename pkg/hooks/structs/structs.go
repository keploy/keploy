package structs

// ConnID is a conversion of the following C-Struct into GO.
//
//	struct conn_id_t {
//	   uint32_t tgid;
//	   int32_t fd;
//	   uint64_t tsid;
//	};.
type ConnID struct {
	TGID uint32
	FD   int32
	TsID uint64
}

// SockAddrIn is a conversion of the following C-Struct into GO.
//
//	struct sockaddr_in {
//	   unsigned short int sin_family;
//	   uint16_t sin_port;
//	   struct in_addr sin_addr;
//
//	   /* _to size of `struct sockaddr'.  */
//	   unsigned char sin_zero[8];
//	};.
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
// struct socket_data_event_t
// {
//     u64 entry_timestamp_ns;
//     u64 timestamp_ns;
//     struct conn_id_t conn_id;
//     enum traffic_direction_t direction;
//     u32 msg_size;
//     u64 pos;
//     char msg[MAX_MSG_SIZE];
//     s64 validate_rd_bytes
//     s64 validate_wr_bytes
// };

// Socket Data Event .....
type SocketDataEvent struct {
	EntryTimestampNano   uint64
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
//
//	struct socket_open_event_t {
//	   uint64_t timestamp_ns;
//	   struct conn_id_t conn_id;
//	   struct sockaddr_in* addr;
//	};.
type SocketOpenEvent struct {
	TimestampNano uint64
	ConnID        ConnID
	Addr          SockAddrIn
}

// SocketCloseEvent is a conversion of the following C-Struct into GO.
//
//	struct socket_close_event_t {
//	   uint64_t timestamp_ns;
//	   struct conn_id_t conn_id;
//	   int64_t wr_bytes;
//	   int64_t rd_bytes;
//	};.
type SocketCloseEvent struct {
	TimestampNano uint64
	ConnID        ConnID
	WrittenBytes  int64
	ReadBytes     int64
}

type Bpf_spin_lock struct{ Val uint32 }

// struct dest_info_t
// {
//     u32 ip_version;
//     u32 dest_ip4;
//     u32 dest_ip6[4];
//     u32 dest_port;
//     u32 kernelPid;
// };

type DestInfo struct {
	IpVersion uint32
	DestIp4   uint32
	DestIp6   [4]uint32
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
	Ip6  [4]uint32
	Port uint32
}

type MasterSecretEvent struct {
	Version int32 `json:"version"` // TLS Version

	// TLS 1.3
	CipherId               uint32   `json:"cipherId"`               // Cipher ID
	HandshakeSecret        [64]byte `json:"handshakeSecret"`        // Handshake Secret
	HandshakeTrafficHash   [64]byte `json:"handshakeTrafficHash"`   // Handshake Traffic Hash
	ClientAppTrafficSecret [64]byte `json:"clientAppTrafficSecret"` // Client App Traffic Secret
	ServerAppTrafficSecret [64]byte `json:"serverAppTrafficSecret"` // Server App Traffic Secret
	ExporterMasterSecret   [64]byte `json:"exporterMasterSecret"`   // Exporter Master Secret
}
