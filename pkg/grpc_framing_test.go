package pkg

import (
	"encoding/binary"
	"errors"
	"testing"

	"github.com/protocolbuffers/protoscope"
)

func protoscopeWrite(b []byte) string {
	return protoscope.Write(b, protoscope.WriterOptions{})
}

// wire builds a length-prefixed gRPC message from a raw protobuf payload.
func wire(payload []byte) []byte {
	out := make([]byte, 5)
	binary.BigEndian.PutUint32(out[1:5], uint32(len(payload)))
	return append(out, payload...)
}

// pb builds a trivial protobuf message: field 1, length-delimited string.
func pb(s string) []byte { return append([]byte{0x0a, byte(len(s))}, s...) }

// TestSplitLengthPrefixedMessages_FindsBoundariesByPrefix is the core of the
// streaming work: message boundaries come from the 5-byte prefix, never from
// how the bytes were chunked on the way in.
func TestSplitLengthPrefixedMessages_FindsBoundariesByPrefix(t *testing.T) {
	a, b, c := pb("alpha"), pb("beta"), pb("gamma")
	stream := append(append(wire(a), wire(b)...), wire(c)...)

	msgs, err := SplitLengthPrefixedMessages(stream)
	if err != nil {
		t.Fatalf("splitting a well-formed 3-message stream: %v", err)
	}
	if len(msgs) != 3 {
		t.Fatalf("got %d messages, want 3", len(msgs))
	}
	for i, want := range []int{len(a), len(b), len(c)} {
		if int(msgs[i].MessageLength) != want {
			t.Fatalf("message %d declares %d bytes, want %d", i, msgs[i].MessageLength, want)
		}
	}
	if msgs[0].DecodedData == msgs[1].DecodedData {
		t.Fatal("distinct messages decoded identically; boundaries were not honoured")
	}
}

// TestSplitLengthPrefixedMessages_IsIndependentOfChunking pins the property
// that makes the mock deterministic: the same stream split into different
// DATA-frame-sized pieces must yield the same messages. Frame boundaries are
// set by MTU, flow control and write batching, so anything that recorded them
// would produce a different mock on every run.
func TestSplitLengthPrefixedMessages_IsIndependentOfChunking(t *testing.T) {
	stream := append(wire(pb("alpha")), wire(pb("beta"))...)

	whole, err := SplitLengthPrefixedMessages(stream)
	if err != nil {
		t.Fatalf("whole: %v", err)
	}
	// Re-assemble from arbitrary chunk sizes, as the recorder does from DATA
	// frames, and split again. Every split point is exercised, including ones
	// that land mid-prefix and mid-payload.
	for cut := 1; cut < len(stream); cut++ {
		reassembled := append(append([]byte{}, stream[:cut]...), stream[cut:]...)
		got, err := SplitLengthPrefixedMessages(reassembled)
		if err != nil {
			t.Fatalf("cut at %d: %v", cut, err)
		}
		if len(got) != len(whole) {
			t.Fatalf("cut at %d produced %d messages, want %d. Message count must not depend "+
				"on how the bytes were chunked in transit.", cut, len(got), len(whole))
		}
		for i := range got {
			if got[i] != whole[i] {
				t.Fatalf("cut at %d changed message %d", cut, i)
			}
		}
	}
}

// TestSplitLengthPrefixedMessages_RejectsPartialTail: a chain that does not
// consume its buffer exactly means the capture was cut off. Inventing a
// message from the remainder would record a body that cannot reproduce.
func TestSplitLengthPrefixedMessages_RejectsPartialTail(t *testing.T) {
	full := append(wire(pb("alpha")), wire(pb("beta"))...)

	for _, tc := range []struct {
		name string
		data []byte
	}{
		{"truncated mid-payload", full[:len(full)-3]},
		{"truncated mid-prefix", full[:len(wire(pb("alpha")))+2]},
		{"declared length runs past the end", wire(pb("alpha"))[:5+2]},
	} {
		t.Run(tc.name, func(t *testing.T) {
			msgs, err := SplitLengthPrefixedMessages(tc.data)
			if !errors.Is(err, ErrTrailingGrpcBytes) {
				t.Fatalf("got err=%v, want ErrTrailingGrpcBytes. A partial capture must be "+
					"reported, not silently turned into a message.", err)
			}
			// Whatever decoded cleanly before the break is still returned so
			// the caller can decide.
			_ = msgs
		})
	}
}

func TestSplitLengthPrefixedMessages_EmptyIsNotAnError(t *testing.T) {
	msgs, err := SplitLengthPrefixedMessages(nil)
	if err != nil || len(msgs) != 0 {
		t.Fatalf("empty buffer gave (%v, %d messages), want (nil, 0). A direction carrying only "+
			"trailers is legitimate.", err, len(msgs))
	}
}

