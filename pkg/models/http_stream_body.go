package models

import (
	"bufio"
	"encoding/json"
	"strings"
	"time"

	yamlLib "gopkg.in/yaml.v3"
)

// maxModelStreamTokenSize caps the maximum size of a single streaming token (10 MB)
// to prevent runaway memory allocation when scanning large streaming responses.
const maxModelStreamTokenSize = 10 * 1024 * 1024

// HTTPStreamDataField represents a single key-value field within a streaming chunk.
//
// For SSE responses, each field maps to an SSE line:
//   - key="data",    value=<event payload>
//   - key="event",   value=<event type name>
//   - key="id",      value=<event ID>
//   - key="retry",   value=<reconnect delay (ms)>
//   - key="comment", value=<comment text after ':'>
//
// For other stream formats (NDJSON, plain text, binary), every chunk
// has exactly one field with key="raw" and value=<the raw line or payload>.
type HTTPStreamDataField struct {
	Key   string
	Value string
}

// HTTPStreamChunk represents one logical unit of a streaming response body.
//
// For SSE this is one complete SSE "frame" (aka event, delimited by blank lines).
// For NDJSON and plain-text streams this is one newline-delimited line.
// For binary streams this is the full captured body.
//
// TS is the wall-clock time the chunk was received during recording,
// used by the replay engine to preserve the inter-chunk timing when replaying.
type HTTPStreamChunk struct {
	TS   time.Time             // Wall-clock time this chunk arrived during recording
	Data []HTTPStreamDataField // Key-value fields that make up this chunk
}

// ValueByKey looks up the value of the field with the given key (case-insensitive).
// Returns ("", false) if no such field exists.
func (c HTTPStreamChunk) ValueByKey(key string) (string, bool) {
	key = strings.ToLower(strings.TrimSpace(key))
	for _, field := range c.Data {
		if strings.ToLower(strings.TrimSpace(field.Key)) == key {
			return field.Value, true
		}
	}
	return "", false
}

// ── YAML Serialization ─────────────────────────────────────────────────────────
//
// HTTPResp can be serialized to YAML in two modes:
//
//  1. FLAT (plain string): used for normal, non-streaming HTTP responses.
//     body: '{"id":1}'
//
//  2. STRUCTURED (YAML sequence): used for streaming responses (SSE, NDJSON, etc.).
//     Each element in the sequence is one chunk (frame/line), storing the
//     timestamp and the key-value fields that make up that chunk.
//
//     body:
//       - ts: "2024-01-01T00:00:01Z"
//         data:
//           event: message
//           data: '{"id":1}'
//       - ts: "2024-01-01T00:00:02Z"
//         data:
//           event: message
//           data: '{"id":2}'
//
// MarshalYAML and UnmarshalYAML implement this two-mode logic.

// MarshalYAML serializes an HTTPResp to YAML.
// If the response has streaming chunks (StreamBody), the body field is written
// as a YAML sequence of chunks. Otherwise, it is written as a plain scalar string.
func (h HTTPResp) MarshalYAML() (interface{}, error) {
	bodyNode := &yamlLib.Node{}
	if chunks, ok := streamChunksForYAML(h); ok {
		// Streaming response — encode as a YAML sequence of chunks
		bodyNode = encodeStreamChunksNode(chunks)
	} else {
		// Non-streaming response — encode body as a plain string scalar
		if err := bodyNode.Encode(h.Body); err != nil {
			return nil, err
		}
	}

	type httpRespYAML struct {
		StatusCode    int               `yaml:"status_code"`
		Header        map[string]string `yaml:"header"`
		Body          *yamlLib.Node     `yaml:"body"`
		BodySkipped   bool              `yaml:"body_skipped,omitempty"`
		BodySize      int64             `yaml:"body_size,omitempty"`
		StatusMessage string            `yaml:"status_message"`
		ProtoMajor    int               `yaml:"proto_major"`
		ProtoMinor    int               `yaml:"proto_minor"`
		Binary        string            `yaml:"binary,omitempty"`
		Timestamp     time.Time         `yaml:"timestamp"`
	}

	return httpRespYAML{
		StatusCode:    h.StatusCode,
		Header:        h.Header,
		Body:          bodyNode,
		BodySkipped:   h.BodySkipped,
		BodySize:      h.BodySize,
		StatusMessage: h.StatusMessage,
		ProtoMajor:    h.ProtoMajor,
		ProtoMinor:    h.ProtoMinor,
		Binary:        h.Binary,
		Timestamp:     h.Timestamp,
	}, nil
}

