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

// SocketDataEventAttr is a conversion of the following C-Struct into GO.
//
//	struct attr_t {
//	    uint64_t timestamp_ns;
//	    struct conn_id_t conn_id;
//	    enum traffic_direction_t direction;
//	    uint32_t msg_size;
//	    uint64_t pos;
//	};.
// type SocketDataEventAttr struct {
// 	TimestampNano uint64
// 	ConnID        ConnID
// 	Direction     TrafficDirectionEnum
// 	MsgSize       uint32
// 	Pos           uint64
// }

const (
	EventBodyMaxSize = 16384 // 16 KB
)

// SocketDataEvent is a conversion of the following C-Struct into GO.
//
//	struct socket_data_event_t {
//	   struct attr_t attr;
//	   char msg[16384];
//	};.
// type SocketDataEvent struct {
// 	Attr SocketDataEventAttr
// 	Msg  [EventBodyMaxSize] byte
// }

// Socket Data Event .....
type SocketDataEvent struct {
	TimestampNano uint64
	ConnID        ConnID
	Direction     TrafficDirectionEnum
	MsgSize       uint32
	Pos           uint64
	Msg           [EventBodyMaxSize]byte
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
//     u32 dest_ip;
//     u32 dest_port;
//     struct bpf_spin_lock lock;
// };

type DestInfo struct {
	DestIp   uint32
	DestPort uint32
	// Lock Bpf_spin_lock
}


// struct proxy_info
// {
//     u32 ip;
//     u32 port;
// };

type ProxyInfo struct {
	IP   uint32
	Port uint32
}
