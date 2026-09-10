package models

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	yamlLib "gopkg.in/yaml.v3"
)

func TestHTTPResp_UnmarshalYAML_StreamingSSEBody(t *testing.T) {
	input := `
status_code: 200
header:
  Content-Type: text/event-stream
body:
  - ts: 2026-02-23T11:17:07.5708415Z
    data:
      comment: heartbeat
  - ts: 2026-02-23T11:17:07.57090625Z
    data:
      id: "1"
      event: message
      data: '{"ok":true}'
status_message: OK
proto_major: 1
proto_minor: 1
timestamp: 2026-02-23T11:17:07.57090625Z
`

	var got HTTPResp
	require.NoError(t, yamlLib.Unmarshal([]byte(input), &got))

	require.Len(t, got.StreamBody, 2)
	assert.Equal(t, "comment", got.StreamBody[0].Data[0].Key)
	assert.Equal(t, "heartbeat", got.StreamBody[0].Data[0].Value)
	assert.Equal(t, "id", got.StreamBody[1].Data[0].Key)
	assert.Equal(t, "event", got.StreamBody[1].Data[1].Key)
	assert.Equal(t, "data", got.StreamBody[1].Data[2].Key)
	assert.Equal(t, "1", got.StreamBody[1].Data[0].Value)
	assert.Contains(t, got.Body, ":heartbeat")
	assert.Contains(t, got.Body, "id:1")
	assert.Contains(t, got.Body, `data:{"ok":true}`)
}

func TestHTTPResp_UnmarshalYAML_StreamingRawBody(t *testing.T) {
	input := `
status_code: 200
header:
  Content-Type: application/x-ndjson
body:
  - ts: 2026-02-23T11:17:07.5708415Z
    data:
      raw: '{"chunk_id":1}'
  - ts: 2026-02-23T11:17:08.5708415Z
    data:
      raw: '{"chunk_id":2}'
status_message: OK
proto_major: 1
proto_minor: 1
timestamp: 2026-02-23T11:17:08.5708415Z
`

	var got HTTPResp
	require.NoError(t, yamlLib.Unmarshal([]byte(input), &got))

	require.Len(t, got.StreamBody, 2)
	assert.Equal(t, "raw", got.StreamBody[0].Data[0].Key)
	assert.Equal(t, `{"chunk_id":1}`, got.StreamBody[0].Data[0].Value)
	assert.Equal(t, `{"chunk_id":1}`+"\n"+`{"chunk_id":2}`, got.Body)
}

func TestHTTPResp_UnmarshalYAML_ScalarTextPlainDoesNotDeriveStreamBody(t *testing.T) {
	input := `
status_code: 200
header:
  Content-Type: text/plain
body: |
  line-1
  line-2
status_message: OK
proto_major: 1
proto_minor: 1
timestamp: 2026-02-23T11:17:08.5708415Z
`

	var got HTTPResp
	require.NoError(t, yamlLib.Unmarshal([]byte(input), &got))

	// After removing backward compat, scalar body should NOT derive StreamBody.
	assert.Len(t, got.StreamBody, 0, "scalar text/plain body should not derive StreamBody after backward compat removal")
	assert.Contains(t, got.Body, "line-1")
	assert.Contains(t, got.Body, "line-2")
}

func TestHTTPResp_MarshalYAML_StreamingSSEBody(t *testing.T) {
	ts := time.Date(2026, 2, 23, 11, 17, 7, 570906250, time.UTC)
	resp := HTTPResp{
		StatusCode: 200,
		Header: map[string]string{
			"Content-Type": "text/event-stream",
		},
		Body:      "id:1\nevent:message\ndata:{\"ok\":true}\n\n",
		Timestamp: ts,
		StreamBody: []HTTPStreamChunk{
			{
				TS: ts,
				Data: []HTTPStreamDataField{
					{Key: "id", Value: "1"},
					{Key: "event", Value: "message"},
					{Key: "data", Value: `{"ok":true}`},
				},
			},
		},
	}

	out, err := yamlLib.Marshal(resp)
	require.NoError(t, err)
	body := string(out)

	assert.Contains(t, body, "\nbody:\n")
	assert.Contains(t, body, "- ts:")
	assert.Contains(t, body, "data:")
	assert.Contains(t, body, "id: \"1\"")
	assert.Contains(t, body, "event: message")
	assert.Contains(t, body, "data: '{\"ok\":true}'")
	assert.False(t, strings.Contains(body, "body: |"), "streaming body should not be serialized as scalar block")
}