// UnmarshalYAML deserializes an HTTPResp from YAML.
// The body field can be either a plain string (non-streaming)
// or a YAML sequence of chunks (streaming). Both formats are handled.
func (h *HTTPResp) UnmarshalYAML(node *yamlLib.Node) error {
	type httpRespYAML struct {
		StatusCode    int               `yaml:"status_code"`
		Header        map[string]string `yaml:"header"`
		Body          yamlLib.Node      `yaml:"body"`
		BodySkipped   bool              `yaml:"body_skipped,omitempty"`
		BodySize      int64             `yaml:"body_size,omitempty"`
		StatusMessage string            `yaml:"status_message"`
		ProtoMajor    int               `yaml:"proto_major"`
		ProtoMinor    int               `yaml:"proto_minor"`
		Binary        string            `yaml:"binary,omitempty"`
		Timestamp     time.Time         `yaml:"timestamp"`
	}

	var raw httpRespYAML
	if err := node.Decode(&raw); err != nil {
		return err
	}

	h.StatusCode = raw.StatusCode
	h.Header = raw.Header
	h.BodySkipped = raw.BodySkipped
	h.BodySize = raw.BodySize
	h.StatusMessage = raw.StatusMessage
	h.ProtoMajor = raw.ProtoMajor
	h.ProtoMinor = raw.ProtoMinor
	h.Binary = raw.Binary
	h.Timestamp = raw.Timestamp

	// Decode body — may produce a plain string body, streaming chunks, or both.
	// StreamBody is populated only when the YAML body field was a sequence node.
	body, streamChunks, err := decodeStreamBody(raw.Body, raw.Header, raw.Timestamp)
	if err != nil {
		return err
	}
	h.Body = body
	h.StreamBody = streamChunks

	return nil
}

// ── Stream body kind detection ─────────────────────────────────────────────────

// streamBodyKind identifies what format a streaming response body uses.
// This is used to decide how to reconstruct the flat Body string from
// structured StreamBody chunks (see streamChunksToLegacyBody).
type streamBodyKind string

const (
	streamBodyKindUnknown streamBodyKind = ""    // Unknown / non-streaming
	streamBodyKindSSE     streamBodyKind = "sse" // Server-Sent Events (text/event-stream)
	streamBodyKindRaw     streamBodyKind = "raw" // All other streaming formats (NDJSON, plain text, binary)
)

// streamChunksForYAML returns the chunks to encode if this response has a streaming body.
// Returns (nil, false) for non-streaming responses.
func streamChunksForYAML(resp HTTPResp) ([]HTTPStreamChunk, bool) {
	if len(resp.StreamBody) > 0 {
		return cloneStreamChunks(resp.StreamBody), true
	}

	if resp.Body != "" {
		kind := detectStreamBodyKind(resp.Header)
		if kind == streamBodyKindSSE {
			chunks := parseSSEBodyToChunks(resp.Body, resp.Timestamp)
			if len(chunks) > 0 {
				return chunks, true
			}
		} else if kind == streamBodyKindRaw {
			contentType := strings.ToLower(getHeaderValueCaseInsensitiveModel(resp.Header, "Content-Type"))
			isNDJSON := strings.Contains(contentType, "application/x-ndjson") || strings.Contains(contentType, "application/ndjson")
			isTextPlain := strings.Contains(contentType, "text/plain")
			isOctetStream := strings.Contains(contentType, "application/octet-stream")

			if isOctetStream {
				if strings.TrimSpace(resp.Body) == "" {
					return nil, false
				}
				return []HTTPStreamChunk{{
					TS: resp.Timestamp,
					Data: []HTTPStreamDataField{{
						Key:   "raw",
						Value: resp.Body,
					}},
				}}, true
			}

			hasContentLength := getHeaderValueCaseInsensitiveModel(resp.Header, "Content-Length") != ""
			isChunked := strings.Contains(strings.ToLower(getHeaderValueCaseInsensitiveModel(resp.Header, "Transfer-Encoding")), "chunked")

			// Only safely auto-chunk known streaming structured formats to avoid false-positives on plain text files.
			shouldChunk := isNDJSON
			if isTextPlain {
				// For text/plain, it is streaming if Transfer-Encoding is chunked or Content-Length is absent.
				if isChunked || !hasContentLength {
					shouldChunk = true
				} else {
					shouldChunk = false
				}
			}

			if shouldChunk {
				chunks := parseRawBodyToChunks(resp.Body, resp.Timestamp, isNDJSON)
				if len(chunks) > 0 {
					return chunks, true
				}
			}
		}
	}

	return nil, false
}

