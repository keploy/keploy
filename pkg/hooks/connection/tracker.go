package connection

import (
	"sync"
	"time"

	structs2 "go.keploy.io/server/pkg/hooks/structs"
	"go.uber.org/zap"
)

const (
	// maxBufferSize = 100 * 1024 // 100KB
	maxBufferSize = 15 * 1024 * 1024 // 15MB
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
	logger *zap.Logger
}

func NewTracker(connID structs2.ConnID, logger *zap.Logger) *Tracker {
	return &Tracker{
		connID:  connID,
		recvBuf: make([]byte, 0, maxBufferSize),
		sentBuf: make([]byte, 0, maxBufferSize),
		mutex:   sync.RWMutex{},
		logger: logger,
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
	// log.Printf("Malformed() called: request completed but is Malformed")
	conn.logger.Debug("data loss of ingress request message", zap.Any("bytes read in ebpf", conn.totalReadBytes), zap.Any("bytes recieved in userspace", conn.recvBytes))
	conn.logger.Debug("data loss of ingress response message", zap.Any("bytes written in ebpf", conn.totalWrittenBytes), zap.Any("bytes sent to user", conn.sentBytes))
	// log.Printf("Total Written bytes:%v but sent only:%v", conn.totalWrittenBytes, conn.sentBytes)
	// log.Printf("Req:%v", string(conn.recvBuf))
	// log.Printf("Res:%v", string(conn.sentBuf))
	return conn.closeTimestamp != 0 &&
		conn.totalReadBytes != conn.recvBytes &&
		conn.totalWrittenBytes != conn.sentBytes
}

func (conn *Tracker) AddDataEvent(event structs2.SocketDataEvent) {
	conn.mutex.Lock()
	defer conn.mutex.Unlock()
	conn.updateTimestamps()

	switch event.Direction {
	case structs2.EgressTraffic:
		conn.sentBuf = append(conn.sentBuf, event.Msg[:event.MsgSize]...)
		conn.sentBytes += uint64(event.MsgSize)
	case structs2.IngressTraffic:
		conn.recvBuf = append(conn.recvBuf, event.Msg[:event.MsgSize]...)
		conn.recvBytes += uint64(event.MsgSize)
		// log.Println("Actual size of read payload becomes [%v]:", len(conn.recvBuf))
		// log.Println("Apparent size of read payload after [%v] becomes:", uint64(event.MsgSize), conn.recvBytes)
	default:
	}
}

func (conn *Tracker) AddOpenEvent(event structs2.SocketOpenEvent) {
	conn.mutex.Lock()
	defer conn.mutex.Unlock()
	conn.updateTimestamps()
	conn.addr = event.Addr
	if conn.openTimestamp != 0 && conn.openTimestamp != event.TimestampNano {
		conn.logger.Debug("Changed open info timestamp due to new request", zap.Any("from", conn.openTimestamp), zap.Any("to", event.TimestampNano))
	}
	conn.openTimestamp = event.TimestampNano
}

func (conn *Tracker) AddCloseEvent(event structs2.SocketCloseEvent) {
	conn.mutex.Lock()
	defer conn.mutex.Unlock()
	conn.updateTimestamps()
	if conn.closeTimestamp != 0 && conn.closeTimestamp != event.TimestampNano {
		conn.logger.Debug("Changed close info timestamp due to new request", zap.Any("from", conn.closeTimestamp), zap.Any("to", event.TimestampNano))
	}
	conn.closeTimestamp = event.TimestampNano

	conn.totalWrittenBytes = uint64(event.WrittenBytes)
	conn.totalReadBytes = uint64(event.ReadBytes)
}

func (conn *Tracker) updateTimestamps() {
	conn.lastActivityTimestamp = uint64(time.Now().UnixNano())
}
