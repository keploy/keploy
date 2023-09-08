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

	RecvBuf []byte
	SentBuf []byte
	mutex   sync.RWMutex
	logger  *zap.Logger
}

func NewTracker(connID structs2.ConnID, logger *zap.Logger) *Tracker {
	return &Tracker{
		connID:  connID,
		RecvBuf: make([]byte, 0, maxBufferSize),
		SentBuf: make([]byte, 0, maxBufferSize),
		mutex:   sync.RWMutex{},
		logger:  logger,
	}
}

func (conn *Tracker) ToBytes() ([]byte, []byte) {
	conn.mutex.RLock()
	defer conn.mutex.RUnlock()
	return conn.RecvBuf, conn.SentBuf
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

	// Get the current timestamp in nanoseconds.
	currentTimestamp := uint64(time.Now().UnixNano())

	// Calculate the time elapsed since the last activity in nanoseconds.
	elapsedTime := currentTimestamp - conn.lastActivityTimestamp

	//Caveat: Added a timeout of 3 seconds, aft          er this duration we assume that all the data events would have come.
	// This will ensure that we capture the requests responses where Connection:keep-alive is enabled.

	// Check if 1 second has passed since the last activity.
	if elapsedTime >= uint64(time.Second*2) {
		// conn.logger.Debug("Either connection is alive or request is a mutlipart/file-upload")
		// return true
	}

	// Check if other conditions for completeness are met.
	return conn.closeTimestamp != 0 &&
		conn.totalReadBytes == conn.recvBytes &&
		conn.totalWrittenBytes == conn.sentBytes
}

func (conn *Tracker) Malformed() bool {
	conn.mutex.RLock()
	defer conn.mutex.RUnlock()
	// conn.logger.Debug("data loss of ingress request message", zap.Any("bytes read in ebpf", conn.totalReadBytes), zap.Any("bytes recieved in userspace", conn.recvBytes))
	// conn.logger.Debug("data loss of ingress response message", zap.Any("bytes written in ebpf", conn.totalWrittenBytes), zap.Any("bytes sent to user", conn.sentBytes))
	// conn.logger.Debug("", zap.Any("Request buffer", string(conn.RecvBuf)))
	// conn.logger.Debug("", zap.Any("Response buffer", string(conn.SentBuf)))
	return conn.closeTimestamp != 0 &&
		conn.totalReadBytes != conn.recvBytes &&
		conn.totalWrittenBytes != conn.sentBytes
}

func (conn *Tracker) AddDataEvent(event structs2.SocketDataEvent) {
	conn.mutex.Lock()
	defer conn.mutex.Unlock()
	conn.UpdateTimestamps()

	conn.logger.Debug("Got a data event from eBPF", zap.Any("Direction", event.Direction), zap.Any("current event size", event.MsgSize))
	switch event.Direction {
	case structs2.EgressTraffic:
		conn.SentBuf = append(conn.SentBuf, event.Msg[:event.MsgSize]...)
		conn.sentBytes += uint64(event.MsgSize)
	case structs2.IngressTraffic:
		conn.RecvBuf = append(conn.RecvBuf, event.Msg[:event.MsgSize]...)
		conn.recvBytes += uint64(event.MsgSize)
	default:
	}
}

func (conn *Tracker) AddOpenEvent(event structs2.SocketOpenEvent) {
	conn.mutex.Lock()
	defer conn.mutex.Unlock()
	conn.UpdateTimestamps()
	conn.addr = event.Addr
	if conn.openTimestamp != 0 && conn.openTimestamp != event.TimestampNano {
		conn.logger.Debug("Changed open info timestamp due to new request", zap.Any("from", conn.openTimestamp), zap.Any("to", event.TimestampNano))
	}
	// conn.logger.Debug("Got an open event from eBPF", zap.Any("File Descriptor", event.ConnID.FD))
	conn.openTimestamp = event.TimestampNano
}

func (conn *Tracker) AddCloseEvent(event structs2.SocketCloseEvent) {
	conn.mutex.Lock()
	defer conn.mutex.Unlock()
	conn.UpdateTimestamps()
	if conn.closeTimestamp != 0 && conn.closeTimestamp != event.TimestampNano {
		conn.logger.Debug("Changed close info timestamp due to new request", zap.Any("from", conn.closeTimestamp), zap.Any("to", event.TimestampNano))
	}
	conn.closeTimestamp = event.TimestampNano

	conn.totalWrittenBytes = uint64(event.WrittenBytes)
	conn.totalReadBytes = uint64(event.ReadBytes)
	conn.logger.Debug("Got a close event from eBPF", zap.Any("TotalReadBytes", conn.totalReadBytes), zap.Any("TotalWrittenBytes", conn.totalWrittenBytes))
}

func (conn *Tracker) UpdateTimestamps() {
	conn.lastActivityTimestamp = uint64(time.Now().UnixNano())
}
