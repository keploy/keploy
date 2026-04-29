// Package fakeconn provides read-only connection abstractions that decouple
// integration parsers from real network sockets. The proxy relay owns the
// real net.Conn and pushes timestamped Chunks into a FakeConn; parsers read
// from the FakeConn but cannot write to real peers.
package fakeconn

import "time"

// Direction identifies which real peer produced the bytes in a Chunk.
type Direction uint8

const (
	// FromClient indicates bytes read from the real client socket.
	FromClient Direction = iota
	// FromDest indicates bytes read from the real destination socket.
	FromDest
)

// String returns a short human-readable label for the direction.
// Returns "client" for FromClient, "dest" for FromDest, and "unknown"
// for any out-of-range value.
func (d Direction) String() string {
	switch d {
	case FromClient:
		return "client"
	case FromDest:
		return "dest"
	default:
		return "unknown"
	}
}

// Chunk is one unit of data delivered to a parser. Timestamps are captured
// at the real-socket boundary by the relay and carried through unchanged;
// parsers must use these for ReqTimestampMock / ResTimestampMock rather than
// calling time.Now() themselves.
type Chunk struct {
	Dir       Direction
	Bytes     []byte
	ReadAt    time.Time // when the relay's Read() returned on the source socket
	WrittenAt time.Time // when the relay's Write() returned on the opposite socket
	SeqNo     uint64    // monotonic, scoped to (connection, direction)
}

// IsZero reports whether c is the zero Chunk value. Useful for
// channel consumers that receive a Chunk after a close-without-value.
//
// We intentionally exclude Dir from the predicate: Direction's zero
// value happens to be FromClient (`iota` starts at 0), so a genuine
// empty client-side chunk (e.g. a probe with zero bytes and no
// timestamps — unusual but possible during teardown) would otherwise
// be misclassified as zero. A chunk with any of bytes, ReadAt,
// WrittenAt, or SeqNo set is non-zero regardless of Dir.
func (c Chunk) IsZero() bool {
	return c.Bytes == nil && c.ReadAt.IsZero() && c.WrittenAt.IsZero() && c.SeqNo == 0
}
