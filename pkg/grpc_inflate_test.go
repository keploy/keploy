package pkg

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"golang.org/x/net/http2"
	"google.golang.org/protobuf/encoding/protowire"
)

// order is message Order { string id = 1; string status = 2; int32 qty = 3; }.
func order() []byte {
	var m []byte
	m = protowire.AppendTag(m, 1, protowire.BytesType)
	m = protowire.AppendString(m, "o-42")
	m = protowire.AppendTag(m, 2, protowire.BytesType)
	m = protowire.AppendString(m, "SHIPPED")
	m = protowire.AppendTag(m, 3, protowire.VarintType)
	return protowire.AppendVarint(m, 3)
}

const orderText = "1: {\"o-42\"}\n2: {\"SHIPPED\"}\n3: 3\n"

func compressed(t *testing.T, codec string, b []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	var w interface {
		Write([]byte) (int, error)
		Close() error
	}
	switch codec {
	case "gzip":
		w = gzip.NewWriter(&buf)
	case "zlib":
		w = zlib.NewWriter(&buf)
	case "flate":
		fw, err := flate.NewWriter(&buf, flate.DefaultCompression)
		require.NoError(t, err)
		w = fw
	}
	_, err := w.Write(b)
	require.NoError(t, err)
	require.NoError(t, w.Close())
	return buf.Bytes()
}

func framedMsg(flag byte, b []byte) []byte {
	out := binary.BigEndian.AppendUint32([]byte{flag}, uint32(len(b)))
	return append(out, b...)
}

// A compressed gRPC message is the app's message, inflated with its
// direction's grpc-encoding: gzip (also with no header, gRPC's mandated
// codec), deflate as a zlib or a raw deflate stream. An uncompressed one is
// as it is.
func TestInflateGrpcMessageReadsWhatTheAppSent(t *testing.T) {
	plain := order()
	for _, c := range []struct {
		name, encoding string
		flag           byte
		payload        []byte
	}{
		{"uncompressed", "gzip", 0, plain},
		{"gzip", "gzip", 1, compressed(t, "gzip", plain)},
		{"gzip, named in another case", " GZip ", 1, compressed(t, "gzip", plain)},
		{"no grpc-encoding: gzip", "", 1, compressed(t, "gzip", plain)},
		{"deflate as zlib", "deflate", 1, compressed(t, "zlib", plain)},
		{"deflate as a raw stream", "deflate", 1, compressed(t, "flate", plain)},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := InflateGrpcMessage(c.flag, c.payload, c.encoding, MaxGRPCFrameLength)
			require.NoError(t, err)
			assert.Equal(t, plain, got)
		})
	}
}

// A compressed message keploy cannot inflate is ErrUndecodableGrpcMessage:
// a codec it does not know, bytes that do not inflate, or more than limit.
func TestInflateGrpcMessageRefusesWhatItCannotRead(t *testing.T) {
	plain := order()
	for _, c := range []struct {
		name, encoding string
		payload        []byte
		limit          int64
	}{
		{"snappy", "snappy", compressed(t, "gzip", plain), MaxGRPCFrameLength},
		{"identity with the flag set", "identity", plain, MaxGRPCFrameLength},
		{"not gzip", "gzip", plain, MaxGRPCFrameLength},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := InflateGrpcMessage(1, c.payload, c.encoding, c.limit)
			assert.True(t, errors.Is(err, ErrUndecodableGrpcMessage), "err %v", err)
		})
	}
	_, err := InflateGrpcMessage(1, compressed(t, "gzip", plain), "gzip", int64(len(plain)-1))
	assert.True(t, errors.Is(err, ErrDecompressedTooLarge), "a message past the limit: %v", err)
	assert.False(t, errors.Is(err, ErrUndecodableGrpcMessage), "a message past the limit is no undecodable one")
	got, err := InflateGrpcMessage(1, compressed(t, "gzip", plain), "gzip", int64(len(plain)))
	require.NoError(t, err, "a message exactly at the limit")
	assert.Equal(t, plain, got)
}

