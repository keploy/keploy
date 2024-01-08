package connection

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	structs2 "go.keploy.io/server/pkg/hooks/structs"
	"go.uber.org/zap"
	// "log"
)

const (
	maxBufferSize = 16 * 1024 * 1024 // 16MB
)

type Tracker struct {
	connID         structs2.ConnID
	addr           structs2.SockAddrIn
	openTimestamp  uint64
	closeTimestamp uint64

	// Indicates the tracker stopped tracking due to closing the session.
	lastActivityTimestamp uint64

	// Queues to handle multiple ingress traffic on the same connection (keep-alive)
	totalSentBytesQ   []uint64
	totalRecvBytesQ   []uint64
	currentSentBytesQ []uint64
	currentRecvBytesQ []uint64
	currentSentBufQ   [][]byte
	currentRecvBufQ   [][]byte

	// Individual parameters to store current request and response data
	sentBytes uint64
	recvBytes uint64
	SentBuf   []byte
	RecvBuf   []byte

	// Additional fields to know when to capture request or response info
	receivedResponse bool
	receivedRequest  bool
	recTestCounter   int32 //atomic counter
	firstRequest     bool

	mutex  sync.RWMutex
	logger *zap.Logger

	reqTimestampTest []time.Time
	resTimestampTest time.Time
	isNewRequest     bool
}

