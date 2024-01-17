package grpcparser

import (
	"fmt"
	"log"

	"go.uber.org/zap"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
)

const (
	KmaxDynamicTableSize = 2048
)

func PrintDataFrame(logger zap.Logger, frame *http2.DataFrame) {
	logger.Debug(fmt.Sprintf("This is a DATA frame"))
	logger.Debug(fmt.Sprintf("length of the data is :", len(frame.Data())))
	logger.Debug(fmt.Sprintf("Contents: %s\n\n", frame.Data()))
}

func PrintHeadersFrame(logger zap.Logger, frame *http2.HeadersFrame) {
	logger.Debug("This is a HEADERS frame")
	decoder := hpack.NewDecoder(2048, nil)
	hf, _ := decoder.DecodeFull(frame.HeaderBlockFragment())
	for _, h := range hf {
		logger.Debug(fmt.Sprint("%s\n", h.Name+":"+h.Value))
	}
}

func PrintSettingsFrame(logger zap.Logger, frame *http2.SettingsFrame) {
	logger.Debug("This is a SETTINGS frame")
	logger.Debug(fmt.Sprintf("Is ACK? %v\n", frame.IsAck()))

	printSettings := func(s http2.Setting) error {
		logger.Debug(fmt.Sprintf("%s\n", s.String()))
		return nil
	}
	if err := frame.ForeachSetting(printSettings); err != nil {
		log.Fatal(err)
	}

}

func PrintWindowUpdateFrame(logger zap.Logger, frame *http2.WindowUpdateFrame) {
	logger.Debug("This is a WINDOW_UPDATE frame")
	logger.Debug(fmt.Sprintf("Window size increment present in frame is %d\n\n", frame.Increment))
}

func PrintPingFrame(logger zap.Logger, frame *http2.PingFrame) {
	logger.Debug("This is a PING frame\n\n")
}

func PrintFrame(logger zap.Logger, frame http2.Frame) {
	logger.Debug(fmt.Sprintf("fh type: %s\n", frame.Header().Type))
	logger.Debug(fmt.Sprintf("fh flag: %d\n", frame.Header().Flags))
	logger.Debug(fmt.Sprintf("fh length: %d\n", frame.Header().Length))
	logger.Debug(fmt.Sprintf("fh streamid: %d\n", frame.Header().StreamID))

	switch frame := frame.(type) {
	case *http2.DataFrame:
		PrintDataFrame(logger, frame)
	case *http2.HeadersFrame:
		PrintHeadersFrame(logger, frame)
	case *http2.SettingsFrame:
		PrintSettingsFrame(logger, frame)
	case *http2.WindowUpdateFrame:
		PrintWindowUpdateFrame(logger, frame)
	case *http2.PingFrame:
		PrintPingFrame(logger, frame)
	}
}

func ExtractHeaders(frame *http2.HeadersFrame, decoder *hpack.Decoder) (pseudoHeaders, ordinaryHeaders map[string]string, err error) {
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