// Each message of a direction is recorded as the app's, uncompressed: flag
// 0, its own length, its fields; the replay's gRPC client reads the app's
// answer so, and the matcher compares the two.
func TestSplitGrpcMessagesKeepsEachMessageUncompressed(t *testing.T) {
	plain := order()
	wire := append(framedMsg(1, compressed(t, "gzip", plain)), framedMsg(0, plain)...)
	msgs, err := SplitGrpcMessages(wire, "gzip", MaxGrpcInflateBytes)
	require.NoError(t, err)
	require.Len(t, msgs, 2)
	for i, m := range msgs {
		assert.Equal(t, uint(0), m.CompressionFlag, "message %d", i)
		assert.Equal(t, uint32(len(plain)), m.MessageLength, "message %d", i)
		assert.Equal(t, orderText, m.DecodedData, "message %d", i)
	}
	_, err = SplitGrpcMessages(append(framedMsg(0, plain), framedMsg(1, plain)...), "gzip", MaxGrpcInflateBytes)
	assert.True(t, errors.Is(err, ErrUndecodableGrpcMessage), "a message that does not inflate: %v", err)
}

// The first message of a call's side, as the proxy-mode ingress records it:
// inflated when compressed, and a compressed one cut short or not inflating
// is ErrUndecodableGrpcMessage.
func TestIngressMessageInflatesACompressedMessage(t *testing.T) {
	plain := order()
	m, err := ingressMessage(framedMsg(1, compressed(t, "gzip", plain)), "gzip")
	require.NoError(t, err)
	assert.Equal(t, uint(0), m.CompressionFlag)
	assert.Equal(t, orderText, m.DecodedData)
	m, err = ingressMessage(framedMsg(0, plain), "")
	require.NoError(t, err)
	assert.Equal(t, orderText, m.DecodedData)
	whole := framedMsg(1, compressed(t, "gzip", plain))
	_, err = ingressMessage(whole[:len(whole)-3], "gzip")
	assert.True(t, errors.Is(err, ErrUndecodableGrpcMessage), "cut short: %v", err)
}

// A direction's messages together inflate to at most the limit: many small
// compressed messages, each under it, do not add up past it (a stream of
// zip bombs), and a small compressed message does not inflate without bound.
func TestSplitGrpcMessagesBoundsADirectionAsAWhole(t *testing.T) {
	part := bytes.Repeat([]byte{0}, MaxGrpcInflateBytes/2+1)
	one := framedMsg(1, compressed(t, "gzip", part))
	msgs, err := SplitGrpcMessages(one, "gzip", MaxGrpcInflateBytes)
	require.NoError(t, err, "one message under the limit")
	require.Len(t, msgs, 1)
	_, err = SplitGrpcMessages(append(append([]byte(nil), one...), one...), "gzip", MaxGrpcInflateBytes)
	assert.True(t, errors.Is(err, ErrDecompressedTooLarge), "two that add up past it: %v", err)
	bomb := framedMsg(1, compressed(t, "gzip", bytes.Repeat([]byte{0}, 64<<20)))
	require.Less(t, len(bomb), 256<<10, "a small compressed message")
	_, err = ingressMessage(bomb, "gzip")
	assert.True(t, errors.Is(err, ErrDecompressedTooLarge), "a zip bomb: %v", err)
}

// A replay names the codecs its own client reads, not the recording
// client's: a server answers in a codec it supports of those the call names.
// Forwarded, the recording's deflate had the app answer in a codec replay
// cannot read, and the test failed on "Decompressor is not installed".
func TestAReplayReadsAnAnswerTheAppCompresses(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	plain := order()
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		codec := ""
		for _, c := range []string{"deflate", "gzip"} {
			if strings.Contains(strings.Join(r.Header.Values("Grpc-Accept-Encoding"), ","), c) {
				codec = c
				break
			}
		}
		w.Header().Set("Content-Type", "application/grpc")
		w.Header().Set("Trailer", "Grpc-Status")
		msg := framedMsg(0, plain)
		switch codec {
		case "gzip":
			w.Header().Set("Grpc-Encoding", "gzip")
			msg = framedMsg(1, compressed(t, "gzip", plain))
		case "deflate":
			w.Header().Set("Grpc-Encoding", "deflate")
			msg = framedMsg(1, compressed(t, "zlib", plain))
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(msg)
		w.Header().Set("Grpc-Status", "0")
	})
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go (&http2.Server{}).ServeConn(c, &http2.ServeConnOpts{Handler: handler})
		}
	}()
	tc := grpcTestCase(ln.Addr().String(), []models.GrpcLengthPrefixedMessage{{}})
	tc.GrpcReq.Headers.OrdinaryHeaders["grpc-accept-encoding"] = "deflate"
	resp, err := SimulateGRPC(context.Background(), tc, "set", zap.NewNop(), SimulationConfig{APITimeout: 5})
	require.NoError(t, err)
	assert.Equal(t, orderText, resp.Body.DecodedData)
}