func NewTracker(connID structs2.ConnID, logger *zap.Logger) *Tracker {
	return &Tracker{
		connID:            connID,
		RecvBuf:           []byte{},
		SentBuf:           []byte{},
		totalSentBytesQ:   []uint64{},
		totalRecvBytesQ:   []uint64{},
		currentSentBytesQ: []uint64{},
		currentRecvBytesQ: []uint64{},
		currentSentBufQ:   [][]byte{},
		currentRecvBufQ:   [][]byte{},
		mutex:             sync.RWMutex{},
		logger:            logger,
		firstRequest:      true,
		isNewRequest:      true,
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

func (conn *Tracker) incRecordTestCount() {
	atomic.AddInt32(&conn.recTestCounter, 1)
}

func (conn *Tracker) decRecordTestCount() {
	atomic.AddInt32(&conn.recTestCounter, -1)
}

// IsComplete() checks if the current connection has valid request & response info to capture
// and also returns the request and response data buffer.
func (conn *Tracker) IsComplete() (bool, []byte, []byte, time.Time, time.Time) {
	conn.mutex.Lock()
	defer conn.mutex.Unlock()

	// Get the current timestamp in nanoseconds.
	currentTimestamp := uint64(time.Now().UnixNano())

	// Calculate the time elapsed since the last activity in nanoseconds.
	elapsedTime := currentTimestamp - conn.lastActivityTimestamp

	//Caveat: Added a timeout of 4 seconds, after this duration we assume that the last response data event would have come.
	// This will ensure that we capture the requests responses where Connection:keep-alive is enabled.

	recordTraffic := false

	requestBuf, responseBuf := []byte{}, []byte{}

	//if recTestCounter > 0, it means that we have num(recTestCounter) of request and response present in the queues to record.
	if conn.recTestCounter > 0 {
		if (len(conn.currentRecvBytesQ) > 0 && len(conn.totalRecvBytesQ) > 0) &&
			(len(conn.currentSentBytesQ) > 0 && len(conn.totalSentBytesQ) > 0) {
			validReq, validRes := false, false

			expectedRecvBytes := conn.currentRecvBytesQ[0]
			actualRecvBytes := conn.totalRecvBytesQ[0]

			//popping out the current request info
			conn.currentRecvBytesQ = conn.currentRecvBytesQ[1:]
			conn.totalRecvBytesQ = conn.totalRecvBytesQ[1:]

			if conn.verifyRequestData(expectedRecvBytes, actualRecvBytes) {
				validReq = true
			} else {
				conn.logger.Debug("Malformed request", zap.Any("ExpectedRecvBytes", expectedRecvBytes), zap.Any("ActualRecvBytes", actualRecvBytes))
				recordTraffic = false
			}

			expectedSentBytes := conn.currentSentBytesQ[0]
			actualSentBytes := conn.totalSentBytesQ[0]

			//popping out the current response info
			conn.currentSentBytesQ = conn.currentSentBytesQ[1:]
			conn.totalSentBytesQ = conn.totalSentBytesQ[1:]

			if conn.verifyResponseData(expectedSentBytes, actualSentBytes) {
				validRes = true
				// Capturing the response timestamp as response is verified
				conn.resTimestampTest = time.Now()
			} else {
				conn.logger.Debug("Malformed response", zap.Any("ExpectedSentBytes", expectedSentBytes), zap.Any("ActualSentBytes", actualSentBytes))
				recordTraffic = false
			}

			if len(conn.currentRecvBufQ) > 0 && len(conn.currentSentBufQ) > 0 { //validated request, response
				requestBuf = conn.currentRecvBufQ[0]
				responseBuf = conn.currentSentBufQ[0]

				//popping out the current request & response data
				conn.currentRecvBufQ = conn.currentRecvBufQ[1:]
				conn.currentSentBufQ = conn.currentSentBufQ[1:]
			} else {
				conn.logger.Debug("no data buffer for request or response", zap.Any("Length of RecvBufQueue", len(conn.currentRecvBufQ)), zap.Any("Length of SentBufQueue", len(conn.currentSentBufQ)))
				recordTraffic = false
			}

			recordTraffic = validReq && validRes
		} else {
			conn.logger.Error("malformed request or response")
			recordTraffic = false
		}

		conn.logger.Debug(fmt.Sprintf("recording traffic after verifying the request and reponse data:%v", recordTraffic))

		// // decrease the recTestCounter
		conn.decRecordTestCount()
		conn.logger.Debug("verified recording", zap.Any("recordTraffic", recordTraffic))
	} else if conn.receivedResponse && elapsedTime >= uint64(time.Second*2) { // Check if 2 seconds has passed since the last activity.
		conn.logger.Debug("might be last request on the connection")

		if len(conn.currentRecvBytesQ) > 0 && len(conn.totalRecvBytesQ) > 0 {

			expectedRecvBytes := conn.currentRecvBytesQ[0]
			actualRecvBytes := conn.totalRecvBytesQ[0]

			//popping out the current request info
			conn.currentRecvBytesQ = conn.currentRecvBytesQ[1:]
			conn.totalRecvBytesQ = conn.totalRecvBytesQ[1:]

			if conn.verifyRequestData(expectedRecvBytes, actualRecvBytes) {
				recordTraffic = true
			} else {
				conn.logger.Debug("Malformed request", zap.Any("ExpectedRecvBytes", expectedRecvBytes), zap.Any("ActualRecvBytes", actualRecvBytes))
				recordTraffic = false
			}

			if len(conn.currentRecvBufQ) > 0 { //validated request, invalided response
				requestBuf = conn.currentRecvBufQ[0]
				//popping out the current request data
				conn.currentRecvBufQ = conn.currentRecvBufQ[1:]

				responseBuf = conn.SentBuf

				conn.resTimestampTest = time.Now()
			} else {
				conn.logger.Debug("no data buffer for request", zap.Any("Length of RecvBufQueue", len(conn.currentRecvBufQ)))
				recordTraffic = false
			}

		} else {
			conn.logger.Error("malformed request")
			recordTraffic = false
		}

		conn.logger.Debug(fmt.Sprintf("recording traffic after verifying the request data (but not response data):%v", recordTraffic))
		//treat immediate next request as first request (2 seconds after last activity)
		conn.resetConnection()

		conn.logger.Debug("unverified recording", zap.Any("recordTraffic", recordTraffic))
	}

	var reqTimestampTest time.Time
	// Checking if record traffic is recorded and reqeust timestamp is captured or not.
	if recordTraffic && len(conn.reqTimestampTest) > 0 {
		// Get the timestamp of current request
		reqTimestampTest = conn.reqTimestampTest[0]
		// Pop the timestamp of current request
		conn.reqTimestampTest = conn.reqTimestampTest[1:]
	}

	return recordTraffic, requestBuf, responseBuf, reqTimestampTest, conn.resTimestampTest
	// // Check if other conditions for completeness are met.
	// return conn.closeTimestamp != 0 &&
	// 	conn.totalReadBytes == conn.recvBytes &&
	// 	conn.totalWrittenBytes == conn.sentBytes
}

func (conn *Tracker) resetConnection() {
	conn.firstRequest = true
	conn.receivedResponse = false
	conn.receivedRequest = false
	conn.recvBytes = 0
	conn.sentBytes = 0
	conn.SentBuf = []byte{}
	conn.RecvBuf = []byte{}
}

func (conn *Tracker) verifyRequestData(expectedRecvBytes, actualRecvBytes uint64) bool {
	return (expectedRecvBytes == actualRecvBytes)
}

func (conn *Tracker) verifyResponseData(expectedSentBytes, actualSentBytes uint64) bool {
	return (expectedSentBytes == actualSentBytes)
}

// func (conn *Tracker) Malformed() bool {
// 	conn.mutex.RLock()
// 	defer conn.mutex.RUnlock()
// 	// conn.logger.Debug("data loss of ingress request message", zap.Any("bytes read in ebpf", conn.totalReadBytes), zap.Any("bytes received in userspace", conn.recvBytes))
// 	// conn.logger.Debug("data loss of ingress response message", zap.Any("bytes written in ebpf", conn.totalWrittenBytes), zap.Any("bytes sent to user", conn.sentBytes))
// 	// conn.logger.Debug("", zap.Any("Request buffer", string(conn.RecvBuf)))
// 	// conn.logger.Debug("", zap.Any("Response buffer", string(conn.SentBuf)))
// 	return conn.closeTimestamp != 0 &&
// 		conn.totalReadBytes != conn.recvBytes &&
// 		conn.totalWrittenBytes != conn.sentBytes
// }

func (conn *Tracker) AddDataEvent(event structs2.SocketDataEvent) {
	conn.mutex.Lock()
	defer conn.mutex.Unlock()
	conn.UpdateTimestamps()

	direction := ""
	if event.Direction == structs2.EgressTraffic {
		direction = "Egress"
	} else if event.Direction == structs2.IngressTraffic {
		direction = "Ingress"
	}

	conn.logger.Debug(fmt.Sprintf("Got a data event from eBPF, Direction:%v || current Event Size:%v || ConnectionID:%v\n", direction, event.MsgSize, event.ConnID))

	switch event.Direction {
	case structs2.EgressTraffic:
		if !conn.isNewRequest {
			conn.isNewRequest = true
		}

		// Assign the size of the message to the variable msgLengt
		msgLength := event.MsgSize
		// If the size of the message exceeds the maximum allowed size,
		// set msgLength to the maximum allowed size instead
		if event.MsgSize > structs2.EventBodyMaxSize {
			msgLength = structs2.EventBodyMaxSize
		}
		// Append the message (up to msgLength) to the connection's sent buffer
		conn.SentBuf = append(conn.SentBuf, event.Msg[:msgLength]...)
		conn.sentBytes += uint64(event.MsgSize)

		//Handling multiple request on same connection to support connection:keep-alive
		if conn.firstRequest || conn.receivedRequest {
			conn.currentRecvBytesQ = append(conn.currentRecvBytesQ, conn.recvBytes)
			conn.recvBytes = 0

			conn.currentRecvBufQ = append(conn.currentRecvBufQ, conn.RecvBuf)
			conn.RecvBuf = []byte{}

			conn.receivedRequest = false
			conn.receivedResponse = true

			conn.totalRecvBytesQ = append(conn.totalRecvBytesQ, uint64(event.ValidateReadBytes))
			conn.firstRequest = false
		}

	case structs2.IngressTraffic:
		// Capturing the timestamp of request as the request just started to come.
		if conn.isNewRequest {
			conn.reqTimestampTest = append(conn.reqTimestampTest, ConvertUnixNanoToTime(event.EntryTimestampNano))
			conn.isNewRequest = false
		}

		// Assign the size of the message to the variable msgLength
		msgLength := event.MsgSize
		// If the size of the message exceeds the maximum allowed size,
		// set msgLength to the maximum allowed size instead
		if event.MsgSize > structs2.EventBodyMaxSize {
			msgLength = structs2.EventBodyMaxSize
		}
		// Append the message (up to msgLength) to the connection's receive buffer
		conn.RecvBuf = append(conn.RecvBuf, event.Msg[:msgLength]...)
		conn.recvBytes += uint64(event.MsgSize)

		//Handling multiple request on same connection to support connection:keep-alive
		if !conn.firstRequest || conn.receivedResponse {
			conn.currentSentBytesQ = append(conn.currentSentBytesQ, conn.sentBytes)
			conn.sentBytes = 0

			conn.currentSentBufQ = append(conn.currentSentBufQ, conn.SentBuf)
			conn.SentBuf = []byte{}

			conn.receivedRequest = true
			conn.receivedResponse = false

			conn.totalSentBytesQ = append(conn.totalSentBytesQ, uint64(event.ValidateWrittenBytes))

			//Record a test case for the current request/
			conn.incRecordTestCount()
		}

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
	conn.logger.Debug(fmt.Sprintf("Got a close event from eBPF on connectionId:%v\n", event.ConnID))
}

func (conn *Tracker) UpdateTimestamps() {
	conn.lastActivityTimestamp = uint64(time.Now().UnixNano())
}

// ConvertUnixNanoToTime takes a Unix timestamp in nanoseconds as a uint64 and returns the corresponding time.Time
func ConvertUnixNanoToTime(unixNano uint64) time.Time {
	// Unix time is the number of seconds since January 1, 1970 UTC,
	// so convert nanoseconds to seconds for time.Unix function
	seconds := int64(unixNano / uint64(time.Second))
	nanoRemainder := int64(unixNano % uint64(time.Second))
	return time.Unix(seconds, nanoRemainder)
}
