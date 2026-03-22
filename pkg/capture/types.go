package capture

import "time"

// File format constants
const (
	// MagicBytes identifies a .kpcap file.
	MagicBytes = "KPCAP\x00\x01\x00"

	// CurrentVersion is the file format version.
	CurrentVersion uint16 = 1

	// MaxPayloadSize limits individual packet payloads to 16MB.
	MaxPayloadSize = 16 * 1024 * 1024

	// MaxPacketsInFile limits the total number of packets per file to prevent runaway captures.
	MaxPacketsInFile = 10_000_000
)

// Direction indicates the direction of data flow.
type Direction uint8

const (
	// DirClientToProxy is data from the application to the proxy.
	DirClientToProxy Direction = 0
	// DirProxyToDest is data from the proxy to the external service.
	DirProxyToDest Direction = 1
	// DirDestToProxy is data from the external service to the proxy.
	DirDestToProxy Direction = 2
	// DirProxyToClient is data from the proxy back to the application.
	DirProxyToClient Direction = 3
)

// String returns a human-readable direction name.
func (d Direction) String() string {
	switch d {
	case DirClientToProxy:
		return "client→proxy"
	case DirProxyToDest:
		return "proxy→dest"
	case DirDestToProxy:
		return "dest→proxy"
	case DirProxyToClient:
		return "proxy→client"
	default:
		return "unknown"
	}
}

// PacketType indicates the type of captured event.
type PacketType uint8

const (
	// PacketTypeData contains actual payload data.
	PacketTypeData PacketType = 0
	// PacketTypeConnOpen records a new connection being established.
	PacketTypeConnOpen PacketType = 1
	// PacketTypeConnClose records a connection being closed.
	PacketTypeConnClose PacketType = 2
	// PacketTypeProtocol records protocol detection for a connection.
	PacketTypeProtocol PacketType = 3
	// PacketTypeError records an error during connection handling.
	PacketTypeError PacketType = 4
	// PacketTypeDNS records a DNS query/response.
	PacketTypeDNS PacketType = 5
)

// String returns a human-readable packet type name.
func (pt PacketType) String() string {
	switch pt {
	case PacketTypeData:
		return "DATA"
	case PacketTypeConnOpen:
		return "CONN_OPEN"
	case PacketTypeConnClose:
		return "CONN_CLOSE"
	case PacketTypeProtocol:
		return "PROTOCOL"
	case PacketTypeError:
		return "ERROR"
	case PacketTypeDNS:
		return "DNS"
	default:
		return "UNKNOWN"
	}
}

// Protocol identifies the detected application-layer protocol.
type Protocol uint8

const (
	ProtoUnknown  Protocol = 0
	ProtoHTTP     Protocol = 1
	ProtoHTTP2    Protocol = 2
	ProtoGRPC     Protocol = 3
	ProtoMySQL    Protocol = 4
	ProtoPostgres Protocol = 5
	ProtoMongo    Protocol = 6
	ProtoRedis    Protocol = 7
	ProtoKafka    Protocol = 8
	ProtoGeneric  Protocol = 9
	ProtoDNS      Protocol = 10
)

// String returns a human-readable protocol name.
func (p Protocol) String() string {
	switch p {
	case ProtoHTTP:
		return "HTTP"
	case ProtoHTTP2:
		return "HTTP2"
	case ProtoGRPC:
		return "gRPC"
	case ProtoMySQL:
		return "MySQL"
	case ProtoPostgres:
		return "Postgres"
	case ProtoMongo:
		return "MongoDB"
	case ProtoRedis:
		return "Redis"
	case ProtoKafka:
		return "Kafka"
	case ProtoGeneric:
		return "Generic"
	case ProtoDNS:
		return "DNS"
	default:
		return "Unknown"
	}
}

// FileHeader is written at the beginning of every .kpcap file.
type FileHeader struct {
	Magic     [8]byte // "KPCAP\x00\x01\x00"
	Version   uint16
	Mode      uint8 // 0=record, 1=test
	CreatedAt int64 // unix nanoseconds
	// MetadataLen precedes a JSON blob with additional context
	MetadataLen uint32
}

// FileMetadata is the JSON blob stored after the FileHeader.
type FileMetadata struct {
	KeployVersion string            `json:"keploy_version"`
	GoVersion     string            `json:"go_version,omitempty"`
	OS            string            `json:"os"`
	Arch          string            `json:"arch"`
	Hostname      string            `json:"hostname,omitempty"`
	Mode          string            `json:"mode"`
	ProxyPort     uint32            `json:"proxy_port,omitempty"`
	DNSPort       uint32            `json:"dns_port,omitempty"`
	AppCommand    string            `json:"app_command,omitempty"`
	CreatedAt     time.Time         `json:"created_at"`
	Labels        map[string]string `json:"labels,omitempty"`
}

// Packet represents a single captured event in the network stream.
type Packet struct {
	Timestamp    time.Time
	ConnectionID uint64
	Type         PacketType
	Direction    Direction
	Protocol     Protocol
	IsTLS        bool
	SrcAddr      string
	DstAddr      string
	Payload      []byte
}

// CaptureFile represents a complete parsed capture file for replay.
type CaptureFile struct {
	Header   FileHeader
	Metadata FileMetadata
	Packets  []*Packet
}

// ConnectionTimeline groups packets by connection for analysis.
type ConnectionTimeline struct {
	ConnectionID uint64
	SrcAddr      string
	DstAddr      string
	IsTLS        bool
	Protocol     Protocol
	OpenedAt     time.Time
	ClosedAt     time.Time
	Packets      []*Packet
	Errors       []string
}

// GetConnections extracts connection timelines from a capture file.
func (cf *CaptureFile) GetConnections() map[uint64]*ConnectionTimeline {
	conns := make(map[uint64]*ConnectionTimeline)

	for _, pkt := range cf.Packets {
		ct, ok := conns[pkt.ConnectionID]
		if !ok {
			ct = &ConnectionTimeline{
				ConnectionID: pkt.ConnectionID,
			}
			conns[pkt.ConnectionID] = ct
		}

		switch pkt.Type {
		case PacketTypeConnOpen:
			ct.SrcAddr = pkt.SrcAddr
			ct.DstAddr = pkt.DstAddr
			ct.IsTLS = pkt.IsTLS
			ct.OpenedAt = pkt.Timestamp
		case PacketTypeConnClose:
			ct.ClosedAt = pkt.Timestamp
		case PacketTypeProtocol:
			ct.Protocol = pkt.Protocol
		case PacketTypeError:
			ct.Errors = append(ct.Errors, string(pkt.Payload))
		case PacketTypeData:
			ct.Packets = append(ct.Packets, pkt)
		}
	}

	return conns
}
