package models

import (
	"fmt"
	"strings"
	"time"

	yamlLib "gopkg.in/yaml.v3"
)

type Method string

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
	StreamBody    []StreamBodyEntry `json:"-" yaml:"-"`                                           // structured streaming body; serialized as body in YAML
	BodySkipped   bool              `json:"body_skipped,omitempty" yaml:"body_skipped,omitempty"` // true when body was >1MB and not saved
	BodySize      int64             `json:"body_size,omitempty" yaml:"body_size,omitempty"`       // original body size in bytes when BodySkipped is true
	StatusMessage string            `json:"status_message" yaml:"status_message"`
	ProtoMajor    int               `json:"proto_major" yaml:"proto_major"`
	ProtoMinor    int               `json:"proto_minor" yaml:"proto_minor"`
	Binary        string            `json:"binary" yaml:"binary,omitempty"`
	Timestamp     time.Time         `json:"timestamp" yaml:"timestamp"`
}

// StreamBodyEntry represents a single entry in a structured streaming response body.
// For SSE, Data contains keys like "comment", "id", "event", "data", "retry".
// For HTTP streaming, Data contains a "raw" key with the chunk content.
type StreamBodyEntry struct {
	Ts   string            `json:"ts" yaml:"ts"`
	Data map[string]string `json:"data" yaml:"data"`
}

// MarshalYAML implements yaml.Marshaler for HTTPResp.
// When StreamBody is populated, the body field is serialized as a YAML sequence
// of StreamBodyEntry instead of a plain string.
func (r HTTPResp) MarshalYAML() (interface{}, error) {
	if len(r.StreamBody) > 0 {
		return &httpRespStreamYAML{
			StatusCode:    r.StatusCode,
			Header:        r.Header,
			Body:          r.StreamBody,
			BodySkipped:   r.BodySkipped,
			BodySize:      r.BodySize,
			StatusMessage: r.StatusMessage,
			ProtoMajor:    r.ProtoMajor,
			ProtoMinor:    r.ProtoMinor,
			Binary:        r.Binary,
			Timestamp:     r.Timestamp,
		}, nil
	}
	type plain HTTPResp
	p := plain(r)
	return &p, nil
}

// UnmarshalYAML implements yaml.Unmarshaler for HTTPResp.
// It handles both the legacy format (body as string) and the new structured
// format (body as []StreamBodyEntry).
func (r *HTTPResp) UnmarshalYAML(value *yamlLib.Node) error {
	if value.Kind != yamlLib.MappingNode {
		return fmt.Errorf("expected mapping node for HTTPResp, got kind %d", value.Kind)
	}

	// Check if the body field is a sequence (structured stream) or a scalar (string).
	bodyIsSequence := false
	for i := 0; i+1 < len(value.Content); i += 2 {
		if value.Content[i].Value == "body" && value.Content[i+1].Kind == yamlLib.SequenceNode {
			bodyIsSequence = true
			break
		}
	}

	if !bodyIsSequence {
		// Standard case: body is a string (or absent). Decode directly.
		type plain HTTPResp
		var p plain
		if err := value.Decode(&p); err != nil {
			return err
		}
		*r = HTTPResp(p)
		return nil
	}

	// Body is a sequence: decode everything except body using the raw struct,
	// then decode the body sequence separately.
	var raw httpRespRawYAML
	if err := value.Decode(&raw); err != nil {
		return err
	}
	r.StatusCode = raw.StatusCode
	r.Header = raw.Header
	r.BodySkipped = raw.BodySkipped
	r.BodySize = raw.BodySize
	r.StatusMessage = raw.StatusMessage
	r.ProtoMajor = raw.ProtoMajor
	r.ProtoMinor = raw.ProtoMinor
	r.Binary = raw.Binary
	r.Timestamp = raw.Timestamp

	// Decode the body sequence node
	for i := 0; i+1 < len(value.Content); i += 2 {
		if value.Content[i].Value == "body" && value.Content[i+1].Kind == yamlLib.SequenceNode {
			var entries []StreamBodyEntry
			if err := value.Content[i+1].Decode(&entries); err == nil && len(entries) > 0 {
				r.StreamBody = entries
				r.Body = ReconstructBodyFromStreamEntries(entries, r.Header)
			}
			break
		}
	}

	return nil
}

