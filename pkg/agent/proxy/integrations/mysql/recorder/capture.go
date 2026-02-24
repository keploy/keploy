// Package recorder implements the MySQL recorder for Keploy.
//
// Architecture: "TeeForward & Defer"
//
// The recording pipeline is split into three stages that run concurrently:
//
//  1. TeeForwardConn — two instances (one per direction) that Read from one
//     peer, immediately Write to the other at wire speed, and buffer the data
//     in a pre-allocated 2 MB ring buffer for the reassembler.  The forwarding
//     goroutine does ZERO heap allocations per Read — it writes into the ring
//     buffer via memcpy into pre-allocated memory.  This gives identical
//     latency to io.Copy / globalPassThrough (~12-13ms P50).
//
//  2. Reassembler — one goroutine per connection that reads from the two
//     TeeForwardConns (via their Read() method backed by the ring buffer),
//     frames complete MySQL packets, and tracks the half-duplex
//     command/response protocol to group packets into request-response
//     pairs.  Output is a stream of RawMockEntry.
//
//  3. Decoder — one goroutine per connection that receives RawMockEntry
//     values, fully decodes them using the existing wire/* decoders, builds
//     models.Mock objects, and sends them to the final mocks channel.
//     This is the only stage that allocates rich Go structs.
//
// Because stages 2 and 3 read from the ring buffer AFTER data has already
// been forwarded, and the ring buffer uses pre-allocated memory with zero
// heap allocations, the forwarding latency is identical to bare io.Copy.
package recorder

import (
	"time"

	"go.keploy.io/server/v3/pkg/models/mysql"
)

// RawMockEntry represents one MySQL request/response exchange as raw packet
// bytes.  No decoding has been applied — the reassembler produces these and
// the decoder (ProcessRawMocksV2) consumes them.
type RawMockEntry struct {
	// ReqPackets are the complete MySQL wire packets sent by the client
	// (including the 4-byte header).  Typically one packet per command.
	ReqPackets [][]byte

	// RespPackets are the complete MySQL wire packets sent by the server.
	// For OK/ERR responses this is a single packet; for result sets it
	// includes column-defs, EOF markers, rows, and the final EOF/OK.
	RespPackets [][]byte

	// CmdType is the first payload byte of the first request packet
	// (e.g. COM_QUERY = 0x03).  The reassembler sets this to enable
	// protocol-aware response framing without full decoding.
	CmdType byte

	// MockType is "config" for the handshake exchange, "mocks" for everything else.
	MockType string

	ReqTimestamp time.Time
	ResTimestamp time.Time
}

// handshakeState holds the minimal information extracted from the MySQL
// handshake that the reassembler needs for protocol-aware response framing.
// It is produced by handleHandshake (synchronous phase) and consumed by
// RunReassembler (async phase).
type handshakeState struct {
	ServerCaps     uint32
	ClientCaps     uint32
	PluginName     string
	UseSSL         bool
	DeprecateEOF   bool
	ServerGreeting *mysql.HandshakeV10Packet // needed by fast-path decoder
}
