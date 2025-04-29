//go:build linux

package grpc

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"

	"go.keploy.io/server/v2/pkg"
	"go.keploy.io/server/v2/pkg/core/proxy/integrations"
	"go.keploy.io/server/v2/utils"

	"go.uber.org/zap"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
)

type Transcoder struct {
	sic     *StreamInfoCollection
	mockDb  integrations.MockMemDb
	logger  *zap.Logger
	framer  *http2.Framer
	decoder *hpack.Decoder
}

func NewTranscoder(logger *zap.Logger, framer *http2.Framer, mockDb integrations.MockMemDb) *Transcoder {
	return &Transcoder{
		logger:  logger,
		framer:  framer,
		mockDb:  mockDb,
		sic:     NewStreamInfoCollection(),
		decoder: NewDecoder(),
	}
}

func (srv *Transcoder) WriteInitialSettingsFrame() error {
	var settings []http2.Setting
	// TODO : Get Settings from config file.
	settings = append(settings, http2.Setting{
		ID:  http2.SettingMaxFrameSize,
		Val: 16384,
	})
	return srv.framer.WriteSettings(settings...)
}

func (srv *Transcoder) ProcessPingFrame(pingFrame *http2.PingFrame) error {
	if pingFrame.IsAck() {
		// An endpoint MUST NOT respond to PING frames containing this flag.
		return nil
	}

	if pingFrame.StreamID != 0 {
		// "PING frames are not associated with any individual
		// stream. If a PING frame is received with a stream
		// identifier field value other than 0x0, the recipient MUST
		// respond with a conn error (Section 5.4.1) of type
		// PROTOCOL_ERROR."
		utils.LogError(srv.logger, nil, "As per HTTP/2 spec, stream ID for PING frame should be zero.", zap.Any("stream_id", pingFrame.StreamID))
		return http2.ConnectionError(http2.ErrCodeProtocol)
	}

	// Write the ACK for the PING request.
	return srv.framer.WritePing(true, pingFrame.Data)

}