func TestHTTPResp_MarshalYAML_TextPlainStreamingBodyAsRawChunks(t *testing.T) {
	ts := time.Date(2026, 2, 24, 5, 53, 37, 0, time.UTC)
	resp := HTTPResp{
		StatusCode: 200,
		Header: map[string]string{
			"Content-Type": "text/plain",
		},
		Body:      "line-1\nline-2\nline-3\n",
		Timestamp: ts,
		StreamBody: []HTTPStreamChunk{
			{TS: ts, Data: []HTTPStreamDataField{{Key: "raw", Value: "line-1"}}},
			{TS: ts, Data: []HTTPStreamDataField{{Key: "raw", Value: "line-2"}}},
			{TS: ts, Data: []HTTPStreamDataField{{Key: "raw", Value: "line-3"}}},
		},
	}

	out, err := yamlLib.Marshal(resp)
	require.NoError(t, err)
	body := string(out)

	assert.Contains(t, body, "\nbody:\n")
	assert.Contains(t, body, "raw: line-1")
	assert.Contains(t, body, "raw: line-2")
	assert.Contains(t, body, "raw: line-3")
	assert.False(t, strings.Contains(body, "body: |"), "text/plain stream body should be serialized as chunk list")
}

func TestHTTPResp_MarshalYAML_SSEMultilineDataUsesSingleDataField(t *testing.T) {
	ts := time.Date(2026, 2, 24, 5, 53, 37, 0, time.UTC)
	resp := HTTPResp{
		StatusCode: 200,
		Header: map[string]string{
			"Content-Type": "text/event-stream",
		},
		Body: strings.Join([]string{
			"id:1",
			"event:message",
			"data:line-1",
			"data:line-2",
			"",
		}, "\n"),
		Timestamp: ts,
		StreamBody: []HTTPStreamChunk{
			{
				TS: ts,
				Data: []HTTPStreamDataField{
					{Key: "id", Value: "1"},
					{Key: "event", Value: "message"},
					{Key: "data", Value: "line-1\nline-2"},
				},
			},
		},
	}

	out, err := yamlLib.Marshal(resp)
	require.NoError(t, err)
	body := string(out)

	// Avoid duplicate "data:" keys in a YAML map by storing multiline data in one scalar.
	assert.Contains(t, body, "data: |-")
	assert.Contains(t, body, "line-1")
	assert.Contains(t, body, "line-2")
}

