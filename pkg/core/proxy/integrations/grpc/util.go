//go:build linux 

package grpc

import "golang.org/x/net/http2/hpack"

// NewDecoder returns a header decoder.
func NewDecoder() *hpack.Decoder {
	return hpack.NewDecoder(KmaxDynamicTableSize, nil)
}
