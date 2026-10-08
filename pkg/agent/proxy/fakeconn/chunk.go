// Package fakeconn provides read-only connection abstractions that decouple
// integration parsers from real network sockets. The proxy relay owns the
// real net.Conn and pushes timestamped Chunks into a FakeConn; parsers read
// from the FakeConn but cannot write to real peers.
package fakeconn

import (
	"math"
	"time"
)

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
// calling time.Now() themselves. They are for a mock's window only: what was
// captured before what, across the two directions of a connection, is
// ConnSeq's to say (CapturedBefore), never theirs.
type Chunk struct {
	Dir Direction
	// ConnSeq is the connection-wide capture sequence: the producer numbers
	// the chunks of BOTH directions of a connection from one counter, once,
	// where the bytes enter the capture, with ConnSeqOf, by one rule: a
	// chunk from the client is numbered after every byte the server had sent
	// the producer when the chunk was read, read yet or not, and before every
	// byte it sent after (the relay: as Read returns on the source socket,
	// before it writes the bytes on; connseq.Upstream). A chunk with a lower
	// ConnSeq was captured first, whichever direction it came from; the
	// server's chunks that reached the producer between the same two client
	// chunks may share one. It is the one thing that orders a connection's
	// two directions (CapturedBefore).
	//
	// Every producer numbers every chunk: 0 is no number, and a reader that
	// orders the directions refuses a chunk without one (ErrUnnumbered). It
	// is 32 bits, compared as a serial number, so it sits in the padding
	// after Dir and a queued chunk costs no more memory than it did.
	ConnSeq   uint32
	Bytes     []byte
	ReadAt    time.Time // when the relay's Read() returned on the source socket
	WrittenAt time.Time // when the relay's Write() returned on the opposite socket
	SeqNo     uint64    // monotonic, scoped to (connection, direction)
}

// ConnSeqOf is the ConnSeq of the n-th chunk (n from 1) a producer captures on
// a connection, counted across both of its directions: n itself, wrapped to 32
// bits past 0, which is no number. A producer keeps n in a 64-bit counter, so
// the counter itself never wraps, and every producer numbers its chunks with
// this, so they all wrap the same way.
func ConnSeqOf(n uint64) uint32 { return uint32((n-1)%math.MaxUint32) + 1 }

// CapturedBefore reports whether c was captured before the chunk numbered seq
// (ConnSeq). It is the one comparison of two capture sequences: every rule
// that orders a connection's two directions goes through it. Server bytes
// captured before a request's first chunk are not its answer.
//
// The numbers are compared as serial numbers (RFC 1982), so they may wrap
// (ConnSeqOf): the answer is right while the two chunks are fewer than 2^31
// chunks apart, and the chunks a reader compares are never further apart than
// what one connection has queued.
func (c Chunk) CapturedBefore(seq uint32) bool { return int32(c.ConnSeq-seq) < 0 }

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
