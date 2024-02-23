package grpc

import "golang.org/x/net/http2/hpack"

// Header Decoder
func NewDecoder() *hpack.Decoder {
	return hpack.NewDecoder(KmaxDynamicTableSize, nil)
}
