package grpcparser

import (
	"bytes"
	"fmt"

	"go.uber.org/zap"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"

	"go.keploy.io/server/pkg/hooks"
)

type transcoder struct {
	sic     *StreamInfoCollection
	hook    *hooks.Hook
	logger  *zap.Logger
	framer  *http2.Framer
	decoder *hpack.Decoder
}

func NewTranscoder(framer *http2.Framer, logger *zap.Logger, h *hooks.Hook) *transcoder {
	return &transcoder{
		logger:  logger,
		framer:  framer,
		hook:    h,
		sic:     NewStreamInfoCollection(h),
		decoder: NewDecoder(),
	}
}

func (srv *transcoder) WriteInitialSettingsFrame() error {
	var settings []http2.Setting
	// TODO : Get Settings from config file.
	settings = append(settings, http2.Setting{
		ID:  http2.SettingMaxFrameSize,
		Val: 16384,
	})
	return srv.framer.WriteSettings(settings...)
}

func (srv *transcoder) ProcessPingFrame(pingFrame *http2.PingFrame) error {
	if pingFrame.IsAck() {
		// An endpoint MUST NOT respond to PING frames containing this flag.
		return nil
	}

	if pingFrame.StreamID != 0 {
		// "PING frames are not associated with any individual
		// stream. If a PING frame is received with a stream
		// identifier field value other than 0x0, the recipient MUST
		// respond with a connection error (Section 5.4.1) of type
		// PROTOCOL_ERROR."
		srv.logger.Error("As per HTTP/2 spec, stream ID for PING frame should be zero.",
			zap.Any("stream_id", pingFrame.StreamID))
		return http2.ConnectionError(http2.ErrCodeProtocol)
	}

	// Write the ACK for the PING request.
	return srv.framer.WritePing(true, pingFrame.Data)

}

func (srv *transcoder) ProcessDataFrame(dataFrame *http2.DataFrame) error {
	id := dataFrame.Header().StreamID
	// DATA frame must be associated with a stream
	if id == 0 {
		srv.logger.Error("As per HTTP/2 spec, DATA frame must be associated with a stream.",
			zap.Any("stream_id", id))
		return http2.ConnectionError(http2.ErrCodeProtocol)
	}
	srv.sic.AddPayloadForRequest(id, dataFrame.Data())

	if dataFrame.StreamEnded() {
		defer srv.sic.ResetStream(dataFrame.StreamID)
	}

	grpcReq := srv.sic.FetchRequestForStream(id)

	// Fetch all the mocks. We can't assume that the grpc calls are made in a certain order.
	mock, err := FilterMocksBasedOnGrpcRequest(grpcReq, srv.hook)
	if err != nil {
		return fmt.Errorf("failed match mocks: %v", err)
	}
	if mock == nil {
		return fmt.Errorf("failed to mock the output for unrecorded outgoing grpc call")
	}

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
			srv.logger.Error("could not encode pseudo header", zap.Error(err),
				zap.Any("key", key), zap.Any("value", value))
			return err
		}
	}
	for key, value := range grpcMockResp.Headers.OrdinaryHeaders {
		err := encoder.WriteField(hpack.HeaderField{
			Name:  key,
			Value: value,
		})
		if err != nil {
			srv.logger.Error("could not encode ordinary header", zap.Error(err),
				zap.Any("key", key), zap.Any("value", value))
			return err
		}
	}

	// The headers are prepared. Write the frame.
	srv.logger.Info("Writing the first set of headers in a new HEADER frame.")
	err = srv.framer.WriteHeaders(http2.HeadersFrameParam{
		StreamID:      id,
		BlockFragment: buf.Bytes(),
		EndStream:     false,
		EndHeaders:    true,
	})
	if err != nil {
		srv.logger.Error("could not write the first set of headers onto client", zap.Error(err))
		return err
	}

	payload, err := CreatePayloadFromLengthPrefixedMessage(grpcMockResp.Body)
	if err != nil {
		srv.logger.Error("could not create grpc payload from mocks", zap.Error(err))
		return err
	}

	// Write the DATA frame with the payload.
	err = srv.framer.WriteData(id, false, payload)
	if err != nil {
		srv.logger.Error("could not write the data frame onto the client", zap.Error(err))
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
			srv.logger.Error("could not encode pseudo header", zap.Error(err),
				zap.Any("key", key), zap.Any("value", value))
			return err
		}
	}
	for key, value := range grpcMockResp.Trailers.OrdinaryHeaders {
		err := encoder.WriteField(hpack.HeaderField{
			Name:  key,
			Value: value,
		})
		if err != nil {
			srv.logger.Error("could not encode ordinary header", zap.Error(err),
				zap.Any("key", key), zap.Any("value", value))
			return err
		}
	}

	// The trailer is prepared. Write the frame.
	srv.logger.Info("Writing the trailers in a different HEADER frame")
	err = srv.framer.WriteHeaders(http2.HeadersFrameParam{
		StreamID:      id,
		BlockFragment: buf.Bytes(),
		EndStream:     true,
		EndHeaders:    true,
	})
	if err != nil {
		srv.logger.Error("could not write trailer on to the client", zap.Error(err))
		return err
	}

	return nil
}

