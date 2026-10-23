// Package fakeconn provides read-only connection abstractions that decouple
// integration parsers from real network sockets. The proxy relay owns the
// real net.Conn and pushes timestamped Chunks into a FakeConn; parsers read
// from the FakeConn but cannot write to real peers.
package fakeconn

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"math"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
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
	// ReadClock is the clock ReadAt is on: set only by a
	// producer that took the stamp itself, ClockUnset on every other chunk,
	// which is never trusted. It sits in the padding after Dir, so a chunk
	// costs no more memory for it (TestChunkSize).
	ReadClock Clock
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

// Clock is the clock a chunk's ReadAt is on. Only a producer
// that took the stamp itself sets one; every other chunk is ClockUnset, which
// is never trusted.
type Clock uint8

const (
	ClockUnset Clock = iota
	// ClockProxy: the relay's time.Now as Read returned on the source socket.
	ClockProxy
	// ClockKtime: CLOCK_MONOTONIC stamped by the producer at capture (a BPF
	// event, JSSE's System.nanoTime), converted by the run's shared anchor.
	ClockKtime

	// clockCount is one past the last clock: a clock below it may be named.
	clockCount
)

// String is c's short name: "unset", "proxy", "ktime", or the number of a
// clock this keploy does not know.
func (c Clock) String() string {
	switch c {
	case ClockUnset:
		return "unset"
	case ClockProxy:
		return "proxy"
	case ClockKtime:
		return "ktime"
	default:
		return "clock(" + strconv.Itoa(int(c)) + ")"
	}
}

// clockIDs are this run's clock names (SetClockID), read on every stamp a
// record names (ClockID): a slot per clock, written once.
var (
	clockIDsMu sync.Mutex // serialises SetClockID
	clockIDs   [clockCount]atomic.Pointer[string]
)

// SetClockID names c for this agent run: the relay sets ClockProxy's
// ("proxy:<run>") at start, the proxyless anchor ClockKtime's
// ("ktime:<boot id>.<anchor>") when it is taken. An id never changes within a
// run: a second, different one panics. Setting the one it has again does
// nothing. ClockUnset is never named, and no id is empty: either panics, as
// does a clock this keploy does not know. The run is the process's: the names
// are process-wide and never cleared, so a producer picks its id once per
// process (a relay that starts again in the same agent sets the same id).
//
// Equal non-empty ids are one clock (models.Origin.Clock): two stamps whose
// clocks' ids are equal may be ordered against each other, and no others.
func SetClockID(c Clock, id string) {
	if c == ClockUnset || c >= clockCount {
		panic("fakeconn.SetClockID: " + c.String() + " cannot be named")
	}
	if id == "" {
		panic("fakeconn.SetClockID(" + c.String() + "): an empty id")
	}
	clockIDsMu.Lock()
	defer clockIDsMu.Unlock()
	if cur := clockIDs[c].Load(); cur != nil {
		if *cur != id {
			panic("fakeconn.SetClockID(" + c.String() + "): named " + strconv.Quote(*cur) + " already, not " + strconv.Quote(id))
		}
		return
	}
	clockIDs[c].Store(&id)
}

// NameProxyClock names ClockProxy for this agent run, once per process, and
// returns its id: "proxy:<run>", with run a random id the process draws the
// first time it is called. Every producer that stamps on the proxy's clock
// (the relay, as it reads a chunk; the ingress, as it opens a test's window)
// calls it before its first stamp, so a stamp on that clock is named.
func NameProxyClock() string {
	id := proxyRunID()
	if ClockID(ClockProxy) != id {
		SetClockID(ClockProxy, id)
	}
	return id
}

// proxyRunID is this process's proxy clock id, drawn once.
var proxyRunID = sync.OnceValue(func() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// No randomness: the process's id and start still name this run
		// apart from another agent's.
		binary.BigEndian.PutUint32(b[:4], uint32(os.Getpid()))
		binary.BigEndian.PutUint32(b[4:], uint32(time.Now().UnixNano()))
	}
	return "proxy:" + hex.EncodeToString(b[:])
})

// ClockID is c's name for this run: "" for ClockUnset or a clock not named.
func ClockID(c Clock) string {
	if c == ClockUnset || c >= clockCount {
		return ""
	}
	if id := clockIDs[c].Load(); id != nil {
		return *id
	}
	return ""
}

// The stamped-request line, the contract between a parser
// that fixes a request's stamp and a lane that waits on it. keploy/integrations'
// Mongo v2 recorder logs it, at debug, once it has fixed a complete client
// message's start (clientMessageStartReadAt), for every op, handshakes
// included:
//
//	logger.Debug("mongo v2 request stamped",
//		zap.String("op", op),            // "insert", "update", "aggregate", "hello", ...
//		zap.String("ns", db+"."+coll),   // "" for a command with no collection
//		zap.String("conn", sess.ClientConnID),
//		zap.String("clock", fakeconn.ClockID(startChunk.ReadClock)))
//
// The message text and the field names are the contract; no value of the
// request is logged. A lane (keploy/enterprise's sqs-s12-chain.sh) records
// with --debug and greps "mongo v2 request stamped" with "op": "<op>" and
// "ns": "<ns>", and treats a conn it had not seen before its send as a new
// connection.

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
