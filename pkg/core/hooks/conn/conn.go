// Package conn provides functionality for handling connections.
package conn

// constant for the maximum size of the event body
const (
	EventBodyMaxSize = 16384 // 16 KB
)

// ID is a conversion of the following C-Struct into GO.
//
//	struct conn_id_t {
//	   uint32_t tgid;
//	   int32_t fd;
//	   uint64_t tsid;
//	};
type ID struct {
	TsID     uint64
	FD       int32
	TGID     uint32
	ClientID uint64
}

// SocketDataEvent is a conversion of the following C-Struct into GO.
// struct socket_data_event_t
//
//	{
//	    u64 entry_timestamp_ns;
//	    u64 timestamp_ns;
//	    struct conn_id_t conn_id;
//	    enum traffic_direction_t direction;
//	    u32 msg_size;
//	    u64 pos;
//	    char msg[MAX_MSG_SIZE];
//	    s64 validate_rd_bytes
//	    s64 validate_wr_bytes
//	};
type SocketDataEvent struct {
	EntryTimestampNano   uint64
	TimestampNano        uint64
	ConnID               ID
	Direction            TrafficDirectionEnum
	MsgSize              uint32
	Pos                  uint64
	Msg                  [EventBodyMaxSize]byte
	ValidateReadBytes    int64
	ValidateWrittenBytes int64
	ClientID             uint64
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
	ConnID        ID
	Addr          SockAddrIn
	ClientID      uint64
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
	ConnID        ID
	WrittenBytes  int64
	ReadBytes     int64
	ClientID      uint64
}

// TrafficDirectionEnum is a GO-equivalent for the following enum.
//
//	enum traffic_direction_t {
//		kEgress,
//		kIngress,
//	};.
type TrafficDirectionEnum int32

// constants for the TrafficDirectionEnum
const (
	EgressTraffic  TrafficDirectionEnum = 0
	IngressTraffic TrafficDirectionEnum = 1
)

func (t TrafficDirectionEnum) String() string {
	names := [...]string{
		"EgressTraffic",
		"IngressTraffic",
	}

	switch t {
	case EgressTraffic:
		return names[0]
	case IngressTraffic:
		return names[1]
	default:
		return "Invalid TrafficDirectionEnum value"
	}
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
