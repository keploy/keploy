package models

import (
	"encoding/base64"
	"fmt"
	"time"
	"unicode/utf8"

	"go.mongodb.org/mongo-driver/v2/bson"
	yamlLib "gopkg.in/yaml.v3"
)

type Method string

// splitBodyForYAML returns the pair that should be written to the yaml
// document: (body, body_base64). If the body is valid UTF-8 it goes in the
// first slot and body_base64 is empty. If it contains any non-UTF-8 bytes
// (e.g. a zip / image / octet-stream response) yaml.v3 rejects it as
// "yaml: cannot marshal invalid UTF-8 data as !!str", so we base64-encode
// and leave the plain body empty.
func splitBodyForYAML(body string) (string, string) {
	if body == "" || utf8.ValidString(body) {
		return body, ""
	}
	return "", base64.StdEncoding.EncodeToString([]byte(body))
}

// joinBodyFromYAML is the inverse of splitBodyForYAML. If body_base64 is
// present it wins and is decoded back into the raw byte-for-byte body.
func joinBodyFromYAML(body, bodyBase64 string) (string, error) {
	if bodyBase64 == "" {
		return body, nil
	}
	decoded, err := base64.StdEncoding.DecodeString(bodyBase64)
	if err != nil {
		return "", fmt.Errorf("decode body_base64: %w", err)
	}
	return string(decoded), nil
}

// MarshalYAML serialises an HTTPReq. When the body contains non-UTF-8 bytes
// (which yaml.v3 refuses to emit as a !!str scalar) it is moved to a
// sibling body_base64 field. See splitBodyForYAML for the rationale.
func (h HTTPReq) MarshalYAML() (interface{}, error) {
	body, bodyB64 := splitBodyForYAML(h.Body)
	type httpReqYAML struct {
		Method     Method            `yaml:"method"`
		ProtoMajor int               `yaml:"proto_major"`
		ProtoMinor int               `yaml:"proto_minor"`
		URL        string            `yaml:"url"`
		URLParams  map[string]string `yaml:"url_params,omitempty"`
		Header     map[string]string `yaml:"header"`
		Body       string            `yaml:"body"`
		BodyBase64 string            `yaml:"body_base64,omitempty"`
		BodyRef    BodyRef           `yaml:"body_ref,omitempty"`
		Binary     string            `yaml:"binary,omitempty"`
		Form       []FormData        `yaml:"form,omitempty"`
		Timestamp  time.Time         `yaml:"timestamp"`
	}
	return httpReqYAML{
		Method:     h.Method,
		ProtoMajor: h.ProtoMajor,
		ProtoMinor: h.ProtoMinor,
		URL:        h.URL,
		URLParams:  h.URLParams,
		Header:     h.Header,
		Body:       body,
		BodyBase64: bodyB64,
		BodyRef:    h.BodyRef,
		Binary:     h.Binary,
		Form:       h.Form,
		Timestamp:  h.Timestamp,
	}, nil
}

func (h *HTTPReq) UnmarshalYAML(node *yamlLib.Node) error {
	type httpReqYAML struct {
		Method     Method            `yaml:"method"`
		ProtoMajor int               `yaml:"proto_major"`
		ProtoMinor int               `yaml:"proto_minor"`
		URL        string            `yaml:"url"`
		URLParams  map[string]string `yaml:"url_params,omitempty"`
		Header     map[string]string `yaml:"header"`
		Body       string            `yaml:"body"`
		BodyBase64 string            `yaml:"body_base64,omitempty"`
		BodyRef    BodyRef           `yaml:"body_ref,omitempty"`
		Binary     string            `yaml:"binary,omitempty"`
		Form       []FormData        `yaml:"form,omitempty"`
		Timestamp  time.Time         `yaml:"timestamp"`
	}
	var raw httpReqYAML
	if err := node.Decode(&raw); err != nil {
		return err
	}
	body, err := joinBodyFromYAML(raw.Body, raw.BodyBase64)
	if err != nil {
		return err
	}
	*h = HTTPReq{
		Method:     raw.Method,
		ProtoMajor: raw.ProtoMajor,
		ProtoMinor: raw.ProtoMinor,
		URL:        raw.URL,
		URLParams:  raw.URLParams,
		Header:     raw.Header,
		Body:       body,
		BodyRef:    raw.BodyRef,
		Binary:     raw.Binary,
		Form:       raw.Form,
		Timestamp:  raw.Timestamp,
	}
	return nil
}

// BodyRef stores a reference to a large request body that has been offloaded
// to the assets directory (bodies > 1MB). When BodyRef is set, Body will be empty.
type BodyRef struct {
	Path string `json:"path" yaml:"path"` // relative path to the asset file
	Size int64  `json:"size" yaml:"size"` // original content size in bytes
}

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
}

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