// TestHTTPResp_YAML_NonUTF8BodyRoundTrips is the regression guard for the
// defect that ended a 46-hour production recording of prod/api-server.
//
// A gzip-encoded response body is perfectly valid HTTP and invalid UTF-8. The
// streaming chunk encoder used to stamp an explicit `!!str` tag on it, and
// yaml.v3 refuses that outright — "cannot marshal invalid UTF-8 data as !!str".
// That error surfaced as a failed mock insert, and the recorder treats a failed
// insert as fatal, so ONE response body destroyed the whole session.
//
// The Content-Type matters: only the streaming kinds took the chunk path.
// application/json always used the flat scalar body and always worked, which is
// why this went unnoticed. text/event-stream additionally corrupted the bytes
// silently, because parseSSEBodyToChunks lower-cases the field name and
// strings.ToLower rewrites every invalid byte to U+FFFD.
func TestHTTPResp_YAML_NonUTF8BodyRoundTrips(t *testing.T) {
	// gzip magic + a deflate payload: real bytes an HTTP response carries.
	body := string([]byte{
		0x1f, 0x8b, 0x08, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0xff,
		0xca, 0x48, 0xcd, 0xc9, 0xc9, 0x07, 0x04, 0x00, 0x00, 0xff, 0xff,
	})
	if utf8.ValidString(body) {
		t.Fatal("fixture must be invalid UTF-8, otherwise this test proves nothing")
	}

	for _, ct := range []string{
		"application/octet-stream",
		"text/plain",
		"application/x-ndjson",
		"text/event-stream",
		"application/json", // control: always worked, must keep working
	} {
		t.Run(ct, func(t *testing.T) {
			in := HTTPResp{
				StatusCode: 200,
				Header:     map[string]string{"Content-Type": ct},
				Body:       body,
				Timestamp:  time.Unix(1700000000, 0).UTC(),
			}

			out, err := yamlLib.Marshal(&in)
			if err != nil {
				t.Fatalf("marshalling a non-UTF-8 %s body failed: %v\n"+
					"  this is the exact production failure: the encoder tags the body !!str, "+
					"yaml.v3 rejects it, the mock insert fails, and the recorder tears down the session", ct, err)
			}

			var got HTTPResp
			if err := yamlLib.Unmarshal(out, &got); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if got.Body != in.Body {
				t.Fatalf("body did not round-trip for %s\n  want % x\n  got  % x\n"+
					"  (silent corruption: a mock recorded this way replays the wrong bytes)",
					ct, in.Body, got.Body)
			}
		})
	}
}

// TestHTTPResp_YAML_TextStreamsStillChunk guards the other direction: the UTF-8
// guard must not disable streaming for ordinary text bodies, which is the whole
// point of the chunk encoder.
func TestHTTPResp_YAML_TextStreamsStillChunk(t *testing.T) {
	in := HTTPResp{
		StatusCode: 200,
		Header:     map[string]string{"Content-Type": "application/octet-stream"},
		Body:       "hello world",
		Timestamp:  time.Unix(1700000000, 0).UTC(),
	}
	out, err := yamlLib.Marshal(&in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(out), "raw") {
		t.Fatalf("a valid-UTF-8 octet-stream body stopped being chunked; the guard must only "+
			"divert NON-UTF-8 bodies.\n---\n%s", out)
	}
	var got HTTPResp
	if err := yamlLib.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Body != in.Body {
		t.Fatalf("text body round-trip broke: want %q got %q", in.Body, got.Body)
	}
}

// TestStringNode_NonUTF8UsesBinaryTagAndDecodesBack pins the two halves of
// the encode/decode pair directly, rather than only through HTTPResp. An
// explicit `!!str` on invalid UTF-8 makes yaml.v3 hard-fail ("cannot marshal
// invalid UTF-8 data as !!str"), which aborted the mock insert and, before
// the recorder learned to skip a bad mock, ended the whole recording. The
// decode half matters just as much: yaml.v3 leaves the BASE64 TEXT in
// node.Value for a !!binary scalar, so a yamlNodeToString that returned it
// raw would hand replay the base64 string and mismatch every time.
func TestStringNode_NonUTF8UsesBinaryTagAndDecodesBack(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{"gzip_magic", string([]byte{0x1f, 0x8b, 0x08, 0x00, 0xff, 0xfe})},
		{"lone_continuation", string([]byte{0x80})},
		{"utf16_bom", string([]byte{0xff, 0xfe, 0x48, 0x00})},
		{"long", strings.Repeat(string([]byte{0xff, 0xfe, 0x00, 0x8b}), 64)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.False(t, utf8.ValidString(tc.in), "fixture must be invalid UTF-8")

			n := stringNode(tc.in)
			assert.Equal(t, "!!binary", n.Tag,
				"invalid UTF-8 must not be stamped !!str — yaml.v3 hard-fails on it")

			// Go through a real marshal/unmarshal so the emitter's line
			// folding is exercised, not just the in-memory node.
			out, err := yamlLib.Marshal(n)
			require.NoError(t, err, "marshalling a !!binary scalar must not fail")

			var got yamlLib.Node
			require.NoError(t, yamlLib.Unmarshal(out, &got))
			require.Equal(t, yamlLib.DocumentNode, got.Kind)
			assert.Equal(t, tc.in, yamlNodeToString(got.Content[0]),
				"binary body did not survive the YAML round-trip")
		})
	}
}

