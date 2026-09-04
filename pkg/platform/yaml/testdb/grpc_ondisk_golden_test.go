package testdb

import (
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	yamlLib "gopkg.in/yaml.v3"
)

// unaryGrpcGolden is the EXACT on-disk spec a unary gRPC test case encodes to.
//
// It was captured from origin/main before streaming support existed and is
// byte-for-byte what that build produces. Every gRPC mock and test case
// already on a user's disk is unary-shaped, so this string is the contract
// with all of them.
//
// If this test fails, a unary recording no longer round-trips through the
// build that wrote it. That is not a cosmetic diff: normalise, templatize,
// dedup and re-record all rewrite these files, and a decode error is fatal
// for the WHOLE file — mockdb and testdb both return the first decode error
// up rather than skipping the offending document. So a change here silently
// converts every existing recording into a shape the previous keploy cannot
// read, on the next unrelated operation, without the user asking for it.
//
// Adding a field to GrpcReq/GrpcResp is safe ONLY while it stays omitempty
// and stays empty for one message. Do not "fix" this test by re-capturing
// the golden; fix the encoder.
const unaryGrpcGolden = `metadata: {}
grpcReq:
    headers:
        pseudo_headers:
            :method: POST
            :path: /pkg.Svc/Method
        ordinary_headers:
            content-type: application/grpc
    body:
        compression_flag: 0
        message_length: 7
        decoded_data: '1: {"alpha"}'
    timestamp: 2023-11-14T22:13:20Z
grpcResp:
    headers:
        pseudo_headers:
            :method: POST
            :path: /pkg.Svc/Method
        ordinary_headers:
            content-type: application/grpc
    body:
        compression_flag: 0
        message_length: 7
        decoded_data: '1: {"alpha"}'
    trailers:
        pseudo_headers: {}
        ordinary_headers:
            grpc-status: "0"
    timestamp: 2023-11-14T22:13:20Z
created: 1700000000
assertions: {}
`

func unaryGrpcTestCase() models.TestCase {
	hdr := models.GrpcHeaders{
		PseudoHeaders:   map[string]string{":method": "POST", ":path": "/pkg.Svc/Method"},
		OrdinaryHeaders: map[string]string{"content-type": "application/grpc"},
	}
	body := models.GrpcLengthPrefixedMessage{CompressionFlag: 0, MessageLength: 7, DecodedData: `1: {"alpha"}`}
	ts := time.Unix(1700000000, 0).UTC()
	return models.TestCase{
		Version: models.V1Beta1, Kind: models.GRPC_EXPORT, Name: "grpc-unary-golden",
		GrpcReq: models.GrpcReq{Headers: hdr, Body: body, Timestamp: ts},
		GrpcResp: models.GrpcResp{
			Headers: hdr, Body: body,
			Trailers:  models.GrpcHeaders{PseudoHeaders: map[string]string{}, OrdinaryHeaders: map[string]string{"grpc-status": "0"}},
			Timestamp: ts,
		},
		Created: 1700000000,
	}
}

// TestEncodeTestcase_UnaryGrpcOnDiskShapeIsUnchanged pins the on-disk bytes
// through the REAL encoder, not a struct-level yaml.Marshal. The encoder is
// what actually writes user files, and it is where a regression would land.
func TestEncodeTestcase_UnaryGrpcOnDiskShapeIsUnchanged(t *testing.T) {
	doc, err := EncodeTestcase(unaryGrpcTestCase(), zap.NewNop())
	if err != nil {
		t.Fatalf("EncodeTestcase: %v", err)
	}
	out, err := yamlLib.Marshal(&doc.Spec)
	if err != nil {
		t.Fatalf("marshal spec: %v", err)
	}
	if string(out) != unaryGrpcGolden {
		t.Fatalf("a unary gRPC test case no longer encodes to the bytes origin/main wrote.\n"+
			"--- got ---\n%s\n--- want ---\n%s\n"+
			"Every existing recording is unary-shaped. A change here means the next normalise, "+
			"templatize, dedup or re-record rewrites those files into a shape the previous "+
			"keploy cannot read, and a decode error there is fatal for the whole file.",
			out, unaryGrpcGolden)
	}
}

