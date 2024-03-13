package grpcparser

import "golang.org/x/net/http2/hpack"

func NewDecoder() *hpack.Decoder {
	return hpack.NewDecoder(KmaxDynamicTableSize, nil)
}