// TestStringNode_UTF8StaysReadable guards the other direction: valid UTF-8
// must keep its plain !!str form so mock files stay human-readable and
// diffable. Base64-ing everything would "fix" the crash and ruin the files.
func TestStringNode_UTF8StaysReadable(t *testing.T) {
	for _, in := range []string{"", "hello", "café ☕", "{\"k\":\"v\"}"} {
		n := stringNode(in)
		assert.Equal(t, "!!str", n.Tag, "valid UTF-8 %q must stay !!str", in)
		assert.Equal(t, in, n.Value)
		assert.Equal(t, in, yamlNodeToString(n))
	}
}

// TestYAMLNodeToString_FoldedBinaryScalar covers reading a mock file whose
// !!binary scalar is wrapped across lines. yaml.v3 emits binary on one long
// line, but the YAML spec says a !!binary payload may carry line breaks and
// libyaml-based writers (PyYAML, ruby-psych) wrap at 80 columns by default.
//
// The `>` (folded) indicator is the case that bites: YAML joins folded lines
// with a SPACE, and base64.StdEncoding rejects a space. (It happens to skip
// \r and \n, so a `|` literal block would decode even without the strip and
// would prove nothing here.) Without stripBase64Whitespace the decode errors
// and yamlNodeToString falls through to handing replay the raw base64 text.
func TestYAMLNodeToString_FoldedBinaryScalar(t *testing.T) {
	want := strings.Repeat(string([]byte{0xff, 0xfe, 0x00, 0x8b}), 24)

	const doc = `body: !!binary >-
  //4Ai//+AIv//gCL//4Ai//+AIv//gCL//4Ai//+AIv//gCL//4Ai//+AIv//gCL//4Ai//+AIv/
  /gCL//4Ai//+AIv//gCL//4Ai//+AIv//gCL//4Ai//+AIv//gCL
`
	var m struct {
		Body yamlLib.Node `yaml:"body"`
	}
	require.NoError(t, yamlLib.Unmarshal([]byte(doc), &m))
	require.Equal(t, "!!binary", m.Body.Tag)
	assert.Contains(t, m.Body.Value, " ",
		"fixture must actually fold to a space, otherwise this asserts nothing")

	assert.Equal(t, want, yamlNodeToString(&m.Body),
		"folded !!binary must decode to the original bytes, not the base64 text")
}

// TestHTTPResp_YAML_NonUTF8ChunksOnlyStillCarryTheBody covers the arm the
// UTF-8 guard opens up. When the chunks fail the UTF-8 check, MarshalYAML falls
// back to the flat scalar body — but StreamBody and Body travel independently
// over the transport, so a producer that populated ONLY the chunk form has an
// empty Body and the fallback would write `body: ""`, silently losing the whole
// response. Guarding against a marshal failure by dropping the payload instead
// is not a fix; the bytes have to reach the flat !!binary scalar.
func TestHTTPResp_YAML_NonUTF8ChunksOnlyStillCarryTheBody(t *testing.T) {
	raw := string([]byte{0x1f, 0x8b, 0x08, 0x00, 0xff, 0xfe})
	require.False(t, utf8.ValidString(raw), "fixture must be invalid UTF-8")

	in := HTTPResp{
		StatusCode: 200,
		Header:     map[string]string{"Content-Type": "application/octet-stream"},
		// Body deliberately empty: only the chunk form is populated.
		Timestamp: time.Unix(1700000000, 0).UTC(),
		StreamBody: []HTTPStreamChunk{
			{TS: time.Unix(1700000000, 0).UTC(), Data: []HTTPStreamDataField{{Key: "raw", Value: raw}}},
		},
	}

	out, err := yamlLib.Marshal(&in)
	require.NoError(t, err, "marshalling non-UTF-8 chunks must not fail")

	var got HTTPResp
	require.NoError(t, yamlLib.Unmarshal(out, &got))
	assert.Equal(t, raw, got.Body,
		"the response body was silently dropped: the chunk form was rejected for being non-UTF-8 "+
			"and the flat fallback encoded an empty Body")
}

