package models

import (
	"encoding/json"
	"testing"

	yamlLib "gopkg.in/yaml.v3"
)

// TestAllMessages_ZeroBodyStillYieldsOneMessage is the guard that keeps a
// currently-passing unary test from turning into a hang.
//
// A zero-value Body is a REAL recorded shape, not an empty one: the gRPC
// health-check test case stores `message_length: 0` with an empty
// `decoded_data`, and replay sends exactly one message with a nil payload for
// it. If AllMessages returned an empty slice for that, `for range
// AllMessages()` would send nothing and the RPC would hang waiting for a
// response that is never written.
func TestAllMessages_ZeroBodyStillYieldsOneMessage(t *testing.T) {
	for _, tc := range []struct {
		name string
		got  []GrpcLengthPrefixedMessage
	}{
		{"request", GrpcReq{}.AllMessages()},
		{"response", GrpcResp{}.AllMessages()},
	} {
		if len(tc.got) != 1 {
			t.Fatalf("%s: a zero-value body yielded %d messages, want exactly 1. The health-check "+
				"test case records message_length:0 with empty decoded_data and replay sends one "+
				"empty message for it; returning none makes that RPC hang.", tc.name, len(tc.got))
		}
	}
}

func TestSetMessages_RoundTripsAndKeepsUnaryTailNil(t *testing.T) {
	msgs := []GrpcLengthPrefixedMessage{
		{MessageLength: 5, DecodedData: `1: {"alpha"}`},
		{MessageLength: 4, DecodedData: `1: {"beta"}`},
		{MessageLength: 5, DecodedData: `1: {"gamma"}`},
	}

	var req GrpcReq
	req.SetMessages(msgs)
	if got := req.AllMessages(); len(got) != 3 || got[0].DecodedData != msgs[0].DecodedData ||
		got[2].DecodedData != msgs[2].DecodedData {
		t.Fatalf("round trip lost order or content: %+v", got)
	}
	if !req.IsStream() {
		t.Fatal("three messages should report IsStream")
	}

	// One message must leave the tail NIL, not an empty slice: omitempty is
	// what keeps a unary mock byte-identical on disk, and an empty non-nil
	// slice still marshals in some encoders.
	var unary GrpcResp
	unary.SetMessages(msgs[:1])
	if unary.AdditionalMessages != nil {
		t.Fatalf("a single message left AdditionalMessages = %#v, want nil", unary.AdditionalMessages)
	}
	if unary.IsStream() {
		t.Fatal("one message must not report IsStream")
	}

	// Empty input zeroes the direction rather than leaving a stale body.
	var stale GrpcResp
	stale.SetMessages(msgs)
	stale.SetMessages(nil)
	if stale.Body != (GrpcLengthPrefixedMessage{}) || stale.AdditionalMessages != nil {
		t.Fatalf("SetMessages(nil) left %+v, want a zeroed direction", stale)
	}
}

// TestUnaryEncodingIsUnchanged is the compatibility guard that matters most.
//
// Every mock already on a user's disk is unary-shaped. If adding the tail
// field changed what a unary mock serialises to, then normalise, templatize,
// dedup or a re-record would silently rewrite those files into a shape older
// keploy cannot read — and a decode error is fatal for the WHOLE file, not
// just the offending document. Nothing new may appear on disk until there is
// actually a second message.
func TestUnaryEncodingIsUnchanged(t *testing.T) {
	body := GrpcLengthPrefixedMessage{CompressionFlag: 0, MessageLength: 7, DecodedData: `1: {"alpha"}`}

	resp := GrpcResp{Body: body}
	y, err := yamlLib.Marshal(resp)
	if err != nil {
		t.Fatalf("yaml: %v", err)
	}
	j, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("json: %v", err)
	}
	for _, out := range []string{string(y), string(j)} {
		if containsKey(out, "additional_messages") {
			t.Fatalf("a unary response emitted additional_messages:\n%s\nA unary mock must stay "+
				"byte-identical to one written before streaming was representable, or every "+
				"rewrite is a one-way upgrade the user did not ask for.", out)
		}
	}
}

// TestLegacyMockDecodesWithNoTail pins the read direction: a mock written
// before this field existed must load with exactly one message.
func TestLegacyMockDecodesWithNoTail(t *testing.T) {
	const legacy = `
headers:
    pseudo_headers: {}
    ordinary_headers: {}
body:
    compression_flag: 0
    message_length: 7
    decoded_data: '1: {"alpha"}'
trailers:
    pseudo_headers: {}
    ordinary_headers: {}
timestamp: 0001-01-01T00:00:00Z
`
	var resp GrpcResp
	if err := yamlLib.Unmarshal([]byte(legacy), &resp); err != nil {
		t.Fatalf("a pre-streaming mock failed to decode: %v", err)
	}
	if got := resp.AllMessages(); len(got) != 1 || got[0].MessageLength != 7 {
		t.Fatalf("legacy mock decoded to %+v, want exactly one 7-byte message", got)
	}
	if resp.IsStream() {
		t.Fatal("a legacy mock must not look like a stream")
	}
}

// TestStreamingMockRoundTripsThroughYAML covers the new shape end to end.
func TestStreamingMockRoundTripsThroughYAML(t *testing.T) {
	var resp GrpcResp
	resp.SetMessages([]GrpcLengthPrefixedMessage{
		{MessageLength: 7, DecodedData: `1: {"alpha"}`},
		{MessageLength: 6, DecodedData: `1: {"beta"}`},
	})
	out, err := yamlLib.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !containsKey(string(out), "additional_messages") {
		t.Fatalf("a two-message response did not emit additional_messages:\n%s", out)
	}
	var back GrpcResp
	if err := yamlLib.Unmarshal(out, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	got := back.AllMessages()
	if len(got) != 2 || got[0].DecodedData != `1: {"alpha"}` || got[1].DecodedData != `1: {"beta"}` {
		t.Fatalf("round trip changed the stream: %+v", got)
	}
}

func containsKey(s, key string) bool {
	for i := 0; i+len(key) <= len(s); i++ {
		if s[i:i+len(key)] == key {
			return true
		}
	}
	return false
}
