package grpcparser

import (
	"fmt"
	"log"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
)

const (
	KmaxDynamicTableSize = 2048
)

func PrintDataFrame(frame *http2.DataFrame) {
	fmt.Println("This is a DATA frame")
	fmt.Println("length of the data is :", len(frame.Data()))
	fmt.Printf("Contents: %s\n\n", frame.Data())
}

func PrintHeadersFrame(frame *http2.HeadersFrame) {
	fmt.Println("This is a HEADERS frame")
	decoder := hpack.NewDecoder(2048, nil)
	hf, _ := decoder.DecodeFull(frame.HeaderBlockFragment())
	for _, h := range hf {
		fmt.Printf("%s\n", h.Name+":"+h.Value)
	}
}

func PrintSettingsFrame(frame *http2.SettingsFrame) {
	fmt.Println("This is a SETTINGS frame")
	fmt.Printf("Is ACK? %v\n", frame.IsAck())

	printSettings := func(s http2.Setting) error {
		fmt.Printf("%s\n", s.String())
		return nil
	}
	if err := frame.ForeachSetting(printSettings); err != nil {
		log.Fatal(err)
	}
	fmt.Println()
}

func PrintWindowUpdateFrame(frame *http2.WindowUpdateFrame) {
	fmt.Println("This is a WINDOW_UPDATE frame")
	fmt.Printf("Window size increment present in frame is %d\n\n", frame.Increment)
}

func PrintPingFrame(frame *http2.PingFrame) {
	fmt.Printf("This is a PING frame\n\n")
}

func PrintFrame(frame http2.Frame) {
	fmt.Printf("fh type: %s\n", frame.Header().Type)
	fmt.Printf("fh flag: %d\n", frame.Header().Flags)
	fmt.Printf("fh length: %d\n", frame.Header().Length)
	fmt.Printf("fh streamid: %d\n", frame.Header().StreamID)

	switch frame.(type) {
	case *http2.DataFrame:
		PrintDataFrame(frame.(*http2.DataFrame))
	case *http2.HeadersFrame:
		PrintHeadersFrame(frame.(*http2.HeadersFrame))
	case *http2.SettingsFrame:
		PrintSettingsFrame(frame.(*http2.SettingsFrame))
	case *http2.WindowUpdateFrame:
		PrintWindowUpdateFrame(frame.(*http2.WindowUpdateFrame))
	case *http2.PingFrame:
		PrintPingFrame(frame.(*http2.PingFrame))
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