func (srv *Transcoder) ProcessDataFrame(ctx context.Context, dataFrame *http2.DataFrame) error {
	id := dataFrame.Header().StreamID
	// DATA frame must be associated with a stream
	if id == 0 {
		utils.LogError(srv.logger, nil, "As per HTTP/2 spec, DATA frame must be associated with a stream.", zap.Any("stream_id", id))
		return http2.ConnectionError(http2.ErrCodeProtocol)
	}
	srv.sic.AddPayloadForRequest(id, dataFrame.Data())

	if dataFrame.StreamEnded() {
		defer srv.sic.ResetStream(dataFrame.StreamID)
	}

	grpcReq := srv.sic.FetchRequestForStream(id)

	srv.logger.Debug("Getting mock for request from the mock database", zap.Any("request", grpcReq))

	// Fetch all the mocks. We can't assume that the grpc calls are made in a certain order.
	mock, err := FilterMocksBasedOnGrpcRequest(ctx, srv.logger, grpcReq, srv.mockDb)
	if err != nil {
		return fmt.Errorf("failed match mocks: %v", err)
	}
	if mock == nil {
		return fmt.Errorf("failed to mock the output for unrecorded outgoing grpc call")
	}

	srv.logger.Debug("Found a mock for the request", zap.Any("mock", mock))

	grpcMockResp := mock.Spec.GRPCResp

	// First, send the headers frame.
	buf := new(bytes.Buffer)
	encoder := hpack.NewEncoder(buf)

	// The pseudo headers should be written before ordinary ones.
	for key, value := range grpcMockResp.Headers.PseudoHeaders {
		err := encoder.WriteField(hpack.HeaderField{
			Name:  key,
			Value: value,
		})
		if err != nil {
			utils.LogError(srv.logger, err, "could not encode pseudo header", zap.Any("key", key), zap.Any("value", value))
			return err
		}
	}
	for key, value := range grpcMockResp.Headers.OrdinaryHeaders {
		err := encoder.WriteField(hpack.HeaderField{
			Name:  key,
			Value: value,
		})
		if err != nil {
			utils.LogError(srv.logger, err, "could not encode ordinary header", zap.Any("key", key), zap.Any("value", value))
			return err
		}
	}

	// The headers are prepared. Write the frame.
	srv.logger.Debug("Writing the first set of headers in a new HEADER frame.")
	err = srv.framer.WriteHeaders(http2.HeadersFrameParam{
		StreamID:      id,
		BlockFragment: buf.Bytes(),
		EndStream:     false,
		EndHeaders:    true,
	})
	if err != nil {
		utils.LogError(srv.logger, err, "could not write the first set of headers onto client")
		return err
	}

	payload, err := pkg.CreatePayloadFromLengthPrefixedMessage(grpcMockResp.Body)
	if err != nil {
		utils.LogError(srv.logger, err, "could not create grpc payload from mocks")
		return err
	}

	// Write the DATA frame with the payload.
	err = srv.framer.WriteData(id, false, payload)
	if err != nil {
		utils.LogError(srv.logger, err, "could not write the data frame onto the client")
		return err
	}

	// Reset the buffer and start with a new encoding.
	buf = new(bytes.Buffer)
	encoder = hpack.NewEncoder(buf)

	//Prepare the trailers.
	//The pseudo headers should be written before ordinary ones.
	for key, value := range grpcMockResp.Trailers.PseudoHeaders {
		err := encoder.WriteField(hpack.HeaderField{
			Name:  key,
			Value: value,
		})
		if err != nil {
			utils.LogError(srv.logger, err, "could not encode pseudo header", zap.Any("key", key), zap.Any("value", value))
			return err
		}
	}
	for key, value := range grpcMockResp.Trailers.OrdinaryHeaders {
		err := encoder.WriteField(hpack.HeaderField{
			Name:  key,
			Value: value,
		})
		if err != nil {
			utils.LogError(srv.logger, err, "could not encode ordinary header", zap.Any("key", key), zap.Any("value", value))
			return err
		}
	}

	// The trailer is prepared. Write the frame.
	srv.logger.Debug("Writing the trailers in a different HEADER frame")
	err = srv.framer.WriteHeaders(http2.HeadersFrameParam{
		StreamID:      id,
		BlockFragment: buf.Bytes(),
		EndStream:     true,
		EndHeaders:    true,
	})
	if err != nil {
		utils.LogError(srv.logger, err, "could not write the trailers onto client")
		return err
	}

	return nil
}

func (srv *Transcoder) ProcessWindowUpdateFrame(_ *http2.WindowUpdateFrame) error {
	// Silently ignore Window tools frames, as we already know the mock payloads that we would send.
	srv.logger.Debug("Received Window Update Frame. Skipping it...")
	return nil
}

func (srv *Transcoder) ProcessResetStreamFrame(resetStreamFrame *http2.RSTStreamFrame) error {
	srv.sic.ResetStream(resetStreamFrame.StreamID)
	return nil
}

func (srv *Transcoder) ProcessSettingsFrame(settingsFrame *http2.SettingsFrame) error {
	// ACK the settings and silently skip the processing.
	// There is no actual server to tune the settings on. We already know the default settings from record mode.
	// TODO : Add support for dynamically updating the settings.
	if !settingsFrame.IsAck() {
		return srv.framer.WriteSettingsAck()
	}
	return nil
}

func (srv *Transcoder) ProcessGoAwayFrame(_ *http2.GoAwayFrame) error {
	// We do not support a client that requests a server to shut down during test mode. Warn the user.
	// TODO : Add support for dynamically shutting down mock server using a channel to send close request.
	srv.logger.Debug("Received GoAway Frame. Ideally, clients should not close server during test mode.")
	return nil
}

