//go:build linux

package grpc

import (
	"context"
	"sync"
	"time"

	"go.keploy.io/server/v2/pkg"
	"go.keploy.io/server/v2/pkg/models"
)

// StreamInfoCollection is a thread-safe data structure to store all communications
// that happen in a stream for grpc. This includes the headers and data frame for the
// request and response.
type StreamInfoCollection struct {
	mutex            sync.Mutex
	StreamInfo       map[uint32]models.GrpcStream
	ReqTimestampMock time.Time
	ResTimestampMock time.Time
}

func NewStreamInfoCollection() *StreamInfoCollection {
	return &StreamInfoCollection{
		StreamInfo: make(map[uint32]models.GrpcStream),
	}
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

	// We cannot modify non pointer values in nested entries in map.
	// Create a copy and overwrite it.
	info := sic.StreamInfo[streamID]
	info.GrpcReq.Body = pkg.CreateLengthPrefixedMessageFromPayload(payload)
	sic.StreamInfo[streamID] = info
}

// AddPayloadForResponse adds the DATA frame to the stream.
// A data frame always appears after at least one header frame. Hence, we implicitly
// assume that the stream has been initialised.
func (sic *StreamInfoCollection) AddPayloadForResponse(streamID uint32, payload []byte) {
	sic.mutex.Lock()
	defer sic.mutex.Unlock()

	// We cannot modify non pointer values in nested entries in map.
	// Create a copy and overwrite it.
	info := sic.StreamInfo[streamID]
	info.GrpcResp.Body = pkg.CreateLengthPrefixedMessageFromPayload(payload)
	sic.StreamInfo[streamID] = info
}

func (sic *StreamInfoCollection) PersistMockForStream(_ context.Context, streamID uint32, mocks chan<- *models.Mock) {
	sic.mutex.Lock()
	defer sic.mutex.Unlock()
	grpcReq := sic.StreamInfo[streamID].GrpcReq
	grpcResp := sic.StreamInfo[streamID].GrpcResp
	// save the mock
	mocks <- &models.Mock{
		Version: models.GetVersion(),
		Name:    "mocks",
		Kind:    models.GRPC_EXPORT,
		Spec: models.MockSpec{
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
