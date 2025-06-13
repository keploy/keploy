//go:build linux

package grpc

import (
	"context"
	"encoding/binary"
	"sync"
	"time"

	"go.keploy.io/server/v2/pkg"
	"go.keploy.io/server/v2/pkg/models"
	"golang.org/x/net/http2"
)

// StreamInfoCollection is a thread-safe data structure to store all communications
// that happen in a stream for grpc. This includes the headers and data frame for the
// request and response.
type StreamInfoCollection struct {
	mutex            sync.Mutex
	StreamInfo       map[uint32]models.GrpcStream
	ReqTimestampMock time.Time
	ResTimestampMock time.Time

	// -------------------------------------------------------------
	// NEW: queue to hold frames that must not be processed re-entrantly
	// -------------------------------------------------------------
	deferred []http2.Frame
}

func NewStreamInfoCollection() *StreamInfoCollection {
	return &StreamInfoCollection{
		StreamInfo: make(map[uint32]models.GrpcStream),
		deferred:   make([]http2.Frame, 0, 8), // small default cap
	}
}

// ---------------------------------------------------------------------------
// Deferred-frame helpers (thread-safe) --------------------------------------
// ---------------------------------------------------------------------------

// DeferFrame queues a frame for later processing by the main loop.
// It is safe to call from any goroutine.
func (sic *StreamInfoCollection) DeferFrame(f http2.Frame) {
	sic.mutex.Lock()
	defer sic.mutex.Unlock()
	sic.deferred = append(sic.deferred, f)
}

// PopDeferredFrame returns the oldest deferred frame (FIFO order) or nil if
// the queue is empty.
func (sic *StreamInfoCollection) PopDeferredFrame() http2.Frame {
	sic.mutex.Lock()
	defer sic.mutex.Unlock()
	if len(sic.deferred) == 0 {
		return nil
	}
	fr := sic.deferred[0]
	sic.deferred = sic.deferred[1:]
	return fr
}

// HasDeferredFrames reports whether the queue is non-empty.
func (sic *StreamInfoCollection) HasDeferredFrames() bool {
	sic.mutex.Lock()
	defer sic.mutex.Unlock()
	return len(sic.deferred) > 0
}

func (sic *StreamInfoCollection) InitialiseStream(streamID uint32) {
	sic.mutex.Lock()
	defer sic.mutex.Unlock()

	_, ok := sic.StreamInfo[streamID]
	if !ok {
		sic.StreamInfo[streamID] = models.NewGrpcStream(streamID)
	}
}

func (sic *StreamInfoCollection) AddHeadersForRequest(streamID uint32, headers map[string]string, isPseudo bool) {
	// Initialise the stream before acquiring the lock for yourself.
	sic.InitialiseStream(streamID)
	sic.mutex.Lock()
	defer sic.mutex.Unlock()

	for key, value := range headers {
		if isPseudo {
			sic.StreamInfo[streamID].GrpcReq.Headers.PseudoHeaders[key] = value
		} else {
			sic.StreamInfo[streamID].GrpcReq.Headers.OrdinaryHeaders[key] = value
		}
	}
}

func (sic *StreamInfoCollection) AddHeadersForResponse(streamID uint32, headers map[string]string, isPseudo, isTrailer bool) {
	// Initialise the stream before acquiring the lock for yourself.
	sic.InitialiseStream(streamID)
	sic.mutex.Lock()
	defer sic.mutex.Unlock()

	for key, value := range headers {
		if isTrailer {
			if isPseudo {
				sic.StreamInfo[streamID].GrpcResp.Trailers.PseudoHeaders[key] = value
			} else {
				sic.StreamInfo[streamID].GrpcResp.Trailers.OrdinaryHeaders[key] = value
			}
		} else {
			if isPseudo {
				sic.StreamInfo[streamID].GrpcResp.Headers.PseudoHeaders[key] = value
			} else {
				sic.StreamInfo[streamID].GrpcResp.Headers.OrdinaryHeaders[key] = value
			}
		}
	}
}

// AddPayloadForRequest adds the DATA frame to the stream.
// A data frame always appears after at least one header frame. Hence, we implicitly
// assume that the stream has been initialised.
func (sic *StreamInfoCollection) AddPayloadForRequest(streamID uint32, payload []byte) {
	sic.mutex.Lock()
	defer sic.mutex.Unlock()

	info := sic.StreamInfo[streamID]

	info.ReqRawData = append(info.ReqRawData, payload...)

	if !info.ReqPrefixParsed && len(info.ReqRawData) >= 5 {
		info.ReqExpectedLength = binary.BigEndian.Uint32(info.ReqRawData[1:5])
		info.ReqPrefixParsed = true
	}

	totalLen := 5 + int(info.ReqExpectedLength)
	if info.ReqPrefixParsed && len(info.ReqRawData) >= totalLen {
		info.GrpcReq.Body = pkg.CreateLengthPrefixedMessageFromPayload(info.ReqRawData[:totalLen])
	}

	sic.StreamInfo[streamID] = info
}

// AddPayloadForResponse adds the DATA frame to the stream.
// A data frame always appears after at least one header frame. Hence, we implicitly
// assume that the stream has been initialised.
func (sic *StreamInfoCollection) AddPayloadForResponse(streamID uint32, payload []byte) {
	sic.mutex.Lock()
	defer sic.mutex.Unlock()

	info := sic.StreamInfo[streamID]

	info.RespRawData = append(info.RespRawData, payload...)

	if !info.RespPrefixParsed && len(info.RespRawData) >= 5 {
		info.RespExpectedLength = binary.BigEndian.Uint32(info.RespRawData[1:5])
		info.RespPrefixParsed = true
	}

	totalLen := 5 + int(info.RespExpectedLength)
	if info.RespPrefixParsed && len(info.RespRawData) >= totalLen {
		info.GrpcResp.Body = pkg.CreateLengthPrefixedMessageFromPayload(info.RespRawData[:totalLen])
	}

	sic.StreamInfo[streamID] = info
}
func (sic *StreamInfoCollection) PersistMockForStream(ctx context.Context, streamID uint32, mocks chan<- *models.Mock) {
	sic.mutex.Lock()
	defer sic.mutex.Unlock()
	grpcReq := sic.StreamInfo[streamID].GrpcReq
	grpcResp := sic.StreamInfo[streamID].GrpcResp
	metadata := make(map[string]string)
	metadata["connID"] = ctx.Value(models.ClientConnectionIDKey).(string)
	// save the mock
	mocks <- &models.Mock{
		Version: models.GetVersion(),
		Name:    "mocks",
		Kind:    models.GRPC_EXPORT,
		Spec: models.MockSpec{
			Metadata:         metadata,
			GRPCReq:          &grpcReq,
			GRPCResp:         &grpcResp,
			ReqTimestampMock: sic.ReqTimestampMock,
			ResTimestampMock: sic.ResTimestampMock,
		},
	}
}

func (sic *StreamInfoCollection) FetchRequestForStream(streamID uint32) models.GrpcReq {
	sic.mutex.Lock()
	defer sic.mutex.Unlock()

	return sic.StreamInfo[streamID].GrpcReq
}

func (sic *StreamInfoCollection) ResetStream(streamID uint32) {
	sic.mutex.Lock()
	defer sic.mutex.Unlock()

	delete(sic.StreamInfo, streamID)
}