// detectStreamBodyKind inspects the Content-Type header to decide
// which streaming format the body uses. This determines how flat body strings
// are reconstructed from structured chunks.
func detectStreamBodyKind(headers map[string]string) streamBodyKind {
	contentType := strings.ToLower(getHeaderValueCaseInsensitiveModel(headers, "Content-Type"))
	if strings.Contains(contentType, "text/event-stream") {
		return streamBodyKindSSE
	}
	if strings.Contains(contentType, "application/x-ndjson") ||
		strings.Contains(contentType, "application/ndjson") ||
		strings.Contains(contentType, "text/plain") ||
		strings.Contains(contentType, "application/octet-stream") {
		return streamBodyKindRaw
	}
	return streamBodyKindUnknown
}

// ── YAML body decode/encode ────────────────────────────────────────────────────

// decodeStreamBody parses the YAML body node and returns:
//   - body: the flat string representation (used by the HTTP matcher).
//   - streamChunks: the structured chunk list (used for chunk-by-chunk comparison
//     and for re-encoding back to YAML). Nil for non-streaming responses.
//
// If the YAML node is a sequence, it's a structured streaming body.
// If it's a scalar, it's a plain string body.
func decodeStreamBody(node yamlLib.Node, headers map[string]string, respTimestamp time.Time) (string, []HTTPStreamChunk, error) {
	if node.Kind == 0 {
		// Empty body node — nothing to decode.
		return "", nil, nil
	}

	if node.Kind == yamlLib.SequenceNode {
		// Structured streaming body: decode the sequence into chunks,
		// then reconstruct the legacy flat Body string so that the rest
		// of the code that reads h.Body still works correctly.
		chunks, err := decodeStreamChunks(node)
		if err != nil {
			return "", nil, err
		}
		flatBody := streamChunksToLegacyBody(chunks, detectStreamBodyKind(headers))
		return flatBody, chunks, nil
	}

	// Plain string body (non-streaming).
	var body string
	if err := node.Decode(&body); err != nil {
		return "", nil, err
	}
	return body, nil, nil
}

// decodeStreamChunks parses a YAML sequence node into a list of HTTPStreamChunk.
// Each element of the sequence is a YAML mapping with "ts" and "data" keys.
func decodeStreamChunks(seqNode yamlLib.Node) ([]HTTPStreamChunk, error) {
	chunks := make([]HTTPStreamChunk, 0, len(seqNode.Content))
	for _, chunkNode := range seqNode.Content {
		if chunkNode == nil || chunkNode.Kind != yamlLib.MappingNode {
			continue
		}
		chunk := HTTPStreamChunk{}

		// Iterate over the mapping key-value pairs (YAML mapping nodes store
		// alternating key nodes and value nodes in Content).
		for i := 0; i+1 < len(chunkNode.Content); i += 2 {
			keyNode := chunkNode.Content[i]
			valueNode := chunkNode.Content[i+1]
			if keyNode == nil || valueNode == nil {
				continue
			}
			switch strings.ToLower(strings.TrimSpace(keyNode.Value)) {
			case "ts":
				if ts := parseStreamChunkTime(valueNode.Value); !ts.IsZero() {
					chunk.TS = ts
				}
			case "data":
				fields, err := decodeStreamDataFields(valueNode)
				if err != nil {
					return nil, err
				}
				chunk.Data = fields
			}
		}
		chunks = append(chunks, chunk)
	}
	return chunks, nil
}

