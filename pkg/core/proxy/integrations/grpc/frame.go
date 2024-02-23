package grpc

import (
	"context"
	"fmt"
	"go.keploy.io/server/v2/pkg/models"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
	"io"
	"net"
	"time"
)

// transferFrame reads one frame from rhs and writes it to lhs.
func transferFrame(ctx context.Context, lhs net.Conn, rhs net.Conn, sic *StreamInfoCollection, reqFromClient bool, decoder *hpack.Decoder, mocks chan<- *models.Mock) error {
	respFromServer := !reqFromClient
	framer := http2.NewFramer(lhs, rhs)
	for {
		select {
		case <-ctx.Done():
			return nil
		default:
			frame, err := framer.ReadFrame()
			if err != nil {
				if err == io.EOF {
					return err
				}
				return fmt.Errorf("error reading frame %v", err)
			}
			//PrintFrame(frame)

			switch frame.(type) {
			case *http2.SettingsFrame:
				settingsFrame := frame.(*http2.SettingsFrame)
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
				headersFrame := frame.(*http2.HeadersFrame)
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
				pseudoHeaders, ordinaryHeaders, err := extractHeaders(headersFrame, decoder)
				if err != nil {
					return fmt.Errorf("could not extract headers from frame: %v", err)
				}

				if reqFromClient {
					sic.AddHeadersForRequest(streamID, pseudoHeaders, true)
					sic.AddHeadersForRequest(streamID, ordinaryHeaders, false)
				} else if respFromServer {
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
				if respFromServer && headersFrame.StreamEnded() {
					sic.PersistMockForStream(ctx, streamID, mocks)
					sic.ResetStream(streamID)
				}

			case *http2.DataFrame:
				dataFrame := frame.(*http2.DataFrame)
				err := framer.WriteData(dataFrame.StreamID, dataFrame.StreamEnded(), dataFrame.Data())
				if err != nil {
					return fmt.Errorf("could not write data frame: %v", err)
				}
				if reqFromClient {
					// Capturing the request timestamp
					sic.ReqTimestampMock = time.Now()

					sic.AddPayloadForRequest(dataFrame.StreamID, dataFrame.Data())
				} else if respFromServer {
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
					return fmt.Errorf("could not write window tools frame: %v", err)
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
}

const (
	KmaxDynamicTableSize = 2048
)

func printDataFrame(frame *http2.DataFrame) {
	fmt.Println("This is a DATA frame")
	fmt.Println("length of the data is :", len(frame.Data()))
	fmt.Printf("Contents: %s\n\n", frame.Data())
}

func printHeadersFrame(frame *http2.HeadersFrame) {
	fmt.Println("This is a HEADERS frame")
	decoder := hpack.NewDecoder(2048, nil)
	hf, _ := decoder.DecodeFull(frame.HeaderBlockFragment())
	for _, h := range hf {
		fmt.Printf("%s\n", h.Name+":"+h.Value)
	}
}

func printSettingsFrame(frame *http2.SettingsFrame) {
	fmt.Println("This is a SETTINGS frame")
	fmt.Printf("Is ACK? %v\n", frame.IsAck())

	printSettings := func(s http2.Setting) error {
		fmt.Printf("%s\n", s.String())
		return nil
	}
	if err := frame.ForeachSetting(printSettings); err != nil {
		fmt.Printf("Error while printing settings: %v\n", err)
	}
	fmt.Println()
}

func printWindowUpdateFrame(frame *http2.WindowUpdateFrame) {
	fmt.Println("This is a WINDOW_UPDATE frame")
	fmt.Printf("Window size increment present in frame is %d\n\n", frame.Increment)
}

func printPingFrame(frame *http2.PingFrame) {
	fmt.Printf("This is a PING frame\n\n")
}

func printFrame(frame http2.Frame) {
	fmt.Printf("fh type: %s\n", frame.Header().Type)
	fmt.Printf("fh flag: %d\n", frame.Header().Flags)
	fmt.Printf("fh length: %d\n", frame.Header().Length)
	fmt.Printf("fh streamid: %d\n", frame.Header().StreamID)

	switch frame.(type) {
	case *http2.DataFrame:
		printDataFrame(frame.(*http2.DataFrame))
	case *http2.HeadersFrame:
		printHeadersFrame(frame.(*http2.HeadersFrame))
	case *http2.SettingsFrame:
		printSettingsFrame(frame.(*http2.SettingsFrame))
	case *http2.WindowUpdateFrame:
		printWindowUpdateFrame(frame.(*http2.WindowUpdateFrame))
	case *http2.PingFrame:
		printPingFrame(frame.(*http2.PingFrame))
	}
}

func extractHeaders(frame *http2.HeadersFrame, decoder *hpack.Decoder) (pseudoHeaders, ordinaryHeaders map[string]string, err error) {
	hf, err := decoder.DecodeFull(frame.HeaderBlockFragment())
	if err != nil {
		return nil, nil, fmt.Errorf("could not decode headers: %v", err)
	}

	pseudoHeaders = make(map[string]string)
	ordinaryHeaders = make(map[string]string)

	for _, header := range hf {
		if header.IsPseudo() {
			pseudoHeaders[header.Name] = header.Value
		} else {
			ordinaryHeaders[header.Name] = header.Value
		}
	}

	return pseudoHeaders, ordinaryHeaders, nil
}
