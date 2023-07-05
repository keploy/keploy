package connection

import (
	"sync"
	"time"

	structs2 "go.keploy.io/server/pkg/hooks/structs"
	"go.uber.org/zap"
	// "log"
)

const (
	maxBufferSize = 16 * 1024 * 1024 // 16MB
)

type Tracker struct {
	connID structs2.ConnID

	addr              structs2.SockAddrIn
	openTimestamp     uint64
	closeTimestamp    uint64
	totalWrittenBytes uint64
	totalReadBytes    uint64

	// Indicates the tracker stopped tracking due to closing the session.
	lastActivityTimestamp uint64
	sentBytes             uint64
	recvBytes             uint64

	recvBuf []byte
	sentBuf []byte
	mutex   sync.RWMutex
	logger  *zap.Logger
}

func NewTracker(connID structs2.ConnID, logger *zap.Logger) *Tracker {
	return &Tracker{
		connID:  connID,
		recvBuf: make([]byte, 0, maxBufferSize),
		sentBuf: make([]byte, 0, maxBufferSize),
		mutex:   sync.RWMutex{},
		logger:  logger,
	}
}

func (conn *Tracker) ToBytes() ([]byte, []byte) {
	conn.mutex.RLock()
	defer conn.mutex.RUnlock()
	return conn.recvBuf, conn.sentBuf
}

func (conn *Tracker) IsInactive(duration time.Duration) bool {
	conn.mutex.RLock()
	defer conn.mutex.RUnlock()
	return uint64(time.Now().UnixNano())-conn.lastActivityTimestamp > uint64(duration.Nanoseconds())
}

func (conn *Tracker) IsComplete() bool {
	conn.mutex.RLock()
	defer conn.mutex.RUnlock()
	// log.Printf("IsComplete() called: Successfully reading the data...")
	return conn.closeTimestamp != 0 &&
		conn.totalReadBytes == conn.recvBytes &&
		conn.totalWrittenBytes == conn.sentBytes
}

func (conn *Tracker) Malformed() bool {
	conn.mutex.RLock()
	defer conn.mutex.RUnlock()
	// conn.logger.Debug(Emoji+"data loss of ingress request message", zap.Any("bytes read in ebpf", conn.totalReadBytes), zap.Any("bytes recieved in userspace", conn.recvBytes))
	// conn.logger.Debug(Emoji+"data loss of ingress response message", zap.Any("bytes written in ebpf", conn.totalWrittenBytes), zap.Any("bytes sent to user", conn.sentBytes))
	// conn.logger.Debug(Emoji, zap.Any("Request buffer", string(conn.recvBuf)))
	// conn.logger.Debug(Emoji, zap.Any("Response buffer", string(conn.sentBuf)))
	return conn.closeTimestamp != 0 &&
		conn.totalReadBytes != conn.recvBytes &&
		conn.totalWrittenBytes != conn.sentBytes
}

func (conn *Tracker) AddDataEvent(event structs2.SocketDataEvent) {
	conn.mutex.Lock()
	defer conn.mutex.Unlock()
	conn.updateTimestamps()

	conn.logger.Debug(Emoji+"Got a data event from eBPF", zap.Any("Direction", event.Direction), zap.Any("current event size", event.MsgSize))
	switch event.Direction {
	case structs2.EgressTraffic:
		conn.sentBuf = append(conn.sentBuf, event.Msg[:event.MsgSize]...)
		conn.sentBytes += uint64(event.MsgSize)
	case structs2.IngressTraffic:
		conn.recvBuf = append(conn.recvBuf, event.Msg[:event.MsgSize]...)
		conn.recvBytes += uint64(event.MsgSize)
	default:
	}
}

func (conn *Tracker) AddOpenEvent(event structs2.SocketOpenEvent) {
	conn.mutex.Lock()
	defer conn.mutex.Unlock()
	conn.updateTimestamps()
	conn.addr = event.Addr
	if conn.openTimestamp != 0 && conn.openTimestamp != event.TimestampNano {
		conn.logger.Debug(Emoji+"Changed open info timestamp due to new request", zap.Any("from", conn.openTimestamp), zap.Any("to", event.TimestampNano))
	}
	// conn.logger.Debug(Emoji+"Got an open event from eBPF", zap.Any("File Descriptor", event.ConnID.FD))
	conn.openTimestamp = event.TimestampNano
}

func (conn *Tracker) AddCloseEvent(event structs2.SocketCloseEvent) {
	conn.mutex.Lock()
	defer conn.mutex.Unlock()
	conn.updateTimestamps()
	if conn.closeTimestamp != 0 && conn.closeTimestamp != event.TimestampNano {
		conn.logger.Debug(Emoji+"Changed close info timestamp due to new request", zap.Any("from", conn.closeTimestamp), zap.Any("to", event.TimestampNano))
	}
	conn.closeTimestamp = event.TimestampNano

	conn.totalWrittenBytes = uint64(event.WrittenBytes)
	conn.totalReadBytes = uint64(event.ReadBytes)
	conn.logger.Debug(Emoji+"Got a close event from eBPF", zap.Any("TotalReadBytes", conn.totalReadBytes), zap.Any("TotalWrittenBytes", conn.totalWrittenBytes))
}

func (conn *Tracker) updateTimestamps() {
	conn.lastActivityTimestamp = uint64(time.Now().UnixNano())
}
