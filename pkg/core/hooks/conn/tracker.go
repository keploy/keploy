//go:build linux

package conn

import (
	"bytes"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.keploy.io/server/v2/pkg"
	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
	"golang.org/x/net/http2"
	// "log"
)

// Protocol represents the type of protocol being used
type Protocol int

// Protocol constants for supported HTTP protocol versions
const (
	// HTTP1 represents HTTP/1.x protocol
	HTTP1 Protocol = 1
	// HTTP2 represents HTTP/2 protocol
	HTTP2 Protocol = 2
)

// StreamManager interface for managing protocol streams
type StreamManager interface {
	HandleFrame(frame http2.Frame, isOutgoing bool, timestamp time.Time) error
	GetCompleteStreams() []*pkg.HTTP2Stream
	CleanupStream(streamID uint32)
}

// Tracker is a routine-safe container that holds a conn with unique ID, and able to create new conn.
type Tracker struct {
	connID         ID
	addr           SockAddrIn
	openTimestamp  uint64
	closeTimestamp uint64

	// Indicates the tracker stopped tracking due to closing the session.
	lastActivityTimestamp uint64

	// Queues to handle multiple ingress traffic on the same conn (keep-alive)

	// kernelRespSizes is a slice of the total number of Response bytes received in the kernel side
	kernelRespSizes []uint64

	// kernelReqSizes is a slice of the total number of Request bytes received in the kernel side
	kernelReqSizes []uint64

	// userRespSizes is a slice of the total number of Response bytes received in the user side
	userRespSizes []uint64

	// userReqSizes is a slice of the total number of Request bytes received in the user side
	userReqSizes []uint64
	// userRespBufs is a slice of the Response data received in the user side on this conn
	userResps [][]byte
	// userReqBufs is a slice of the Request data received in the user side on this conn
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
	// firstRequest is used to indicate if the current request is the first request on the conn
	// reset after 2 seconds of inactivity
	// Note: This is used to handle multiple requests on the same conn (keep-alive)
	// Its different from isNewRequest which is used to indicate if the current request chunk is the first chunk of the request
	firstRequest bool

	mutex  sync.RWMutex
	logger *zap.Logger

	reqTimestamps []time.Time
	isNewRequest  bool

	// New fields to support protocol detection and http2 stream management
	protocol         Protocol
	streamMgr        StreamManager
	protocolDetected bool
	buffer           []byte
}

// NewTracker creates a new connection tracker
func NewTracker(connID ID, logger *zap.Logger) *Tracker {
	t := &Tracker{
		connID: connID,
		logger: logger,
		mutex:  sync.RWMutex{},
		// Initialize HTTP/1 fields
		req:             []byte{},
		resp:            []byte{},
		kernelRespSizes: []uint64{},
		kernelReqSizes:  []uint64{},
		userRespSizes:   []uint64{},
		userReqSizes:    []uint64{},
		userResps:       [][]byte{},
		userReqs:        [][]byte{},
		firstRequest:    true,
		isNewRequest:    true,
		buffer:          make([]byte, 0, pkg.DefaultMaxFrameSize),
	}

	// Always start with HTTP/1
	t.protocol = HTTP1
	t.protocolDetected = false // Allow protocol detection

	return t
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

// reset resets the conn's request and response data buffers.
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
	return expectedRecvBytes == actualRecvBytes
}

