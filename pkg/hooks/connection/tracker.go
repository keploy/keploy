package connection

import (
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	structs2 "go.keploy.io/server/pkg/hooks/structs"
	"go.keploy.io/server/pkg/models"
	"go.uber.org/zap"
	// "log"
)

const (
	maxBufferSize = 16 * 1024 * 1024 // 16MB
)

// Tracker is a routine-safe container that holds a connection with unique ID, and able to create new connection.
type Tracker struct {
	connID         structs2.ConnID
	addr           structs2.SockAddrIn
	openTimestamp  uint64
	closeTimestamp uint64

	// Indicates the tracker stopped tracking due to closing the session.
	lastActivityTimestamp uint64

	// Queues to handle multiple ingress traffic on the same connection (keep-alive)

	// kernelRespSizes is a slice of the total number of Response bytes received in the kernel side
	kernelRespSizes []uint64

	// kernelReqSizes is a slice of the total number of Request bytes received in the kernel side
	kernelReqSizes []uint64

	// userRespSizes is a slice of the total number of Response bytes received in the user side
	userRespSizes []uint64

	// userReqSizes is a slice of the total number of Request bytes received in the user side
	userReqSizes []uint64
	// userRespBufs is a slice of the Response data received in the user side on this connection
	userResps [][]byte
	// userReqBufs is a slice of the Request data received in the user side on this connection
	userReqs [][]byte

	// req and resp are the buffers to store the request and response data for the current request
	// reset after 2 seconds of inactivity
	respSize uint64
	reqSize  uint64
	resp     []byte
	req      []byte

	// Additional fields to know when to capture request or response info
	// reset after 2 seconds of inactivity
	lastChunkWasResp bool
	lastChunkWasReq  bool
	recTestCounter   int32 //atomic counter
	// firstRequest is used to indicate if the current request is the first request on the connection
	// reset after 2 seconds of inactivity
	// Note: This is used to handle multiple requests on the same connection (keep-alive)
	// Its different from isNewRequest which is used to indicate if the current request chunk is the first chunk of the request
	firstRequest bool

	mutex  sync.RWMutex
	logger *zap.Logger

	reqTimestamps []time.Time
	isNewRequest  bool
}

func NewTracker(connID structs2.ConnID, logger *zap.Logger) *Tracker {
	return &Tracker{
		connID:          connID,
		req:             []byte{},
		resp:            []byte{},
		kernelRespSizes: []uint64{},
		kernelReqSizes:  []uint64{},
		userRespSizes:   []uint64{},
		userReqSizes:    []uint64{},
		userResps:       [][]byte{},
		userReqs:        [][]byte{},
		mutex:           sync.RWMutex{},
		logger:          logger,
		firstRequest:    true,
		isNewRequest:    true,
	}
}