// TestDecodeStreamDataFields_BinaryKeyDecodes pins the key half of the chunk
// decoder. stringNode can emit a data-field KEY as !!binary, and yaml.v3 leaves
// the BASE64 TEXT in node.Value for those — reading Value raw substitutes the
// base64 for the real field name. Uses a folded (`>-`) scalar so the whitespace
// strip is exercised too: YAML joins folded lines with a space, which
// base64.StdEncoding rejects.
func TestDecodeStreamDataFields_BinaryKeyDecodes(t *testing.T) {
	// base64 of the 48 bytes below, wrapped so the fold inserts a space.
	const doc = `data:
  ? !!binary >-
    //4Ai//+AIv//gCL//4Ai//+AIv//gCL//4Ai//+AIv//gCL//4Ai//+AIv//gCL//4Ai//+AIv/
    /gCL
  : plain-value
  ordinary-key: v2
`
	var m struct {
		Data yamlLib.Node `yaml:"data"`
	}
	require.NoError(t, yamlLib.Unmarshal([]byte(doc), &m))

	fields, err := decodeStreamDataFields(&m.Data)
	require.NoError(t, err)
	require.Len(t, fields, 2)

	wantKey := strings.Repeat(string([]byte{0xff, 0xfe, 0x00, 0x8b}), 15)
	assert.Equal(t, wantKey, fields[0].Key,
		"a !!binary data-field key was not decoded — replay would see the base64 text as the field name")
	assert.Equal(t, "plain-value", fields[0].Value)
	// An ordinary key must still be trimmed as before.
	assert.Equal(t, "ordinary-key", fields[1].Key)
}

// TestHTTPResp_YAML_NonUTF8SSEFieldNameSurvivesFallback covers the corner where
// it is the SSE field NAME, not the value, that is not valid UTF-8.
//
// The chunk form is rejected for exactly that reason and MarshalYAML falls back
// to the flat body — which for text/event-stream is rebuilt by
// streamChunksToLegacyBody, and that used to lower-case every field name.
// strings.ToLower rewrites each invalid byte to U+FFFD, so the fallback added to
// stop the body being dropped would instead have shipped it corrupted: the same
// silent mangling the UTF-8 guard exists to prevent, one layer down.
func TestHTTPResp_YAML_NonUTF8SSEFieldNameSurvivesFallback(t *testing.T) {
	badKey := string([]byte{0xff, 0xfe, 0x41})
	require.False(t, utf8.ValidString(badKey), "fixture must be invalid UTF-8")

	in := HTTPResp{
		StatusCode: 200,
		Header:     map[string]string{"Content-Type": "text/event-stream"},
		Timestamp:  time.Unix(1700000000, 0).UTC(),
		StreamBody: []HTTPStreamChunk{
			{TS: time.Unix(1700000000, 0).UTC(), Data: []HTTPStreamDataField{{Key: badKey, Value: "v"}}},
		},
	}

	out, err := yamlLib.Marshal(&in)
	require.NoError(t, err)

	var got HTTPResp
	require.NoError(t, yamlLib.Unmarshal(out, &got))
	assert.Contains(t, got.Body, badKey,
		"the non-UTF-8 SSE field name was mangled to U+FFFD by strings.ToLower in the flat-body fallback")
	assert.NotContains(t, got.Body, "�", "replacement characters appeared in the body")
}