func (srv *Transcoder) ProcessPriorityFrame(_ *http2.PriorityFrame) error {
	// We do not support reordering of frames based on priority, because we flush after each response.
	// Silently skip it.
	srv.logger.Info("Received PRIORITY frame, Skipping it...")
	return nil
}

func (srv *Transcoder) ProcessHeadersFrame(headersFrame *http2.HeadersFrame) error {
	id := headersFrame.StreamID
	// Streams initiated by a client MUST use odd-numbered stream identifiers
	if id%2 != 1 {
		utils.LogError(srv.logger, nil, "As per HTTP/2 spec, stream_id must be odd for a client if conn init by client.", zap.Any("stream_id", id))
		return http2.ConnectionError(http2.ErrCodeProtocol)
	}

	pseudoHeaders, ordinaryHeaders, err := extractHeaders(headersFrame, srv.decoder)
	if err != nil {
		utils.LogError(srv.logger, err, "could not extract headers from frame")
	}

	srv.sic.AddHeadersForRequest(id, pseudoHeaders, true)
	srv.sic.AddHeadersForRequest(id, ordinaryHeaders, false)
	return nil
}

func (srv *Transcoder) ProcessPushPromise(_ *http2.PushPromiseFrame) error {
	// A client cannot push. Thus, servers MUST treat the receipt of a PUSH_PROMISE
	// frame as a conn error (Section 5.4.1) of type PROTOCOL_ERROR.
	utils.LogError(srv.logger, nil, "As per HTTP/2 spec, client cannot send PUSH_PROMISE.")
	return http2.ConnectionError(http2.ErrCodeProtocol)
}

func (srv *Transcoder) ProcessContinuationFrame(_ *http2.ContinuationFrame) error {
	// Continuation frame support is overkill currently because the headers won't exceed the frame size
	// used by our mock server.
	// However, if we really need this feature, we can implement it later.
	utils.LogError(srv.logger, nil, "Continuation Frame received. This is unsupported currently")
	return fmt.Errorf("continuation frame is unsupported in the current implementation")
}

func (srv *Transcoder) ProcessGenericFrame(ctx context.Context, frame http2.Frame) error {
	var err error
	switch frame := frame.(type) {
	case *http2.PingFrame:
		err = srv.ProcessPingFrame(frame)
	case *http2.DataFrame:
		err = srv.ProcessDataFrame(ctx, frame)
	case *http2.WindowUpdateFrame:
		err = srv.ProcessWindowUpdateFrame(frame)
	case *http2.RSTStreamFrame:
		err = srv.ProcessResetStreamFrame(frame)
	case *http2.SettingsFrame:
		err = srv.ProcessSettingsFrame(frame)
	case *http2.GoAwayFrame:
		err = srv.ProcessGoAwayFrame(frame)
	case *http2.PriorityFrame:
		err = srv.ProcessPriorityFrame(frame)
	case *http2.HeadersFrame:
		err = srv.ProcessHeadersFrame(frame)
	case *http2.PushPromiseFrame:
		err = srv.ProcessPushPromise(frame)
	case *http2.ContinuationFrame:
		err = srv.ProcessContinuationFrame(frame)
	default:
		err = fmt.Errorf("unknown frame received from the client")
	}

	return err
}

// ListenAndServe is a forever blocking call that reads one frame at a time, and responds to them.
func (srv *Transcoder) ListenAndServe(ctx context.Context) error {
	err := srv.WriteInitialSettingsFrame()
	if err != nil {
		utils.LogError(srv.logger, err, "could not write initial settings frame")
		return err
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			frame, err := srv.framer.ReadFrame()
			if err != nil && err != io.EOF && err.Error() != "http2: connection is closed" && !strings.Contains(err.Error(), "read: connection reset by peer") {
				utils.LogError(srv.logger, err, "Failed to read frame")
				return err
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			err = srv.ProcessGenericFrame(ctx, frame)
			if err != nil {
				return err
			}
		}
	}
}
