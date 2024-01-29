package grpcparser

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"go.uber.org/zap"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"

	"go.keploy.io/server/pkg"
	"go.keploy.io/server/pkg/hooks"
	"go.keploy.io/server/pkg/models"
	"go.keploy.io/server/utils"
)

type GrpcParser struct {
	logger *zap.Logger
	hooks  *hooks.Hook
}

func NewGrpcParser(logger *zap.Logger, h *hooks.Hook) *GrpcParser {
	return &GrpcParser{
		logger: logger,
		hooks:  h,
	}
}

// OutgoingType will be a method of GrpcParser.
func (g *GrpcParser) OutgoingType(buffer []byte) bool {
	return bytes.HasPrefix(buffer[:], []byte("PRI * HTTP/2"))
}

func (g *GrpcParser) ProcessOutgoing(requestBuffer []byte, clientConn, destConn net.Conn, ctx context.Context) {
	switch models.GetMode() {
	case models.MODE_RECORD:
		encodeOutgoingGRPC(requestBuffer, clientConn, destConn, g.hooks, g.logger, ctx)
	case models.MODE_TEST:
		decodeOutgoingGRPC(requestBuffer, clientConn, destConn, g.hooks, g.logger)
	default:
		g.logger.Fatal("Unsupported mode")
	}

}

func decodeOutgoingGRPC(requestBuffer []byte, clientConn, destConn net.Conn, h *hooks.Hook, logger *zap.Logger) {
	framer := http2.NewFramer(clientConn, clientConn)
	srv := NewTranscoder(framer, logger, h)
	err := srv.ListenAndServe()
	if err != nil {
		logger.Error("could not serve grpc request")
	}
}

func encodeOutgoingGRPC(requestBuffer []byte, clientConn, destConn net.Conn, h *hooks.Hook, logger *zap.Logger, ctx context.Context) {
	// Send the client preface to the server. This should be the first thing sent from the client.
	_, err := destConn.Write(requestBuffer)
	if err != nil {
		logger.Fatal("Could not write preface onto the server", zap.Error(err))
	}

	var wg sync.WaitGroup
	streamInfoCollection := NewStreamInfoCollection(h)
	isReqFromClient := true

	// Route requests from the client to the server.
	serverSideDecoder := NewDecoder()
	wg.Add(1)
	go func() {
		// Recover from panic and gracefully shutdown
		defer h.Recover(pkg.GenerateRandomID())
		defer utils.HandlePanic()
		defer wg.Done()
		err := TransferFrame(destConn, clientConn, streamInfoCollection, isReqFromClient, serverSideDecoder, ctx)
		if err != nil {
			// check for EOF error
			if err == io.EOF {
				logger.Debug("EOF error received from client. Closing connection")
				return
			}
			logger.Error("failed to transfer frame from client to server", zap.Error(err))
		}
	}()

	// Route response from the server to the client.
	clientSideDecoder := NewDecoder()
	wg.Add(1)
	go func() {
		// Recover from panic and gracefully shutdown
		defer h.Recover(pkg.GenerateRandomID())
		defer utils.HandlePanic()
		defer wg.Done()
		err := TransferFrame(clientConn, destConn, streamInfoCollection, !isReqFromClient, clientSideDecoder, ctx)
		if err != nil {
			logger.Error("failed to transfer frame from server to client", zap.Error(err))
		}
	}()

	// This would practically be an infinite loop, unless the client closes the grpc connection
	// during the runtime of the application.
	// A grpc server/client terminating after some time maybe intentional.
	wg.Wait()
}

