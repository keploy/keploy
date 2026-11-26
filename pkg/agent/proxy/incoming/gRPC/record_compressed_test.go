package proxy

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"encoding/binary"
	"io"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
	"google.golang.org/protobuf/encoding/protowire"
)

// orderMsg is message Order { string id = 1; string status = 2; }.
func orderMsg(status string) []byte {
	var m []byte
	m = protowire.AppendTag(m, 1, protowire.BytesType)
	m = protowire.AppendString(m, "o-42")
	m = protowire.AppendTag(m, 2, protowire.BytesType)
	return protowire.AppendString(m, status)
}

func squeezed(t *testing.T, codec string, b []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	var w io.WriteCloser = gzip.NewWriter(&buf)
	if codec == "deflate" {
		w = zlib.NewWriter(&buf)
	}
	_, err := w.Write(b)
	require.NoError(t, err)
	require.NoError(t, w.Close())
	return buf.Bytes()
}

func grpcFrame(flag byte, b []byte) []byte {
	return append(binary.BigEndian.AppendUint32([]byte{flag}, uint32(len(b))), b...)
}

// compressedCall sends a unary gRPC call whose request and answer are each
// one message compressed with codec (grpc-encoding), through g, as a gRPC
// client and server send one with grpc.UseCompressor.
func compressedCall(t *testing.T, g *grpcIngress, codec string, compress func([]byte) []byte) {
	t.Helper()
	sidedCall(t, g, codec, compress, codec, compress, orderMsg("SHIPPED"))
}

// sidedCall is compressedCall with each side's codec its own: the client's
// grpc.UseCompressor names the request's, the server's the answer's.
func sidedCall(t *testing.T, g *grpcIngress, reqCodec string, reqCompress func([]byte) []byte, respCodec string, respCompress func([]byte) []byte, answer []byte) {
	t.Helper()
	handler := func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/grpc")
		w.Header().Set("Grpc-Encoding", respCodec)
		w.Header().Set("Trailer", "Grpc-Status")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(grpcFrame(1, respCompress(answer)))
		w.Header().Set("Grpc-Status", "0")
	}
	req, err := http.NewRequest(http.MethodPost, "http://orders:50051/shop.Orders/Get", bytes.NewReader(grpcFrame(1, reqCompress(orderMsg("PENDING")))))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/grpc")
	req.Header.Set("Grpc-Encoding", reqCodec)
	req.Header.Set("Te", "trailers")
	_ = h2cThrough(t, g, handler, req)
}

// A gRPC call whose messages are compressed (grpc-encoding gzip or deflate)
// is recorded with the app's messages, inflated and kept uncompressed: its
// fields, not a dump of compressed bytes, as the egress recorder keeps a
// mock's, and as replay's gRPC client reads the app's answer.
func TestRecordIncoming_ACompressedGRPCCallIsRecordedWithItsMessages(t *testing.T) {
	for _, codec := range []string{"gzip", "deflate"} {
		t.Run(codec, func(t *testing.T) {
			g := startGRPCIngress(t)
			compressedCall(t, g, codec, func(b []byte) []byte { return squeezed(t, codec, b) })
			tc := g.recordedOne(t)
			require.Equal(t, models.GRPC_EXPORT, tc.Kind)
			require.Equal(t, "1: {\"o-42\"}\n2: {\"PENDING\"}\n", tc.GrpcReq.Body.DecodedData)
			require.Equal(t, "1: {\"o-42\"}\n2: {\"SHIPPED\"}\n", tc.GrpcResp.Body.DecodedData)
			require.Zero(t, tc.GrpcResp.Body.CompressionFlag)
			require.Equal(t, uint32(len(orderMsg("SHIPPED"))), tc.GrpcResp.Body.MessageLength)
		})
	}
}

// A call compressed with a codec keploy cannot inflate (snappy) is no test
// case, as the egress recorder records no such mock: recorded, its body was
// bytes no app reads as its message, sent uncompressed at replay.
func TestRecordIncoming_AGRPCCallKeployCannotInflateIsNotRecorded(t *testing.T) {
	g := startGRPCIngress(t)
	compressedCall(t, g, "snappy", func(b []byte) []byte { return append([]byte{0xff, 0x06}, b...) })
	g.noneRecorded(t)
}

// Each side is inflated with its own grpc-encoding: a client compressing
// with deflate and a server answering with gzip are both read.
func TestRecordIncoming_EachSideIsInflatedWithItsOwnCodec(t *testing.T) {
	g := startGRPCIngress(t)
	sidedCall(t, g, "deflate", func(b []byte) []byte { return squeezed(t, "deflate", b) },
		"gzip", func(b []byte) []byte { return squeezed(t, "gzip", b) }, orderMsg("SHIPPED"))
	tc := g.recordedOne(t)
	require.Equal(t, "1: {\"o-42\"}\n2: {\"PENDING\"}\n", tc.GrpcReq.Body.DecodedData)
	require.Equal(t, "1: {\"o-42\"}\n2: {\"SHIPPED\"}\n", tc.GrpcResp.Body.DecodedData)
}

// An answer that inflates past the test-case size limit (a zip bomb: a
// small compressed message) is no test case, skipped as one past the limit
// on the wire is, and is not inflated past it.
func TestRecordIncoming_AGRPCCallInflatingPastTheLimitIsNotRecorded(t *testing.T) {
	core, logs := observer.New(zapcore.WarnLevel)
	g := startGRPCIngressLogged(t, zap.New(core), nil, nil)
	sidedCall(t, g, "gzip", func(b []byte) []byte { return squeezed(t, "gzip", b) },
		"gzip", func(b []byte) []byte { return squeezed(t, "gzip", b) }, bytes.Repeat([]byte{0}, 64<<20))
	g.noneRecorded(t)
	require.Equal(t, 1, logs.FilterMessage("gRPC test case data exceeds the test-case size limit once inflated, skipping capture").Len(), "logs: %v", logs.All())
}

// A call the recording's filters leave out is skipped quietly, though its
// compressed message cannot be inflated.
func TestRecordIncoming_AFilteredCallKeployCannotInflateIsSkippedQuietly(t *testing.T) {
	core, logs := observer.New(zapcore.WarnLevel)
	g := startGRPCIngressLogged(t, zap.New(core), nil, func() models.IncomingOptions {
		return models.IncomingOptions{Filters: []models.Filter{{Path: "/shop.Orders/.*", FilterPolicy: "exclude"}}}
	})
	compressedCall(t, g, "snappy", func(b []byte) []byte { return append([]byte{0xff, 0x06}, b...) })
	g.noneRecorded(t)
	require.Zero(t, logs.Len(), "logs: %v", logs.All())
}
