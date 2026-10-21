package models

import (
	"fmt"
	"net/http"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

type Method string

// BodyRef stores a reference to a large request body that has been offloaded
// to the assets directory (bodies > 1MB). When BodyRef is set, Body will be empty.
type BodyRef struct {
	Path string `json:"path" yaml:"path"` // relative path to the asset file
	Size int64  `json:"size" yaml:"size"` // original content size in bytes
}

// HeaderLineLengths records, for each header name that arrived on more than one
// wire line, the length in bytes of each line's value, in arrival order.
//
// Header keeps one value per name, the lines joined with "," (RFC 9110 §5.3),
// because matching, noise and templating all work on that form. The join alone
// cannot be undone: `X-A: a` twice and `X-A: a,a` once both store "a,a",
// and a value can carry commas of its own. The lengths say where each line
// ends, so replay sends the lines again (pkg.ToWireHTTPHeader) instead of one
// folded line, which is a different request to an app that reads the first
// value.
//
// Lengths rather than a second copy of the values, so nothing that a secret
// encryptor or obfuscator rewrites in Header survives here in plain text. A
// Header value that no longer fits its lengths (edited, templated, obfuscated)
// is sent as one line, as a recording without lengths always was.
type HeaderLineLengths map[string][]int

type HTTPReq struct {
	Method     Method            `json:"method" yaml:"method"`
	ProtoMajor int               `json:"proto_major" yaml:"proto_major"` // e.g. 1
	ProtoMinor int               `json:"proto_minor" yaml:"proto_minor"` // e.g. 0
	URL        string            `json:"url" yaml:"url"`
	URLParams  map[string]string `json:"url_params" yaml:"url_params,omitempty"`
	Header     map[string]string `json:"header" yaml:"header"`
	Body       string            `json:"body" yaml:"body"`
	BodyRef    BodyRef           `json:"body_ref,omitempty" yaml:"body_ref,omitempty"` // set when body is offloaded to assets (>1MB)
	Binary     string            `json:"binary" yaml:"binary,omitempty"`
	Form       []FormData        `json:"form" yaml:"form,omitempty"`
	Timestamp  time.Time         `json:"timestamp" yaml:"timestamp"`

	// HeaderLineLengths holds the wire lines behind a Header value that arrived
	// on more than one line (see the type). Absent for every other request, so
	// a recording without a repeated header is unchanged.
	HeaderLineLengths HeaderLineLengths `json:"header_line_lengths,omitempty" yaml:"header_line_lengths,omitempty"`

	// URLRendered marks a URL a replay rendered from its test set's
	// templates (pkg/templates): each value in it is escaped for its place,
	// so it is sent as it stands. Never stored.
	URLRendered bool `json:"-" yaml:"-" bson:"-"`

	// NOTE: schema-noise (req_body_noise) is NOT stored here. It lives on the
	// kind-agnostic MockSpec.ReqBodyNoise for every parser, HTTP included, so the
	// learn/enforce flow is uniform across protocols (see pkg/agent/proxy/
	// integrations/schemanoise).
}

type HTTPSchema struct {
	Metadata         map[string]string             `json:"metadata" yaml:"metadata"`
	Request          HTTPReq                       `json:"req" yaml:"req"`
	Response         HTTPResp                      `json:"resp" yaml:"resp"`
	Objects          []*OutputBinary               `json:"objects" yaml:"objects"`
	Assertions       map[AssertionType]interface{} `json:"assertions" yaml:"assertions,omitempty"`
	Created          int64                         `json:"created" yaml:"created,omitempty"`
	ReqTimestampMock time.Time                     `json:"reqTimestampMock" yaml:"reqTimestampMock,omitempty"`
	ResTimestampMock time.Time                     `json:"resTimestampMock" yaml:"resTimestampMock,omitempty"`
	AppPort          uint16                        `json:"app_port" yaml:"app_port,omitempty"`
}

type FormData struct {
	Key       string   `json:"key" bson:"key" yaml:"key"`
	Values    []string `json:"values" bson:"values,omitempty" yaml:"values,omitempty"`
	Paths     []string `json:"paths" bson:"paths,omitempty" yaml:"paths,omitempty"`
	FileNames []string `json:"file_names,omitempty" bson:"file_names,omitempty" yaml:"-"`
	// FileContents carries a file part's bytes, index-aligned with FileNames
	// and Paths, for a test case persisted by another process than the one
	// that captured it: the DaemonSet agent streams its test cases to
	// k8s-proxy's control plane, where the capture's temp file in Paths does
	// not exist (see conn.InlineFormFiles). []byte so JSON carries binary
	// content intact, which a string in Values does not. It rides only that
	// JSON upload: the persister writes it to the test case's assets and
	// clears it, and yaml/bson "-" keep it out of stored test cases.
	FileContents [][]byte `json:"file_contents,omitempty" bson:"-" yaml:"-"`
}

// HTTPResp is an HTTP response. StreamBody is a streamed body's frames, as a
// recording keeps them in YAML (MarshalYAML). JSON carries the body alone, one
// form of the stream: Keploy Cloud receives and serves a test case as JSON and
// edits its Body (the golden body), so frames carried beside it would go stale
// and outvote the edit. A test case read from frames has a Body that splits back
// into them (streamChunksToLegacyBody), and the replay compares a stream that
// kept no frames with its Body, read as the app's stream is read.
type HTTPResp struct {
	StatusCode    int               `json:"status_code" yaml:"status_code"` // e.g. 200
	Header        map[string]string `json:"header" yaml:"header"`
	Body          string            `json:"body" yaml:"body"`
	StreamBody    []HTTPStreamChunk `json:"-" yaml:"-"`
	BodySkipped   bool              `json:"body_skipped,omitempty" yaml:"body_skipped,omitempty"` // true when body was >1MB and not saved
	BodySize      int64             `json:"body_size,omitempty" yaml:"body_size,omitempty"`       // original body size in bytes when BodySkipped is true
	StatusMessage string            `json:"status_message" yaml:"status_message"`
	ProtoMajor    int               `json:"proto_major" yaml:"proto_major"`
	ProtoMinor    int               `json:"proto_minor" yaml:"proto_minor"`
	Binary        string            `json:"binary" yaml:"binary,omitempty"`
	Timestamp     time.Time         `json:"timestamp" yaml:"timestamp"`

	// HeaderLineLengths: as on HTTPReq. Recorded for a mock's response, the
	// one recorded response keploy sends back out at replay.
	HeaderLineLengths HeaderLineLengths `json:"header_line_lengths,omitempty" yaml:"header_line_lengths,omitempty"`

	// exactBody: MarshalYAML writes Body as it is (WithExactBody).
	exactBody bool
}

// WithExactBody is h written with its body exactly as it is: MarshalYAML
// writes a streamed body (NDJSON, SSE, streamed text) as the chunks of a
// stream only when they give the body back byte for byte, and as the flat
// string otherwise. The chunks keep a stream's frames, not its bytes: they
// drop the newline after its last line, its blank lines and its CRs, and an
// SSE field's space after the colon. A mock's writer uses it: a replay serves
// a mock's body as it was recorded, and the app reads a stream by its bytes,
// a line once its newline comes. A test case keeps the chunked form, whose
// frames its stream comparison reads.
func (h HTTPResp) WithExactBody() HTTPResp {
	h.exactBody = true
	return h
}

// HTTPResponseHasNoBody reports whether a response carries no body whatever
// its headers say (RFC 9112 §6.3): the answer to a HEAD, an interim 1xx, 204
// or 304. Not a 101, nor a 2xx to a CONNECT: their message has no body either,
// but the connection then carries another protocol, which is no next HTTP
// message. It is the one rule for this, the proxy's framing and the replay's
// stream detection alike.
func HTTPResponseHasNoBody(reqMethod string, status int) bool {
	return reqMethod == http.MethodHead || IsInterimHTTPStatus(status) || status == http.StatusNoContent || status == http.StatusNotModified
}

// IsInterimHTTPStatus reports whether code is an interim response's (RFC 9110
// 15.2): a 1xx other than 101 (Switching Protocols). An interim response has
// no body, and the final response to the same request comes after it on the
// connection. After a 101 the connection speaks another protocol: it is final.
func IsInterimHTTPStatus(code int) bool {
	return code >= 100 && code < 200 && code != http.StatusSwitchingProtocols
}

// MongoDB's BSON DateTime is an int64 count of milliseconds, so the default
// `time.Time` -> BSON encoding silently drops nanoseconds. HTTPReq.Timestamp
// and HTTPResp.Timestamp are later used as the edges of the mock-matching
// time window, and the closing edge often lands in the exact same millisecond
// as the final MongoDB mock's `reqTimestampMock`. Truncation pushes that mock
// outside the window by a few microseconds, starving the `find` matcher.
//
// The BSON methods below persist both timestamps as RFC3339Nano strings so the
// round-trip through MongoDB preserves the original precision. MockEntry in
// mappings.go uses the same strategy (see FormatMockTimestamp).
//
// Field names on the shadow structs match the v1/v2 driver default
// (`strings.ToLower(fieldName)`) so the BSON wire shape is unchanged for every
// field except `timestamp`, which flips from BSON DateTime to BSON String.
// UnmarshalBSON transparently accepts either shape, so existing records written
// before this change continue to decode.

type httpReqBSON struct {
	Method     Method            `bson:"method"`
	ProtoMajor int               `bson:"protomajor"`
	ProtoMinor int               `bson:"protominor"`
	URL        string            `bson:"url"`
	URLParams  map[string]string `bson:"urlparams"`
	Header     map[string]string `bson:"header"`
	Body       string            `bson:"body"`
	BodyRef    BodyRef           `bson:"bodyref"`
	Binary     string            `bson:"binary"`
	Form       []FormData        `bson:"form"`
	Timestamp  string            `bson:"timestamp"`

	HeaderLineLengths HeaderLineLengths `bson:"headerlinelengths,omitempty"`
}

type httpReqBSONReader struct {
	Method     Method            `bson:"method"`
	ProtoMajor int               `bson:"protomajor"`
	ProtoMinor int               `bson:"protominor"`
	URL        string            `bson:"url"`
	URLParams  map[string]string `bson:"urlparams"`
	Header     map[string]string `bson:"header"`
	Body       string            `bson:"body"`
	BodyRef    BodyRef           `bson:"bodyref"`
	Binary     string            `bson:"binary"`
	Form       []FormData        `bson:"form"`
	Timestamp  bson.RawValue     `bson:"timestamp"`

	HeaderLineLengths HeaderLineLengths `bson:"headerlinelengths,omitempty"`
}

// MarshalBSON writes HTTPReq with the Timestamp field serialised as an
// RFC3339Nano string so BSON DateTime's millisecond resolution cannot truncate
// it.
func (h HTTPReq) MarshalBSON() ([]byte, error) {
	return bson.Marshal(httpReqBSON{
		Method:     h.Method,
		ProtoMajor: h.ProtoMajor,
		ProtoMinor: h.ProtoMinor,
		URL:        h.URL,
		URLParams:  h.URLParams,
		Header:     h.Header,
		Body:       h.Body,
		BodyRef:    h.BodyRef,
		Binary:     h.Binary,
		Form:       h.Form,
		Timestamp:  FormatMockTimestamp(h.Timestamp),

		HeaderLineLengths: h.HeaderLineLengths,
	})
}

// UnmarshalBSON reads HTTPReq, accepting either the new RFC3339Nano string
// timestamp or the legacy BSON DateTime (millisecond) shape for backward
// compatibility with records written before MarshalBSON landed.
func (h *HTTPReq) UnmarshalBSON(data []byte) error {
	var raw httpReqBSONReader
	if err := bson.Unmarshal(data, &raw); err != nil {
		return err
	}

	ts, err := decodeBSONTimestamp(raw.Timestamp)
	if err != nil {
		return fmt.Errorf("HTTPReq.UnmarshalBSON timestamp: %w", err)
	}

	*h = HTTPReq{
		Method:     raw.Method,
		ProtoMajor: raw.ProtoMajor,
		ProtoMinor: raw.ProtoMinor,
		URL:        raw.URL,
		URLParams:  raw.URLParams,
		Header:     raw.Header,
		Body:       raw.Body,
		BodyRef:    raw.BodyRef,
		Binary:     raw.Binary,
		Form:       raw.Form,
		Timestamp:  ts,

		HeaderLineLengths: raw.HeaderLineLengths,
	}
	return nil
}

type httpRespBSON struct {
	StatusCode    int               `bson:"statuscode"`
	Header        map[string]string `bson:"header"`
	Body          string            `bson:"body"`
	StreamBody    []HTTPStreamChunk `bson:"streambody"`
	BodySkipped   bool              `bson:"bodyskipped"`
	BodySize      int64             `bson:"bodysize"`
	StatusMessage string            `bson:"statusmessage"`
	ProtoMajor    int               `bson:"protomajor"`
	ProtoMinor    int               `bson:"protominor"`
	Binary        string            `bson:"binary"`
	Timestamp     string            `bson:"timestamp"`

	HeaderLineLengths HeaderLineLengths `bson:"headerlinelengths,omitempty"`
}

type httpRespBSONReader struct {
	StatusCode    int               `bson:"statuscode"`
	Header        map[string]string `bson:"header"`
	Body          string            `bson:"body"`
	StreamBody    []HTTPStreamChunk `bson:"streambody"`
	BodySkipped   bool              `bson:"bodyskipped"`
	BodySize      int64             `bson:"bodysize"`
	StatusMessage string            `bson:"statusmessage"`
	ProtoMajor    int               `bson:"protomajor"`
	ProtoMinor    int               `bson:"protominor"`
	Binary        string            `bson:"binary"`
	Timestamp     bson.RawValue     `bson:"timestamp"`

	HeaderLineLengths HeaderLineLengths `bson:"headerlinelengths,omitempty"`
}

// MarshalBSON — see the HTTPReq version above. Same rationale.
func (h HTTPResp) MarshalBSON() ([]byte, error) {
	return bson.Marshal(httpRespBSON{
		StatusCode:    h.StatusCode,
		Header:        h.Header,
		Body:          h.Body,
		StreamBody:    h.StreamBody,
		BodySkipped:   h.BodySkipped,
		BodySize:      h.BodySize,
		StatusMessage: h.StatusMessage,
		ProtoMajor:    h.ProtoMajor,
		ProtoMinor:    h.ProtoMinor,
		Binary:        h.Binary,
		Timestamp:     FormatMockTimestamp(h.Timestamp),

		HeaderLineLengths: h.HeaderLineLengths,
	})
}

func (h *HTTPResp) UnmarshalBSON(data []byte) error {
	var raw httpRespBSONReader
	if err := bson.Unmarshal(data, &raw); err != nil {
		return err
	}

	ts, err := decodeBSONTimestamp(raw.Timestamp)
	if err != nil {
		return fmt.Errorf("HTTPResp.UnmarshalBSON timestamp: %w", err)
	}

	*h = HTTPResp{
		StatusCode:    raw.StatusCode,
		Header:        raw.Header,
		Body:          raw.Body,
		StreamBody:    raw.StreamBody,
		BodySkipped:   raw.BodySkipped,
		BodySize:      raw.BodySize,
		StatusMessage: raw.StatusMessage,
		ProtoMajor:    raw.ProtoMajor,
		ProtoMinor:    raw.ProtoMinor,
		Binary:        raw.Binary,
		Timestamp:     ts,

		HeaderLineLengths: raw.HeaderLineLengths,
	}
	return nil
}

// decodeBSONTimestamp handles the two on-disk shapes for HTTP timestamps:
// the new RFC3339Nano string (preserves nanoseconds) and the legacy BSON
// DateTime (millisecond int64). An absent/null value decodes to the zero time.
func decodeBSONTimestamp(v bson.RawValue) (time.Time, error) {
	switch v.Type {
	case 0, bson.TypeNull, bson.TypeUndefined:
		return time.Time{}, nil
	case bson.TypeString:
		s, ok := v.StringValueOK()
		if !ok {
			return time.Time{}, fmt.Errorf("expected string, got malformed value")
		}
		return ParseMockTimestamp(s)
	case bson.TypeDateTime:
		dt, ok := v.DateTimeOK()
		if !ok {
			return time.Time{}, fmt.Errorf("expected DateTime, got malformed value")
		}
		return bson.DateTime(dt).Time(), nil
	default:
		return time.Time{}, fmt.Errorf("unsupported BSON type %v for timestamp", v.Type)
	}
}