// TransferFrame reads one frame from rhs and writes it to lhs.
func TransferFrame(lhs net.Conn, rhs net.Conn, sic *StreamInfoCollection, isReqFromClient bool, decoder *hpack.Decoder, ctx context.Context) error {
	isRespFromServer := !isReqFromClient
	framer := http2.NewFramer(lhs, rhs)
	for {
		frame, err := framer.ReadFrame()
		if err != nil {
			if err == io.EOF {
				return err
			}
			return fmt.Errorf("error reading frame %v", err)
		}
		//PrintFrame(frame)

		switch f := frame.(type) {
		case *http2.SettingsFrame:
			settingsFrame := f
			if settingsFrame.IsAck() {
				// Transfer Ack.
				if err := framer.WriteSettingsAck(); err != nil {
					return fmt.Errorf("could not write ack for settings frame: %v", err)
				}
			} else {
				var settingsCollection []http2.Setting
				err = settingsFrame.ForeachSetting(func(setting http2.Setting) error {
					settingsCollection = append(settingsCollection, setting)
					return nil
				})
				if err != nil {
					return fmt.Errorf("could not read settings from settings frame: %v", err)
				}

				if err := framer.WriteSettings(settingsCollection...); err != nil {
					return fmt.Errorf("could not write settings fraame: %v", err)
				}
			}
		case *http2.HeadersFrame:
			headersFrame := f
			streamID := headersFrame.StreamID
			err := framer.WriteHeaders(http2.HeadersFrameParam{
				StreamID:      streamID,
				BlockFragment: headersFrame.HeaderBlockFragment(),
				EndStream:     headersFrame.StreamEnded(),
				EndHeaders:    headersFrame.HeadersEnded(),
				PadLength:     0,
				Priority:      headersFrame.Priority,
			})
			if err != nil {
				return fmt.Errorf("could not write headers frame: %v", err)
			}
			pseudoHeaders, ordinaryHeaders, err := ExtractHeaders(headersFrame, decoder)
			if err != nil {
				return fmt.Errorf("could not extract headers from frame: %v", err)
			}

			if isReqFromClient {
				sic.AddHeadersForRequest(streamID, pseudoHeaders, true)
				sic.AddHeadersForRequest(streamID, ordinaryHeaders, false)
			} else if isRespFromServer {
				// If this is the last fragment of a stream from the server, it has to be a trailer.
				isTrailer := false
				if headersFrame.StreamEnded() {
					isTrailer = true
				}
				sic.AddHeadersForResponse(streamID, pseudoHeaders, true, isTrailer)
				sic.AddHeadersForResponse(streamID, ordinaryHeaders, false, isTrailer)
			}

			// The trailers frame has been received. The stream has been closed by the server.
			// Capture the mock and clear the map, as the stream ID can be reused by client.
			if isRespFromServer && headersFrame.StreamEnded() {
				sic.PersistMockForStream(streamID, ctx)
				sic.ResetStream(streamID)
			}

		case *http2.DataFrame:
			dataFrame := frame.(*http2.DataFrame)
			err := framer.WriteData(dataFrame.StreamID, dataFrame.StreamEnded(), dataFrame.Data())
			if err != nil {
				return fmt.Errorf("could not write data frame: %v", err)
			}
			if isReqFromClient {
				// Capturing the request timestamp
				sic.ReqTimestampMock = time.Now()

				sic.AddPayloadForRequest(dataFrame.StreamID, dataFrame.Data())
			} else if isRespFromServer {
				// Capturing the response timestamp
				sic.ResTimestampMock = time.Now()

				sic.AddPayloadForResponse(dataFrame.StreamID, dataFrame.Data())
			}
		case *http2.PingFrame:
			pingFrame := frame.(*http2.PingFrame)
			err := framer.WritePing(pingFrame.IsAck(), pingFrame.Data)
			if err != nil {
				return fmt.Errorf("could not write ACK for ping: %v", err)
			}
		case *http2.WindowUpdateFrame:
			windowUpdateFrame := frame.(*http2.WindowUpdateFrame)
			err := framer.WriteWindowUpdate(windowUpdateFrame.StreamID, windowUpdateFrame.Increment)
			if err != nil {
				return fmt.Errorf("could not write window update frame: %v", err)
			}
		case *http2.ContinuationFrame:
			continuationFrame := frame.(*http2.ContinuationFrame)
			err := framer.WriteContinuation(continuationFrame.StreamID, continuationFrame.HeadersEnded(),
				continuationFrame.HeaderBlockFragment())
			if err != nil {
				return fmt.Errorf("could not write continuation frame: %v", err)
			}
		case *http2.PriorityFrame:
			priorityFrame := frame.(*http2.PriorityFrame)
			err := framer.WritePriority(priorityFrame.StreamID, priorityFrame.PriorityParam)
			if err != nil {
				return fmt.Errorf("could not write priority frame: %v", err)
			}
		case *http2.RSTStreamFrame:
			rstStreamFrame := frame.(*http2.RSTStreamFrame)
			err := framer.WriteRSTStream(rstStreamFrame.StreamID, rstStreamFrame.ErrCode)
			if err != nil {
				return fmt.Errorf("could not write reset stream frame: %v", err)
			}
		case *http2.GoAwayFrame:
			goAwayFrame := frame.(*http2.GoAwayFrame)
			err := framer.WriteGoAway(goAwayFrame.StreamID, goAwayFrame.ErrCode, goAwayFrame.DebugData())
			if err != nil {
				return fmt.Errorf("could not write GoAway frame: %v", err)
			}
		case *http2.PushPromiseFrame:
			pushPromiseFrame := frame.(*http2.PushPromiseFrame)
			err := framer.WritePushPromise(http2.PushPromiseParam{
				StreamID:      pushPromiseFrame.StreamID,
				PromiseID:     pushPromiseFrame.PromiseID,
				BlockFragment: pushPromiseFrame.HeaderBlockFragment(),
				EndHeaders:    pushPromiseFrame.HeadersEnded(),
				PadLength:     0,
			})
			if err != nil {
				return fmt.Errorf("could not write PushPromise frame: %v", err)
			}
		}
	}
}