// decodeStreamDataFields parses the "data" value node of a chunk into key-value fields.
//
// If the data node is a YAML mapping, each entry becomes one HTTPStreamDataField.
// If it's a scalar (e.g. a plain line of text), it's wrapped in a single
// field with key="raw".
func decodeStreamDataFields(dataNode *yamlLib.Node) ([]HTTPStreamDataField, error) {
	if dataNode == nil {
		return nil, nil
	}
	if dataNode.Kind != yamlLib.MappingNode {
		// Scalar or sequence — wrap as a raw field.
		return []HTTPStreamDataField{{
			Key:   "raw",
			Value: yamlNodeToString(dataNode),
		}}, nil
	}

	fields := make([]HTTPStreamDataField, 0, len(dataNode.Content)/2)
	for i := 0; i+1 < len(dataNode.Content); i += 2 {
		keyNode := dataNode.Content[i]
		valueNode := dataNode.Content[i+1]
		if keyNode == nil || valueNode == nil {
			continue
		}
		fields = append(fields, HTTPStreamDataField{
			Key:   strings.TrimSpace(keyNode.Value),
			Value: yamlNodeToString(valueNode),
		})
	}
	return fields, nil
}

// encodeStreamChunksNode converts a list of HTTPStreamChunk into a YAML sequence node
// for serialization. Each chunk becomes a YAML mapping with "ts" and "data" keys.
func encodeStreamChunksNode(chunks []HTTPStreamChunk) *yamlLib.Node {
	root := &yamlLib.Node{Kind: yamlLib.SequenceNode, Tag: "!!seq"}
	for _, chunk := range chunks {
		chunkNode := &yamlLib.Node{Kind: yamlLib.MappingNode, Tag: "!!map"}

		if !chunk.TS.IsZero() {
			chunkNode.Content = append(chunkNode.Content,
				stringNode("ts"),
				stringNode(chunk.TS.UTC().Format(time.RFC3339Nano)),
			)
		}

		// Encode each key-value field under the "data" mapping.
		dataNode := &yamlLib.Node{Kind: yamlLib.MappingNode, Tag: "!!map"}
		for _, field := range chunk.Data {
			dataNode.Content = append(dataNode.Content, stringNode(field.Key), stringNode(field.Value))
		}
		chunkNode.Content = append(chunkNode.Content, stringNode("data"), dataNode)
		root.Content = append(root.Content, chunkNode)
	}
	return root
}

// ── Flat body reconstruction ──────────────────────────────────────────────────

// streamChunksToLegacyBody reconstructs the flat Body string from structured chunks.
//
// This is used when loading a test case from YAML so that the rest of the code
// (which reads HTTPResp.Body as a single string) still works as expected,
// even though the YAML file now stores structured streaming data.
//
// For SSE bodies, each chunk becomes an SSE frame (fields joined by \n,
// frames joined by \n\n, matching the SSE wire format).
//
// For all other streaming formats (NDJSON, plain text, binary), each chunk's
// "raw" field value becomes one line.
func streamChunksToLegacyBody(chunks []HTTPStreamChunk, kind streamBodyKind) string {
	if len(chunks) == 0 {
		return ""
	}

	switch kind {
	case streamBodyKindSSE:
		// Reconstruct SSE wire format: each frame is a block of "key:value" lines
		// separated by \n, and frames are separated by \n\n.
		frames := make([]string, 0, len(chunks))
		for _, chunk := range chunks {
			lines := make([]string, 0, len(chunk.Data))
			for _, field := range chunk.Data {
				key := strings.TrimSpace(field.Key)
				if strings.EqualFold(key, "comment") {
					// SSE comments start with ':' and have no key prefix.
					lines = append(lines, ":"+field.Value)
					continue
				}
				if key == "" {
					continue
				}
				lines = append(lines, strings.ToLower(key)+":"+field.Value)
			}
			if len(lines) == 0 {
				continue
			}
			frames = append(frames, strings.Join(lines, "\n"))
		}
		return strings.Join(frames, "\n\n")

	default:
		// NDJSON, plain text, binary — each chunk contributes one line via its "raw" field.
		lines := make([]string, 0, len(chunks))
		for _, chunk := range chunks {
			if raw, ok := chunk.ValueByKey("raw"); ok {
				lines = append(lines, raw)
				continue
			}
			// Fallback: use the first field value if no "raw" key exists.
			if len(chunk.Data) > 0 {
				lines = append(lines, chunk.Data[0].Value)
			}
		}
		return strings.Join(lines, "\n")
	}
}