// TestEncodeTestcase_StreamingGrpcAddsOnlyTheTail proves the new field is
// strictly additive: the streaming document is the golden one plus the tail,
// with nothing else moved or changed.
func TestEncodeTestcase_StreamingGrpcAddsOnlyTheTail(t *testing.T) {
	tc := unaryGrpcTestCase()
	tc.GrpcResp.SetMessages([]models.GrpcLengthPrefixedMessage{
		tc.GrpcResp.Body,
		{CompressionFlag: 0, MessageLength: 6, DecodedData: `1: {"beta"}`},
	})

	doc, err := EncodeTestcase(tc, zap.NewNop())
	if err != nil {
		t.Fatalf("EncodeTestcase: %v", err)
	}
	out, err := yamlLib.Marshal(&doc.Spec)
	if err != nil {
		t.Fatalf("marshal spec: %v", err)
	}
	got := string(out)

	// The tail must appear...
	if !contains(got, "additional_messages") {
		t.Fatalf("a two-message response did not emit additional_messages:\n%s", got)
	}
	// ...and message 0 must still be exactly where it was.
	if !contains(got, "        decoded_data: '1: {\"alpha\"}'") {
		t.Fatalf("message 0 moved or changed shape:\n%s", got)
	}
	// ...and the request, which is still unary, must be untouched.
	reqStart := indexOf(got, "grpcReq:")
	reqEnd := indexOf(got, "grpcResp:")
	if reqStart < 0 || reqEnd < 0 || contains(got[reqStart:reqEnd], "additional_messages") {
		t.Fatalf("a unary REQUEST gained a tail because the response had one:\n%s", got[maxInt(reqStart, 0):maxInt(reqEnd, 0)])
	}

	// And it must read back as the same two messages, through the REAL
	// reader (testdb.Decode), not a hand-rolled unmarshal — that is the
	// function which loads a user's recorded file.
	doc.Kind = models.GRPC_EXPORT
	doc.Name = tc.Name
	doc.Version = models.V1Beta1
	back, err := Decode(doc, zap.NewNop())
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if msgs := back.GrpcResp.AllMessages(); len(msgs) != 2 ||
		msgs[0].DecodedData != `1: {"alpha"}` || msgs[1].DecodedData != `1: {"beta"}` {
		t.Fatalf("round trip through the real reader changed the stream: %+v", msgs)
	}
	if back.GrpcReq.IsStream() {
		t.Fatal("the unary request read back as a stream")
	}
}

func contains(s, sub string) bool { return indexOf(s, sub) >= 0 }
func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// TestEncodeTestcase_UnaryViaSetMessagesMatchesGolden covers the path a
// RECORDER actually takes.
//
// The golden test above builds the fixture by assigning Body directly, which
// is how a hand-written test case looks. A recorder does not do that — it
// calls SetMessages with whatever it decoded, which for a unary call is a
// one-element slice. If SetMessages left an empty-but-non-nil tail there,
// omitempty would not suppress it in every encoder and unary recordings would
// start carrying an `additional_messages: []` key that older keploy has never
// seen. Same golden, reached the way production reaches it.
func TestEncodeTestcase_UnaryViaSetMessagesMatchesGolden(t *testing.T) {
	tc := unaryGrpcTestCase()
	body := tc.GrpcResp.Body
	tc.GrpcReq.SetMessages([]models.GrpcLengthPrefixedMessage{body})
	tc.GrpcResp.SetMessages([]models.GrpcLengthPrefixedMessage{body})

	doc, err := EncodeTestcase(tc, zap.NewNop())
	if err != nil {
		t.Fatalf("EncodeTestcase: %v", err)
	}
	out, err := yamlLib.Marshal(&doc.Spec)
	if err != nil {
		t.Fatalf("marshal spec: %v", err)
	}
	if string(out) != unaryGrpcGolden {
		t.Fatalf("a unary gRPC test case built through SetMessages does not match the golden "+
			"on-disk bytes.\n--- got ---\n%s\n--- want ---\n%s\nA recorder reaches this path for "+
			"every unary call, so a difference here changes every new recording.",
			out, unaryGrpcGolden)
	}
}