func (conn *Tracker) ToBytes() ([]byte, []byte) {
	conn.mutex.RLock()
	defer conn.mutex.RUnlock()
	return conn.req, conn.resp
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

	var reqTimestamps, respTimestamp time.Time

	//if recTestCounter > 0, it means that we have num(recTestCounter) of request and response present in the queues to record.
	if conn.recTestCounter > 0 {
		if (len(conn.userReqSizes) > 0 && len(conn.kernelReqSizes) > 0) &&
			(len(conn.userRespSizes) > 0 && len(conn.kernelRespSizes) > 0) {
			validReq, validRes := false, false

			expectedRecvBytes := conn.userReqSizes[0]
			actualRecvBytes := conn.kernelReqSizes[0]

			if expectedRecvBytes == 0 || actualRecvBytes == 0 {
				conn.logger.Warn("Malformed request", zap.Any("ExpectedRecvBytes", expectedRecvBytes), zap.Any("ActualRecvBytes", actualRecvBytes))
			}

			//popping out the current request info
			conn.userReqSizes = conn.userReqSizes[1:]
			conn.kernelReqSizes = conn.kernelReqSizes[1:]

			if conn.verifyRequestData(expectedRecvBytes, actualRecvBytes) {
				validReq = true
			} else {
				conn.logger.Debug("Malformed request", zap.Any("ExpectedRecvBytes", expectedRecvBytes), zap.Any("ActualRecvBytes", actualRecvBytes))
				recordTraffic = false
			}

			expectedSentBytes := conn.userRespSizes[0]
			actualSentBytes := conn.kernelRespSizes[0]

			//popping out the current response info
			conn.userRespSizes = conn.userRespSizes[1:]
			conn.kernelRespSizes = conn.kernelRespSizes[1:]

			if conn.verifyResponseData(expectedSentBytes, actualSentBytes) {
				validRes = true
				respTimestamp = time.Now()
			} else {
				conn.logger.Debug("Malformed response", zap.Any("ExpectedSentBytes", expectedSentBytes), zap.Any("ActualSentBytes", actualSentBytes))
				recordTraffic = false
			}

			if len(conn.userReqs) > 0 && len(conn.userResps) > 0 { //validated request, response
				requestBuf = conn.userReqs[0]
				responseBuf = conn.userResps[0]

				//popping out the current request & response data
				conn.userReqs = conn.userReqs[1:]
				conn.userResps = conn.userResps[1:]
			} else {
				conn.logger.Debug("no data buffer for request or response", zap.Any("Length of RecvBufQueue", len(conn.userReqs)), zap.Any("Length of SentBufQueue", len(conn.userResps)))
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
	} else if conn.lastChunkWasResp && elapsedTime >= uint64(time.Second*2) { // Check if 2 seconds has passed since the last activity.
		conn.logger.Debug("might be last request on the connection")

		if len(conn.userReqSizes) > 0 && len(conn.kernelReqSizes) > 0 {

			expectedRecvBytes := conn.userReqSizes[0]
			actualRecvBytes := conn.kernelReqSizes[0]

			//popping out the current request info
			conn.userReqSizes = conn.userReqSizes[1:]
			conn.kernelReqSizes = conn.kernelReqSizes[1:]

			if expectedRecvBytes == 0 || actualRecvBytes == 0 {
				conn.logger.Warn("Malformed request", zap.Any("ExpectedRecvBytes", expectedRecvBytes), zap.Any("ActualRecvBytes", actualRecvBytes))
			}

			if conn.verifyRequestData(expectedRecvBytes, actualRecvBytes) {
				recordTraffic = true
			} else {
				conn.logger.Debug("Malformed request", zap.Any("ExpectedRecvBytes", expectedRecvBytes), zap.Any("ActualRecvBytes", actualRecvBytes))
				recordTraffic = false
			}

			if len(conn.userReqs) > 0 { //validated request, invalided response
				requestBuf = conn.userReqs[0]
				//popping out the current request data
				conn.userReqs = conn.userReqs[1:]

				responseBuf = conn.resp
				respTimestamp = time.Now()
			} else {
				conn.logger.Debug("no data buffer for request", zap.Any("Length of RecvBufQueue", len(conn.userReqs)))
				recordTraffic = false
			}

		} else {
			conn.logger.Error("malformed request")
			recordTraffic = false
		}

		conn.logger.Debug(fmt.Sprintf("recording traffic after verifying the request data (but not response data):%v", recordTraffic))
		//treat immediate next request as first request (2 seconds after last activity)
		// this can be to avoid potential corruption in the connection
		conn.reset()

		conn.logger.Debug("unverified recording", zap.Any("recordTraffic", recordTraffic))
	}

	// Checking if record traffic is recorded and request & response timestamp is captured or not.
	if recordTraffic {
		if len(conn.reqTimestamps) > 0 {
			// Get the timestamp of current request
			reqTimestamps = conn.reqTimestamps[0]
			// Pop the timestamp of current request
			conn.reqTimestamps = conn.reqTimestamps[1:]
		} else {
			conn.logger.Debug("no request timestamp found")
			if len(requestBuf) > 0 {
				reqLine := strings.Split(string(requestBuf), "\n")
				if models.GetMode() == models.MODE_RECORD && len(reqLine) > 0 && reqLine[0] != "" {
					conn.logger.Warn(fmt.Sprintf("failed to capture request timestamp for a request. Please record it again if important:%v", reqLine[0]))
				}
			}
			recordTraffic = false
		}

		conn.logger.Debug(fmt.Sprintf("TestRequestTimestamp:%v || TestResponseTimestamp:%v", reqTimestamps, respTimestamp))
	}

	return recordTraffic, requestBuf, responseBuf, reqTimestamps, respTimestamp
}

// reset resets the connection's request and response data buffers.
func (conn *Tracker) reset() {
	conn.firstRequest = true
	conn.lastChunkWasResp = false
	conn.lastChunkWasReq = false
	conn.reqSize = 0
	conn.respSize = 0
	conn.resp = []byte{}
	conn.req = []byte{}
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
// 	// conn.log.Debug("data loss of ingress request message", zap.Any("bytes read in ebpf", conn.totalReadBytes), zap.Any("bytes received in userspace", conn.reqSize))
// 	// conn.log.Debug("data loss of ingress response message", zap.Any("bytes written in ebpf", conn.totalWrittenBytes), zap.Any("bytes sent to user", conn.respSize))
// 	// conn.log.Debug("", zap.Any("Request buffer", string(conn.req)))
// 	// conn.log.Debug("", zap.Any("Response buffer", string(conn.resp)))
// 	return conn.closeTimestamp != 0 &&
// 		conn.totalReadBytes != conn.reqSize &&
// 		conn.totalWrittenBytes != conn.respSize
// }

func (conn *Tracker) AddDataEvent(event structs2.SocketDataEvent) {
	conn.mutex.Lock()
	defer conn.mutex.Unlock()
	conn.UpdateTimestamps()

	conn.logger.Debug(fmt.Sprintf("Got a data event from eBPF, Direction:%v || current Event Size:%v || ConnectionID:%v\n", event.Direction, event.MsgSize, event.ConnID))

	switch event.Direction {
	case structs2.EgressTraffic:
		// Capturing the timestamp of response as the response just started to come.
		// This is to ensure that we capture the response timestamp for the first chunk of the response.
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
		conn.resp = append(conn.resp, event.Msg[:msgLength]...)
		conn.respSize += uint64(event.MsgSize)

		//Handling multiple request on same connection to support connection:keep-alive
		if conn.firstRequest || conn.lastChunkWasReq {
			conn.userReqSizes = append(conn.userReqSizes, conn.reqSize)
			conn.reqSize = 0

			conn.userReqs = append(conn.userReqs, conn.req)
			conn.req = []byte{}

			conn.lastChunkWasReq = false
			conn.lastChunkWasResp = true

			conn.kernelReqSizes = append(conn.kernelReqSizes, uint64(event.ValidateReadBytes))
			conn.firstRequest = false
		}

	case structs2.IngressTraffic:
		// Capturing the timestamp of request as the request just started to come.
		if conn.isNewRequest {
			conn.reqTimestamps = append(conn.reqTimestamps, ConvertUnixNanoToTime(event.EntryTimestampNano))
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
		conn.req = append(conn.req, event.Msg[:msgLength]...)
		conn.reqSize += uint64(event.MsgSize)

		//Handling multiple request on same connection to support connection:keep-alive
		if conn.lastChunkWasResp {
			// conn.userRespSizes is the total numner of bytes received in the user side
			// consumer for the last response.
			conn.userRespSizes = append(conn.userRespSizes, conn.respSize)
			conn.respSize = 0

			conn.userResps = append(conn.userResps, conn.resp)
			conn.resp = []byte{}

			conn.lastChunkWasReq = true
			conn.lastChunkWasResp = false

			conn.kernelRespSizes = append(conn.kernelRespSizes, uint64(event.ValidateWrittenBytes))

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
	// conn.log.Debug("Got an open event from eBPF", zap.Any("File Descriptor", event.ConnID.FD))
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