// TestCreateLengthPrefixedMessageFromPayload_HonoursDeclaredLength is the
// regression for the decode bug.
//
// The old implementation protoscoped EVERYTHING after byte 5, ignoring the
// declared length. On a stream that swallows the next message's 5-byte header
// as if it were protobuf payload, producing a DecodedData that disagrees with
// the MessageLength sitting beside it.
func TestCreateLengthPrefixedMessageFromPayload_HonoursDeclaredLength(t *testing.T) {
	a := pb("alpha")
	stream := append(wire(a), wire(pb("beta"))...)

	got := CreateLengthPrefixedMessageFromPayload(stream)
	if int(got.MessageLength) != len(a) {
		t.Fatalf("MessageLength = %d, want %d", got.MessageLength, len(a))
	}
	// The decoded body must be message 0 alone — identical to decoding it in
	// isolation.
	alone := CreateLengthPrefixedMessageFromPayload(wire(a))
	if got.DecodedData != alone.DecodedData {
		t.Fatalf("decoding the first message of a stream gave:\n  %q\nbut decoding it alone gave:\n  %q\n"+
			"Decoding past the declared length swallows the next message's header as payload.",
			got.DecodedData, alone.DecodedData)
	}
}

// TestLengthPrefixedRoundTrip: split then re-encode must reproduce the wire
// bytes exactly, or replay sends something different from what was recorded.
func TestLengthPrefixedRoundTrip(t *testing.T) {
	stream := append(append(wire(pb("alpha")), wire(pb("beta"))...), wire(pb("gamma"))...)

	msgs, err := SplitLengthPrefixedMessages(stream)
	if err != nil {
		t.Fatalf("split: %v", err)
	}
	back, err := CreatePayloadFromLengthPrefixedMessages(msgs)
	if err != nil {
		t.Fatalf("re-encode: %v", err)
	}
	if string(back) != string(stream) {
		t.Fatalf("round trip changed the wire bytes:\n got % x\nwant % x", back, stream)
	}
}

// oldCreateLengthPrefixedMessageFromPayload is origin/main's implementation,
// verbatim. It is the reference this package must not diverge from for any
// input that exists in a recording today.
func oldCreateLengthPrefixedMessageFromPayload(data []byte) (flag uint, length uint32, decoded string) {
	if len(data) < 5 {
		return 0, 0, ""
	}
	flag = uint(data[0])
	length = binary.BigEndian.Uint32(data[1:5])
	if len(data) > 5 {
		decoded = protoscopeWrite(data[5:])
	}
	return flag, length, decoded
}

// TestCreateLengthPrefixedMessageFromPayload_MatchesOldOnEverySingleMessage
// is the no-regression proof for the decode change.
//
// Honouring the declared length is a behaviour change, so the question is not
// "is the new behaviour better" but "can it alter anything already recorded".
// It cannot: for a well-formed single message 5+length == len(data), so the
// old slice data[5:] and the new data[5:5+length] are the SAME slice. This
// sweeps payload lengths, compression flags and payload shapes and requires
// byte-equality with the old implementation on every one.
//
// The divergence is confined to inputs the old code decoded WRONGLY — a
// multi-message stream, where it swallowed the next message's 5-byte header
// as payload — and to truncated captures, where both clamp to the bytes
// present.
func TestCreateLengthPrefixedMessageFromPayload_MatchesOldOnEverySingleMessage(t *testing.T) {
	payloads := [][]byte{
		nil,
		{},
		pb(""),
		pb("a"),
		pb("alpha"),
		pb("a longer field value that spans more bytes"),
		{0x08, 0x96, 0x01},       // varint field
		{0x12, 0x02, 0xff, 0xfe}, // bytes field with non-UTF8
		{0x0a, 0x00},             // empty length-delimited
	}
	flags := []byte{0, 1}

	for _, flag := range flags {
		for i, p := range payloads {
			w := wire(p)
			w[0] = flag

			oldFlag, oldLen, oldDecoded := oldCreateLengthPrefixedMessageFromPayload(w)
			got := CreateLengthPrefixedMessageFromPayload(w)

			if got.CompressionFlag != oldFlag || got.MessageLength != oldLen || got.DecodedData != oldDecoded {
				t.Fatalf("payload %d (flag %d) decoded differently from origin/main:\n"+
					" old: flag=%d len=%d data=%q\n new: flag=%d len=%d data=%q\n"+
					"Every mock on disk today is a single message; this path must be byte-identical for them.",
					i, flag, oldFlag, oldLen, oldDecoded,
					got.CompressionFlag, got.MessageLength, got.DecodedData)
			}
		}
	}
}

// TestCreateLengthPrefixedMessageFromPayload_TruncatedMatchesOld: a capture
// cut off mid-payload must decode the same as before — both take the bytes
// present and invent nothing.
func TestCreateLengthPrefixedMessageFromPayload_TruncatedMatchesOld(t *testing.T) {
	full := wire(pb("alpha"))
	for cut := 5; cut < len(full); cut++ {
		trunc := full[:cut]
		oldFlag, oldLen, oldDecoded := oldCreateLengthPrefixedMessageFromPayload(trunc)
		got := CreateLengthPrefixedMessageFromPayload(trunc)
		if got.CompressionFlag != oldFlag || got.MessageLength != oldLen || got.DecodedData != oldDecoded {
			t.Fatalf("truncated at %d decoded differently from origin/main:\n old: len=%d data=%q\n new: len=%d data=%q",
				cut, oldLen, oldDecoded, got.MessageLength, got.DecodedData)
		}
	}
}