// httpRespStreamYAML is the YAML-serialization shape when StreamBody is present.
type httpRespStreamYAML struct {
	StatusCode    int               `yaml:"status_code"`
	Header        map[string]string `yaml:"header"`
	Body          []StreamBodyEntry `yaml:"body"`
	BodySkipped   bool              `yaml:"body_skipped,omitempty"`
	BodySize      int64             `yaml:"body_size,omitempty"`
	StatusMessage string            `yaml:"status_message"`
	ProtoMajor    int               `yaml:"proto_major"`
	ProtoMinor    int               `yaml:"proto_minor"`
	Binary        string            `yaml:"binary,omitempty"`
	Timestamp     time.Time         `yaml:"timestamp"`
}

// httpRespRawYAML is used for flexible YAML unmarshaling that skips the body field
// (which may be a sequence that can't decode into string).
type httpRespRawYAML struct {
	StatusCode    int               `yaml:"status_code"`
	Header        map[string]string `yaml:"header"`
	Body          interface{}       `yaml:"body,omitempty"` // string or []interface{} depending on YAML content
	BodySkipped   bool              `yaml:"body_skipped,omitempty"`
	BodySize      int64             `yaml:"body_size,omitempty"`
	StatusMessage string            `yaml:"status_message"`
	ProtoMajor    int               `yaml:"proto_major"`
	ProtoMinor    int               `yaml:"proto_minor"`
	Binary        string            `yaml:"binary,omitempty"`
	Timestamp     time.Time         `yaml:"timestamp"`
}

// ReconstructBodyFromStreamEntries rebuilds the raw response body string from
// structured StreamBodyEntry slices. The reconstruction format depends on the
// Content-Type header: SSE for text/event-stream, raw-line joining for everything else.
func ReconstructBodyFromStreamEntries(entries []StreamBodyEntry, headers map[string]string) string {
	if len(entries) == 0 {
		return ""
	}
	contentType := headerValueCaseInsensitive(headers, "Content-Type")
	contentTypeLower := strings.ToLower(contentType)

	if strings.Contains(contentTypeLower, "text/event-stream") {
		return reconstructSSEBody(entries)
	}
	// For all other streaming types (NDJSON, plain-text, binary, etc.), use raw
	return reconstructHTTPStreamBody(entries)
}

// reconstructSSEBody rebuilds a raw SSE body from structured entries.
func reconstructSSEBody(entries []StreamBodyEntry) string {
	var sb strings.Builder
	for i, entry := range entries {
		if i > 0 {
			sb.WriteString("\n")
		}
		data := entry.Data
		if comment, ok := data["comment"]; ok {
			sb.WriteString(": ")
			sb.WriteString(comment)
			sb.WriteString("\n")
			continue
		}
		// Reconstruct non-comment SSE frame: id, retry, event, data
		if id, ok := data["id"]; ok {
			sb.WriteString("id:")
			sb.WriteString(id)
			sb.WriteString("\n")
		}
		if retry, ok := data["retry"]; ok {
			sb.WriteString("retry:")
			sb.WriteString(retry)
			sb.WriteString("\n")
		}
		if event, ok := data["event"]; ok {
			sb.WriteString("event:")
			sb.WriteString(event)
			sb.WriteString("\n")
		}
		if dataVal, ok := data["data"]; ok {
			// Multi-line data: each line gets a data: prefix
			lines := strings.Split(dataVal, "\n")
			for _, line := range lines {
				sb.WriteString("data:")
				sb.WriteString(line)
				sb.WriteString("\n")
			}
		}
	}
	return sb.String()
}

// reconstructHTTPStreamBody rebuilds a raw HTTP streaming body from structured entries.
func reconstructHTTPStreamBody(entries []StreamBodyEntry) string {
	var sb strings.Builder
	for i, entry := range entries {
		if i > 0 {
			sb.WriteString("\n")
		}
		if raw, ok := entry.Data["raw"]; ok {
			sb.WriteString(raw)
		}
	}
	if sb.Len() > 0 {
		sb.WriteString("\n")
	}
	return sb.String()
}

func headerValueCaseInsensitive(headers map[string]string, key string) string {
	key = strings.ToLower(strings.TrimSpace(key))
	for k, v := range headers {
		if strings.ToLower(strings.TrimSpace(k)) == key {
			return v
		}
	}
	return ""
}