func (conn *Tracker) verifyResponseData(expectedSentBytes, actualSentBytes uint64) bool {
	return expectedSentBytes == actualSentBytes
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

func (conn *Tracker) AddOpenEvent(event SocketOpenEvent) {
	conn.mutex.Lock()
	defer conn.mutex.Unlock()
	conn.UpdateTimestamps()
	conn.addr = event.Addr
	if conn.openTimestamp != 0 && conn.openTimestamp != event.TimestampNano {
		conn.logger.Debug("Changed open info timestamp due to new request", zap.Any("from", conn.openTimestamp), zap.Any("to", event.TimestampNano))
	}
	conn.openTimestamp = event.TimestampNano
}

func (conn *Tracker) AddDataEventBig(event SocketDataEventBig) {
	conn.mutex.Lock()
	defer conn.mutex.Unlock()
	conn.UpdateTimestamps()
	msgLength := event.MsgSize
	// If the size of the message exceeds the maximum allowed size,
	// set msgLength to the maximum allowed size instead
	// if event.MsgSize > EventBodyMaxSize {
	// 	msgLength = EventBodyMaxSize
	// }
	// Trim leading zeros from the data
	start := 3
	for ; start < len(event.Msg)-1; start++ {
		if event.Msg[start] != 0 {
			break
		}
	}
	fmt.Println("Start index:", start)
	fmt.Println("Length of data:", event.Msg[start])
	data := event.Msg[start:]
	data = data[:msgLength]
	// spew.Dump(data)
	// Check for HTTP/2 preface if we haven't detected protocol yet
	if !conn.protocolDetected {
		conn.logger.Debug("Connection check")
		if isHTTP2Request(data) {
			// Create HTTP/2 parser and stream manager
			conn.protocol = HTTP2
			conn.streamMgr = pkg.NewStreamManager(conn.logger)
			conn.protocolDetected = true
			conn.logger.Debug("Detected HTTP/2 protocol (preface received)")

			// If there's more data after preface, process it as HTTP/2
			if len(data) > 24 {
				// Create new event with remaining data
				newEvent := event
				copy(newEvent.Msg[:], data[24:])
				newEvent.MsgSize = uint32(len(data) - 24)
				conn.handleHTTP2DataBig(newEvent)
			}
			return
		}

		// If we see a valid HTTP/1 request line, mark as HTTP/1
		if isHTTP1Request(data) {
			conn.protocolDetected = true
			conn.logger.Debug("Detected HTTP/1.x protocol")
		}
	}

	// Process based on current protocol
	switch conn.protocol {
	case HTTP2:
		conn.handleHTTP2DataBig(event)
	default:
		conn.handleHTTP1DataBig(event)
	}
}

func (conn *Tracker) AddDataEventSmall(event SocketDataEventSmall) {
	conn.mutex.Lock()
	defer conn.mutex.Unlock()
	conn.UpdateTimestamps()
	msgLength := event.MsgSize
	// If the size of the message exceeds the maximum allowed size,
	// set msgLength to the maximum allowed size instead
	if event.MsgSize > EventBodyMaxSize {
		msgLength = EventBodyMaxSize
	}
	// Trim leading zeros from the data
	start := 3
	for ; start < len(event.Msg)-1; start++ {
		if event.Msg[start] != 0 {
			break
		}
	}
	fmt.Println("Start index:", start)
	fmt.Println("Length of data:", event.Msg[start])
	data := event.Msg[start:]
	data = data[:msgLength]
	// spew.Dump(data)
	// Check for HTTP/2 preface if we haven't detected protocol yet
	if !conn.protocolDetected {
		conn.logger.Debug("Connection check")
		if isHTTP2Request(data) {
			// Create HTTP/2 parser and stream manager
			conn.protocol = HTTP2
			conn.streamMgr = pkg.NewStreamManager(conn.logger)
			conn.protocolDetected = true
			conn.logger.Debug("Detected HTTP/2 protocol (preface received)")

			// If there's more data after preface, process it as HTTP/2
			if len(data) > 24 {
				// Create new event with remaining data
				newEvent := event
				copy(newEvent.Msg[:], data[24:])
				newEvent.MsgSize = uint32(len(data) - 24)
				conn.handleHTTP2DataSmall(newEvent)
			}
			return
		}

		// If we see a valid HTTP/1 request line, mark as HTTP/1
		if isHTTP1Request(data) {
			conn.protocolDetected = true
			conn.logger.Debug("Detected HTTP/1.x protocol")
		}
	}

	// Process based on current protocol
	switch conn.protocol {
	case HTTP2:
		conn.handleHTTP2DataSmall(event)
	default:
		conn.handleHTTP1DataSmall(event)
	}
}
// isHTTP1Request checks if the data starts with a valid HTTP/1 request line
func isHTTP1Request(data []byte) bool {
	// Convert to string for easier checking
	s := string(data)

	// Check for common HTTP methods
	return strings.HasPrefix(s, "GET ") ||
		strings.HasPrefix(s, "POST ") ||
		strings.HasPrefix(s, "PUT ") ||
		strings.HasPrefix(s, "DELETE ") ||
		strings.HasPrefix(s, "HEAD ") ||
		strings.HasPrefix(s, "OPTIONS ") ||
		strings.HasPrefix(s, "PATCH ")
}

// isHTTP2Request checks if the data starts with the HTTP/2 connection preface
func isHTTP2Request(data []byte) bool {
	return len(data) >= 24 && bytes.Equal(data[:24], []byte(pkg.HTTP2Preface))
}

func (conn *Tracker) AddCloseEvent(event SocketCloseEvent) {
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

// getHTTP2CompletedStream returns a completed HTTP2/gRPC stream if available
func (conn *Tracker) getHTTP2CompletedStream() *pkg.HTTP2Stream {
	conn.mutex.RLock()
	defer conn.mutex.RUnlock()

	if conn.streamMgr == nil {
		return nil
	}

	streams := conn.streamMgr.GetCompleteStreams()
	if len(streams) == 0 {
		return nil
	}

	// Return the first completed stream
	stream := streams[0]

	// Cleanup the processed stream
	conn.streamMgr.CleanupStream(stream.ID)

	return stream
}

// Existing HTTP/1 completion check
func (conn *Tracker) isHTTP1Complete() (bool, []byte, []byte, time.Time, time.Time) {
	conn.mutex.RLock()
	defer conn.mutex.RUnlock()

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
			// fmt.Println("expectedRecvBytes", expectedRecvBytes)
			// fmt.Println("actualRecvBytes", actualRecvBytes)
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
			}

			if len(conn.userReqs) > 0 && len(conn.userResps) > 0 { //validated request, response
				requestBuf = conn.userReqs[0]
				responseBuf = conn.userResps[0]

				//popping out the current request & response data
				conn.userReqs = conn.userReqs[1:]
				conn.userResps = conn.userResps[1:]
			} else {
				conn.logger.Debug("no data buffer for request or response", zap.Any("Length of RecvBufQueue", len(conn.userReqs)), zap.Any("Length of SentBufQueue", len(conn.userResps)))
			}

			recordTraffic = validReq && validRes
		} else {
			utils.LogError(conn.logger, nil, "malformed request or response")
			recordTraffic = false
		}

		conn.logger.Debug(fmt.Sprintf("recording traffic after verifying the request and reponse data:%v", recordTraffic))

		// // decrease the recTestCounter
		conn.decRecordTestCount()
		conn.logger.Debug("verified recording", zap.Any("recordTraffic", recordTraffic))
	} else if conn.lastChunkWasResp && elapsedTime >= uint64(time.Second*2) { // Check if 2 seconds has passed since the last activity.
		conn.logger.Debug("might be last request on the conn")

		if len(conn.userReqSizes) > 0 && len(conn.kernelReqSizes) > 0 {

			expectedRecvBytes := conn.userReqSizes[0]
			actualRecvBytes := conn.kernelReqSizes[0]

			//popping out the current request info
			conn.userReqSizes = conn.userReqSizes[1:]
			conn.kernelReqSizes = conn.kernelReqSizes[1:]
			// fmt.Println("wanted this :")
			// spew.Dump(conn)
			// fmt.Println("expectedRecvBytes", expectedRecvBytes)
			// fmt.Println("actualRecvBytes", actualRecvBytes)

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
			utils.LogError(conn.logger, nil, "malformed request")
			recordTraffic = false
		}

		conn.logger.Debug(fmt.Sprintf("recording traffic after verifying the request data (but not response data):%v", recordTraffic))
		//treat immediate next request as first request (2 seconds after last activity)
		// this can be to avoid potential corruption in the conn
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
	// spew.Dump("requestbuff :", requestBuf)
	// spew.Dump("responsebuf :", responseBuf)
	return recordTraffic, requestBuf, responseBuf, reqTimestamps, respTimestamp
}

