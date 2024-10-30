//go:build linux || windows

package grpc

import (
	"context"
	"encoding/binary"
	"fmt"
	"sync"
	"time"

	"github.com/protocolbuffers/protoscope"

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
	info.GrpcReq.Body = createLengthPrefixedMessageFromPayload(payload)
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
	info.GrpcResp.Body = createLengthPrefixedMessageFromPayload(payload)
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

func createLengthPrefixedMessageFromPayload(data []byte) models.GrpcLengthPrefixedMessage {
	msg := models.GrpcLengthPrefixedMessage{}

	// If the body is not length prefixed, we return the default value.
	if len(data) < 5 {
		return msg
	}

	// The first byte is the compression flag.
	msg.CompressionFlag = uint(data[0])

	// The next 4 bytes are message length.
	msg.MessageLength = binary.BigEndian.Uint32(data[1:5])

	// The payload could be empty. We only parse it if it is present.
	if len(data) >= 5 {
		// Use protoscope to decode the message.
		msg.DecodedData = protoscope.Write(data[5:], protoscope.WriterOptions{})
	}

	return msg
}

func createPayloadFromLengthPrefixedMessage(msg models.GrpcLengthPrefixedMessage) ([]byte, error) {
	scanner := protoscope.NewScanner(msg.DecodedData)
	encodedData, err := scanner.Exec()
	if err != nil {
		return nil, fmt.Errorf("could not encode grpc msg using protoscope: %v", err)
	}

	// Note that the encoded length is present in the msg, but it is also equal to the len of encodedData.
	// We should give the preference to the length of encodedData, since the mocks might have been altered.

	// Reserve 1 byte for compression flag, 4 bytes for length capture.
	payload := make([]byte, 1+4)
	payload[0] = uint8(msg.CompressionFlag)
	binary.BigEndian.PutUint32(payload[1:5], uint32(len(encodedData)))
	payload = append(payload, encodedData...)

	return payload, nil
}