func (srv *transcoder) ProcessWindowUpdateFrame(windowUpdateFrame *http2.WindowUpdateFrame) error {
	// Silently ignore Window update frames, as we already know the mock payloads that we would send.
	srv.logger.Info("Received Window Update Frame. Skipping it...")
	return nil
}

func (srv *transcoder) ProcessResetStreamFrame(resetStreamFrame *http2.RSTStreamFrame) error {
	srv.sic.ResetStream(resetStreamFrame.StreamID)
	return nil
}

func (srv *transcoder) ProcessSettingsFrame(settingsFrame *http2.SettingsFrame) error {
	// ACK the settings and silently skip the processing.
	// There is no actual server to tune the settings on. We already know the default settings from record mode.
	// TODO : Add support for dynamically updating the settings.
	if !settingsFrame.IsAck() {
		return srv.framer.WriteSettingsAck()
	}
	return nil
}

func (srv *transcoder) ProcessGoAwayFrame(goAwayFrame *http2.GoAwayFrame) error {
	// We do not support a client that requests a server to shut down during test mode. Warn the user.
	// TODO : Add support for dynamically shutting down mock server using a channel to send close request.
	srv.logger.Warn("Received GoAway Frame. Ideally, clients should not close server during test mode.")
	return nil
}

func (srv *transcoder) ProcessPriorityFrame(priorityFrame *http2.PriorityFrame) error {
	// We do not support reordering of frames based on priority, because we flush after each response.
	// Silently skip it.
	srv.logger.Info("Received PRIORITY frame, Skipping it...")
	return nil
}

func (srv *transcoder) ProcessHeadersFrame(headersFrame *http2.HeadersFrame) error {
	id := headersFrame.StreamID
	// Streams initiated by a client MUST use odd-numbered stream identifiers
	if id%2 != 1 {
		srv.logger.Error("As per HTTP/2 spec, stream_id must be odd for a client if conn init by client.",
			zap.Any("stream_id", id))
		return http2.ConnectionError(http2.ErrCodeProtocol)
	}

	pseudoHeaders, ordinaryHeaders, err := ExtractHeaders(headersFrame, srv.decoder)
	if err != nil {
		fmt.Errorf("could not extract headers from frame: %v", err)
	}

	srv.sic.AddHeadersForRequest(id, pseudoHeaders, true)
	srv.sic.AddHeadersForRequest(id, ordinaryHeaders, false)
	return nil
}

func (srv *transcoder) ProcessPushPromise(pushPromiseFrame *http2.PushPromiseFrame) error {
	// A client cannot push. Thus, servers MUST treat the receipt of a PUSH_PROMISE
	// frame as a connection error (Section 5.4.1) of type PROTOCOL_ERROR.
	srv.logger.Error("As per HTTP/2 spec, client cannot send PUSH_PROMISE.")
	return http2.ConnectionError(http2.ErrCodeProtocol)
}

func (srv *transcoder) ProcessContinuationFrame(ContinuationFrame *http2.ContinuationFrame) error {
	// Continuation frame support is overkill currently because the headers won't exceed the frame size
	// used by our mock server.
	// However, if we really need this feature, we can implement it later.
	srv.logger.Error("Continuation Frame received. This is unsupported currently")
	return fmt.Errorf("continuation frame is unsupported in the current implementation")
}

func (srv *transcoder) ProcessGenericFrame(frame http2.Frame) error {
	//PrintFrame(frame)
	var err error
	switch frame.(type) {
	case *http2.PingFrame:
		err = srv.ProcessPingFrame(frame.(*http2.PingFrame))
	case *http2.DataFrame:
		err = srv.ProcessDataFrame(frame.(*http2.DataFrame))
	case *http2.WindowUpdateFrame:
		err = srv.ProcessWindowUpdateFrame(frame.(*http2.WindowUpdateFrame))
	case *http2.RSTStreamFrame:
		err = srv.ProcessResetStreamFrame(frame.(*http2.RSTStreamFrame))
	case *http2.SettingsFrame:
		err = srv.ProcessSettingsFrame(frame.(*http2.SettingsFrame))
	case *http2.GoAwayFrame:
		err = srv.ProcessGoAwayFrame(frame.(*http2.GoAwayFrame))
	case *http2.PriorityFrame:
		err = srv.ProcessPriorityFrame(frame.(*http2.PriorityFrame))
	case *http2.HeadersFrame:
		err = srv.ProcessHeadersFrame(frame.(*http2.HeadersFrame))
	case *http2.PushPromiseFrame:
		err = srv.ProcessPushPromise(frame.(*http2.PushPromiseFrame))
	case *http2.ContinuationFrame:
		err = srv.ProcessContinuationFrame(frame.(*http2.ContinuationFrame))
	default:
		err = fmt.Errorf("unknown frame received from the client")
	}

	return err
}

// ListenAndServe is a forever blocking call that reads one frame at a time, and responds to them.
func (srv *transcoder) ListenAndServe() error {
	err := srv.WriteInitialSettingsFrame()
	if err != nil {
		srv.logger.Error("error writing initial settings frame", zap.Error(err))
		return err
	}
	for {
		frame, err := srv.framer.ReadFrame()
		if err != nil {
			srv.logger.Error("Failed to read frame", zap.Error(err))
			return err
		}
		err = srv.ProcessGenericFrame(frame)
		if err != nil {
			return err
		}
	}

}