// Add HTTP/2 specific handling
func (conn *Tracker) handleHTTP2DataBig(event SocketDataEventBig) {
	// Convert fixed-size array to slice
	msgLength := event.MsgSize
	// If the size of the message exceeds the maximum allowed size,
	// set msgLength to the maximum allowed size instead
	// if event.MsgSize > EventBodyMaxSize {
	// 	msgLength = EventBodyMaxSize
	// }
	// data := event.Msg[:event.MsgSize]

	// Append new data to the buffer
	conn.buffer = append(conn.buffer, event.Msg[:msgLength]...)

	// Process as many complete frames as possible
	for len(conn.buffer) >= 9 { // Minimum frame size
		frame, consumed, err := pkg.ExtractHTTP2Frame(conn.buffer)
		if err != nil {
			if strings.Contains(err.Error(), "incomplete frame") {
				conn.logger.Debug("Incomplete frame", zap.Any("error", err))
				// Not enough data yet, wait for more
				break
			}
			// Real error, log and remove the problematic data
			conn.logger.Error("Failed to extract HTTP/2 frame", zap.Error(err))
			if len(conn.buffer) > 9 {
				// Try to recover by removing the first byte and trying again next time
				conn.buffer = conn.buffer[1:]
			} else {
				conn.buffer = nil
			}
			break
		}

		// Handle the frame
		if err := conn.streamMgr.HandleFrame(frame, event.Direction == EgressTraffic, ConvertUnixNanoToTime(event.TimestampNano)); err != nil {
			conn.logger.Error("Failed to handle HTTP/2 frame", zap.Error(err))
		}

		// Remove processed data from buffer
		conn.buffer = conn.buffer[consumed:]
	}

	// Store timestamps for requests
	if event.Direction == IngressTraffic {
		conn.reqTimestamps = append(conn.reqTimestamps, ConvertUnixNanoToTime(event.EntryTimestampNano))
	}
}
// Add HTTP/2 specific handling
func (conn *Tracker) handleHTTP2DataSmall(event SocketDataEventSmall) {
	// Convert fixed-size array to slice
	msgLength := event.MsgSize
	// If the size of the message exceeds the maximum allowed size,
	// set msgLength to the maximum allowed size instead
	if event.MsgSize > EventBodyMaxSize {
		msgLength = EventBodyMaxSize
	}
	// data := event.Msg[:event.MsgSize]

	// Append new data to the buffer
	conn.buffer = append(conn.buffer, event.Msg[:msgLength]...)

	// Process as many complete frames as possible
	for len(conn.buffer) >= 9 { // Minimum frame size
		frame, consumed, err := pkg.ExtractHTTP2Frame(conn.buffer)
		if err != nil {
			if strings.Contains(err.Error(), "incomplete frame") {
				conn.logger.Debug("Incomplete frame", zap.Any("error", err))
				// Not enough data yet, wait for more
				break
			}
			// Real error, log and remove the problematic data
			conn.logger.Error("Failed to extract HTTP/2 frame", zap.Error(err))
			if len(conn.buffer) > 9 {
				// Try to recover by removing the first byte and trying again next time
				conn.buffer = conn.buffer[1:]
			} else {
				conn.buffer = nil
			}
			break
		}

		// Handle the frame
		if err := conn.streamMgr.HandleFrame(frame, event.Direction == EgressTraffic, ConvertUnixNanoToTime(event.TimestampNano)); err != nil {
			conn.logger.Error("Failed to handle HTTP/2 frame", zap.Error(err))
		}

		// Remove processed data from buffer
		conn.buffer = conn.buffer[consumed:]
	}

	// Store timestamps for requests
	if event.Direction == IngressTraffic {
		conn.reqTimestamps = append(conn.reqTimestamps, ConvertUnixNanoToTime(event.EntryTimestampNano))
	}
}
// Existing HTTP/1 handling
func (conn *Tracker) handleHTTP1DataBig(event SocketDataEventBig) {
	conn.logger.Debug(fmt.Sprintf("Got a data event from eBPF, Direction:%v || current Event Size:%v || ConnectionID:%v\n", event.Direction, event.MsgSize, event.ConnID))
	// fmt.Println("here is the event")
	// spew.Dump(event)
	// fmt.Println("here is the msg length :", event.MsgSize)
	// if event.MsgSize == 0 {
	// 	conn.logger.Debug("Received empty message, skipping")
	// 	return
	// }
	switch event.Direction {
	case EgressTraffic:
		// Capturing the timestamp of response as the response just started to come.
		// This is to ensure that we capture the response timestamp for the first chunk of the response.
		if !conn.isNewRequest {
			conn.isNewRequest = true
		}

		// Assign the size of the message to the variable msgLength
		msgLength := event.MsgSize
		// If the size of the message exceeds the maximum allowed size,
		// set msgLength to the maximum allowed size instead
		// if event.MsgSize > EventBodyMaxSize {
		// 	msgLength = EventBodyMaxSize
		// }

		start := 3
		for ; start < len(event.Msg)-1; start++ {
			if event.Msg[start] != 0 {
				break
			}
		}
		fmt.Println("Start index:", start)
		// fmt.Println("Length of data:", event.Msg[start])
		data := event.Msg[start:]
		data = data[:msgLength]
		// Append the message (up to msgLength) to the conn's sent buffer
		// fmt.Println("here is the connreps :")
		conn.resp = append(conn.resp, data...)
		// spew.Dump(conn.resp)
		// spew.Dump(event.Msg)
		conn.respSize += uint64(event.MsgSize)

		//Handling multiple request on same conn to support conn:keep-alive
		if conn.firstRequest || conn.lastChunkWasReq {
			conn.userReqSizes = append(conn.userReqSizes, conn.reqSize)

			conn.userReqs = append(conn.userReqs, conn.req)
			conn.req = []byte{}

			conn.lastChunkWasReq = false
			conn.lastChunkWasResp = true
			fmt.Println("here is the validate read bytes :", event.ValidateReadBytes)
			conn.kernelReqSizes = append(conn.kernelReqSizes, uint64(conn.reqSize))
			conn.reqSize = 0
			conn.firstRequest = false
		}

	case IngressTraffic:
		conn.logger.Debug("isNewRequest", zap.Any("isNewRequest", conn.isNewRequest), zap.Any("connID", conn.connID))
		// Capturing the timestamp of request as the request just started to come.
		if conn.isNewRequest {
			conn.reqTimestamps = append(conn.reqTimestamps, ConvertUnixNanoToTime(event.EntryTimestampNano))
			conn.isNewRequest = false
		}

		// Assign the size of the message to the variable msgLength
		msgLength := event.MsgSize
		// If the size of the message exceeds the maximum allowed size,
		// set msgLength to the maximum allowed size instead
		// if event.MsgSize > EventBodyMaxSize {
		// 	msgLength = EventBodyMaxSize
		// }

		start := 3
		for ; start < len(event.Msg)-1; start++ {
			if event.Msg[start] != 0 {
				break
			}
		}
		// fmt.Println("Start index:", start)
		// spew.Dump(event.Msg)
		// fmt.Println("Length of data:", event.Msg[start])
		data := event.Msg[start:]
		data = data[:msgLength]
		// Append the message (up to msgLength) to the conn's receive buffer
		conn.req = append(conn.req, data...)
		// spew.Dump(conn.req)
		conn.reqSize += uint64(event.MsgSize)

		//Handling multiple request on same conn to support conn:keep-alive
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
// Existing HTTP/1 handling
func (conn *Tracker) handleHTTP1DataSmall(event SocketDataEventSmall) {
	conn.logger.Debug(fmt.Sprintf("Got a data event from eBPF, Direction:%v || current Event Size:%v || ConnectionID:%v\n", event.Direction, event.MsgSize, event.ConnID))
	// fmt.Println("here is the event")
	// spew.Dump(event)
	// fmt.Println("here is the msg length :", event.MsgSize)
	// if event.MsgSize == 0 {
	// 	conn.logger.Debug("Received empty message, skipping")
	// 	return
	// }
	switch event.Direction {
	case EgressTraffic:
		// Capturing the timestamp of response as the response just started to come.
		// This is to ensure that we capture the response timestamp for the first chunk of the response.
		if !conn.isNewRequest {
			conn.isNewRequest = true
		}

		// Assign the size of the message to the variable msgLength
		msgLength := event.MsgSize
		// If the size of the message exceeds the maximum allowed size,
		// set msgLength to the maximum allowed size instead
		if event.MsgSize > EventBodyMaxSize {
			msgLength = EventBodyMaxSize
		}

		start := 3
		for ; start < len(event.Msg)-1; start++ {
			if event.Msg[start] != 0 {
				break
			}
		}
		fmt.Println("Start index:", start)
		// fmt.Println("Length of data:", event.Msg[start])
		data := event.Msg[start:]
		data = data[:msgLength]
		// Append the message (up to msgLength) to the conn's sent buffer
		// fmt.Println("here is the connreps :")
		conn.resp = append(conn.resp, data...)
		// spew.Dump(conn.resp)
		// spew.Dump(event.Msg)
		conn.respSize += uint64(event.MsgSize)

		//Handling multiple request on same conn to support conn:keep-alive
		if conn.firstRequest || conn.lastChunkWasReq {
			conn.userReqSizes = append(conn.userReqSizes, conn.reqSize)

			conn.userReqs = append(conn.userReqs, conn.req)
			conn.req = []byte{}

			conn.lastChunkWasReq = false
			conn.lastChunkWasResp = true
			fmt.Println("here is the validate read bytes :", event.ValidateReadBytes)
			conn.kernelReqSizes = append(conn.kernelReqSizes, uint64(conn.reqSize))
			conn.reqSize = 0
			conn.firstRequest = false
		}

	case IngressTraffic:
		conn.logger.Debug("isNewRequest", zap.Any("isNewRequest", conn.isNewRequest), zap.Any("connID", conn.connID))
		// Capturing the timestamp of request as the request just started to come.
		if conn.isNewRequest {
			conn.reqTimestamps = append(conn.reqTimestamps, ConvertUnixNanoToTime(event.EntryTimestampNano))
			conn.isNewRequest = false
		}

		// Assign the size of the message to the variable msgLength
		msgLength := event.MsgSize
		// If the size of the message exceeds the maximum allowed size,
		// set msgLength to the maximum allowed size instead
		if event.MsgSize > EventBodyMaxSize {
			msgLength = EventBodyMaxSize
		}

		start := 3
		for ; start < len(event.Msg)-1; start++ {
			if event.Msg[start] != 0 {
				break
			}
		}
		// fmt.Println("Start index:", start)
		// spew.Dump(event.Msg)
		// fmt.Println("Length of data:", event.Msg[start])
		data := event.Msg[start:]
		data = data[:msgLength]
		// Append the message (up to msgLength) to the conn's receive buffer
		conn.req = append(conn.req, data...)
		// spew.Dump(conn.req)
		conn.reqSize += uint64(event.MsgSize)

		//Handling multiple request on same conn to support conn:keep-alive
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