// ── Body → Chunk parsers ───────────────────────────────────────────────────────
// These are used during recording to convert a raw HTTP response body string into
// structured HTTPStreamChunk slices for storage in the test case YAML.

// parseSSEBodyToChunks splits a raw SSE body string into HTTPStreamChunk slices.
//
// SSE frames are delimited by blank lines (\n\n). Within each frame, each line
// is a "key: value" pair. Consecutive "data" lines are merged per the SSE spec.
// Comment lines (starting with ':') are stored with key="comment".
func parseSSEBodyToChunks(body string, ts time.Time) []HTTPStreamChunk {
	body = normalizeModelLineEndings(body)
	frames := splitModelSSEFrames(body)

	chunks := make([]HTTPStreamChunk, 0, len(frames))
	for _, frame := range frames {
		lines := strings.Split(frame, "\n")
		fields := make([]HTTPStreamDataField, 0, len(lines))

		for _, line := range lines {
			line = strings.TrimRight(line, "\r")
			if strings.TrimSpace(line) == "" {
				continue
			}

			// SSE comment line: starts with ':'
			if strings.HasPrefix(line, ":") {
				fields = append(fields, HTTPStreamDataField{
					Key:   "comment",
					Value: strings.TrimPrefix(line, ":"),
				})
				continue
			}

			// Regular SSE field: "key: value" or just "key" (value is empty string).
			key, value, ok := strings.Cut(line, ":")
			if !ok {
				key = strings.TrimSpace(line)
				value = ""
			} else if strings.HasPrefix(value, " ") {
				// Per SSE spec, one optional leading space after ':' is stripped.
				value = value[1:]
			}

			if key == "" {
				continue
			}

			normalizedKey := strings.ToLower(strings.TrimSpace(key))

			// Per SSE spec, consecutive "data" lines in the same frame
			// are concatenated with a '\n' separator.
			if normalizedKey == "data" && len(fields) > 0 && strings.EqualFold(fields[len(fields)-1].Key, "data") {
				fields[len(fields)-1].Value = fields[len(fields)-1].Value + "\n" + value
				continue
			}

			fields = append(fields, HTTPStreamDataField{
				Key:   normalizedKey,
				Value: value,
			})
		}

		if len(fields) == 0 {
			continue
		}
		chunks = append(chunks, HTTPStreamChunk{
			TS:   ts,
			Data: fields,
		})
	}
	return chunks
}

// parseRawBodyToChunks splits a raw body string into HTTPStreamChunk slices
// where each line becomes one chunk with a single "raw" field.
//
// Used for NDJSON, plain text, and binary streaming formats.
// If ignoreEmpty is true, blank lines are skipped (useful for NDJSON which
// can have empty separator lines between JSON objects).
func parseRawBodyToChunks(body string, ts time.Time, ignoreEmpty bool) []HTTPStreamChunk {
	body = normalizeModelLineEndings(body)
	if body == "" {
		return nil
	}

	lines := make([]string, 0)
	scanner := bufio.NewScanner(strings.NewReader(body))
	scanner.Buffer(make([]byte, 0, 64*1024), maxModelStreamTokenSize)
	for scanner.Scan() {
		line := scanner.Text()
		if ignoreEmpty && strings.TrimSpace(line) == "" {
			continue
		}
		lines = append(lines, line)
	}

	if err := scanner.Err(); err != nil {
		// Scanner error — fall back to treating the whole body as one chunk.
		lines = nil
	}

	if len(lines) == 0 {
		// No lines found — treat the entire body as a single raw chunk.
		lines = append(lines, body)
	}

	chunks := make([]HTTPStreamChunk, 0, len(lines))
	for _, line := range lines {
		chunks = append(chunks, HTTPStreamChunk{
			TS: ts,
			Data: []HTTPStreamDataField{{
				Key:   "raw",
				Value: line,
			}},
		})
	}
	return chunks
}

// ── Helpers ───────────────────────────────────────────────────────────────────

// splitModelSSEFrames splits an SSE body string into individual frames
// by splitting on blank lines (\n\n). Empty frames are discarded.
func splitModelSSEFrames(body string) []string {
	body = normalizeModelLineEndings(body)
	frames := make([]string, 0)
	for _, part := range strings.Split(body, "\n\n") {
		part = strings.Trim(part, "\n")
		if strings.TrimSpace(part) == "" {
			continue
		}
		frames = append(frames, part)
	}
	return frames
}

// normalizeModelLineEndings converts all line ending variants (\r\n, \r)
// to the canonical Unix form (\n) so that parsing is line-ending agnostic.
func normalizeModelLineEndings(value string) string {
	value = strings.ReplaceAll(value, "\r\n", "\n")
	value = strings.ReplaceAll(value, "\r", "\n")
	return value
}

// getHeaderValueCaseInsensitiveModel looks up a header value by key, ignoring case.
// Returns "" if the key is not found.
func getHeaderValueCaseInsensitiveModel(headers map[string]string, key string) string {
	key = strings.ToLower(strings.TrimSpace(key))
	for k, v := range headers {
		if strings.ToLower(strings.TrimSpace(k)) == key {
			return v
		}
	}
	return ""
}

// parseStreamChunkTime tries to parse a timestamp string in RFC3339Nano or RFC3339 format.
// Returns the zero time.Time if the string is empty or cannot be parsed.
func parseStreamChunkTime(raw string) time.Time {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}
	}
	// Try nanosecond precision first, then second precision.
	if parsed, err := time.Parse(time.RFC3339Nano, raw); err == nil {
		return parsed
	}
	if parsed, err := time.Parse(time.RFC3339, raw); err == nil {
		return parsed
	}
	return time.Time{}
}

// yamlNodeToString converts a YAML node to its string representation.
// Scalar nodes return their value directly.
// For complex nodes, the decoded value is re-marshalled to compact JSON.
func yamlNodeToString(node *yamlLib.Node) string {
	if node == nil {
		return ""
	}
	if node.Kind == yamlLib.ScalarNode {
		return node.Value
	}
	// Complex node (mapping or sequence) — decode then re-encode as JSON.
	var decoded interface{}
	if err := node.Decode(&decoded); err != nil {
		return strings.TrimSpace(node.Value)
	}
	b, err := json.Marshal(decoded)
	if err != nil {
		return strings.TrimSpace(node.Value)
	}
	return string(b)
}

// stringNode creates a YAML scalar string node with the given value.
// Used as a helper when constructing YAML trees programmatically.
func stringNode(value string) *yamlLib.Node {
	return &yamlLib.Node{
		Kind:  yamlLib.ScalarNode,
		Tag:   "!!str",
		Value: value,
	}
}

// cloneStreamChunks returns a deep copy of a slice of HTTPStreamChunk.
// This ensures that mutations to one copy do not affect the original.
func cloneStreamChunks(in []HTTPStreamChunk) []HTTPStreamChunk {
	out := make([]HTTPStreamChunk, len(in))
	for i := range in {
		out[i].TS = in[i].TS
		if len(in[i].Data) == 0 {
			continue
		}
		out[i].Data = make([]HTTPStreamDataField, len(in[i].Data))
		copy(out[i].Data, in[i].Data)
	}
	return out
}
