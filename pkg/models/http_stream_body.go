package models

import (
	"bufio"
	"encoding/json"
	"strings"
	"time"

	yamlLib "gopkg.in/yaml.v3"
)

const maxModelStreamTokenSize = 10 * 1024 * 1024

type HTTPStreamDataField struct {
	Key   string
	Value string
}

type HTTPStreamChunk struct {
	TS   time.Time
	Data []HTTPStreamDataField
}

func (c HTTPStreamChunk) ValueByKey(key string) (string, bool) {
	key = strings.ToLower(strings.TrimSpace(key))
	for _, field := range c.Data {
		if strings.ToLower(strings.TrimSpace(field.Key)) == key {
			return field.Value, true
		}
	}
	return "", false
}

func (h HTTPResp) MarshalYAML() (interface{}, error) {
	bodyNode := &yamlLib.Node{}
	if chunks, ok := streamChunksForYAML(h); ok {
		bodyNode = encodeStreamChunksNode(chunks)
	} else {
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
		StreamRef     *StreamRef        `yaml:"stream_ref,omitempty"`
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
		StreamRef:     h.StreamRef,
	}, nil
}

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
		StreamRef     *StreamRef        `yaml:"stream_ref,omitempty"`
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
	h.StreamRef = raw.StreamRef

	body, chunks, err := decodeStreamBody(raw.Body, raw.Header, raw.Timestamp)
	if err != nil {
		return err
	}
	h.Body = body
	h.StreamBody = chunks

	return nil
}

type streamBodyKind string

const (
	streamBodyKindUnknown streamBodyKind = ""
	streamBodyKindSSE     streamBodyKind = "sse"
	streamBodyKindRaw     streamBodyKind = "raw"
)

func streamChunksForYAML(resp HTTPResp) ([]HTTPStreamChunk, bool) {
	if len(resp.StreamBody) > 0 {
		return cloneStreamChunks(resp.StreamBody), true
	}
	if !shouldStoreStreamingBody(resp) {
		return nil, false
	}

	kind := detectStreamBodyKind(resp.Header, resp.Body)
	if kind == streamBodyKindUnknown {
		return nil, false
	}

	switch kind {
	case streamBodyKindSSE:
		return parseSSEBodyToChunks(resp.Body, resp.Timestamp), true
	case streamBodyKindRaw:
		return parseRawBodyToChunks(resp.Body, resp.Timestamp, strings.Contains(strings.ToLower(getHeaderValueCaseInsensitiveModel(resp.Header, "Content-Type")), "application/x-ndjson") || strings.Contains(strings.ToLower(getHeaderValueCaseInsensitiveModel(resp.Header, "Content-Type")), "application/ndjson")), true
	default:
		return nil, false
	}
}

func shouldStoreStreamingBody(resp HTTPResp) bool {
	contentType := strings.ToLower(getHeaderValueCaseInsensitiveModel(resp.Header, "Content-Type"))
	transferEncoding := strings.ToLower(getHeaderValueCaseInsensitiveModel(resp.Header, "Transfer-Encoding"))

	if strings.Contains(contentType, "text/event-stream") {
		return true
	}
	if strings.Contains(contentType, "application/x-ndjson") || strings.Contains(contentType, "application/ndjson") {
		return true
	}
	if strings.Contains(contentType, "text/plain") {
		if strings.Contains(transferEncoding, "chunked") || looksLikeLineDelimitedStreamBody(resp.Body) {
			return true
		}
	}
	if strings.Contains(contentType, "application/octet-stream") && strings.Contains(transferEncoding, "chunked") {
		return true
	}
	return false
}

func detectStreamBodyKind(headers map[string]string, body string) streamBodyKind {
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
	if looksLikeSSEBodyForModel(body) {
		return streamBodyKindSSE
	}
	return streamBodyKindUnknown
}

func decodeStreamBody(node yamlLib.Node, headers map[string]string, respTimestamp time.Time) (string, []HTTPStreamChunk, error) {
	if node.Kind == 0 {
		return "", nil, nil
	}

	if node.Kind == yamlLib.SequenceNode {
		chunks, err := decodeStreamChunks(node)
		if err != nil {
			return "", nil, err
		}
		return streamChunksToLegacyBody(chunks, detectStreamBodyKind(headers, "")), chunks, nil
	}

	var body string
	if err := node.Decode(&body); err != nil {
		return "", nil, err
	}

	legacyChunks, ok := deriveLegacyStreamChunks(headers, body, respTimestamp)
	if ok {
		return body, legacyChunks, nil
	}

	return body, nil, nil
}

func deriveLegacyStreamChunks(headers map[string]string, body string, ts time.Time) ([]HTTPStreamChunk, bool) {
	kind := detectStreamBodyKind(headers, body)
	switch kind {
	case streamBodyKindSSE:
		chunks := parseSSEBodyToChunks(body, ts)
		if len(chunks) > 0 {
			return chunks, true
		}
	case streamBodyKindRaw:
		contentType := strings.ToLower(getHeaderValueCaseInsensitiveModel(headers, "Content-Type"))
		isNDJSON := strings.Contains(contentType, "application/x-ndjson") || strings.Contains(contentType, "application/ndjson")
		if strings.Contains(contentType, "application/octet-stream") {
			if strings.TrimSpace(body) == "" {
				return nil, false
			}
			return []HTTPStreamChunk{{
				TS: ts,
				Data: []HTTPStreamDataField{{
					Key:   "raw",
					Value: body,
				}},
			}}, true
		}

		chunks := parseRawBodyToChunks(body, ts, isNDJSON)
		if len(chunks) > 0 {
			return chunks, true
		}
	}
	return nil, false
}

func decodeStreamChunks(seqNode yamlLib.Node) ([]HTTPStreamChunk, error) {
	chunks := make([]HTTPStreamChunk, 0, len(seqNode.Content))
	for _, chunkNode := range seqNode.Content {
		if chunkNode == nil || chunkNode.Kind != yamlLib.MappingNode {
			continue
		}
		chunk := HTTPStreamChunk{}

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

func decodeStreamDataFields(dataNode *yamlLib.Node) ([]HTTPStreamDataField, error) {
	if dataNode == nil {
		return nil, nil
	}
	if dataNode.Kind != yamlLib.MappingNode {
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

func encodeStreamChunksNode(chunks []HTTPStreamChunk) *yamlLib.Node {
	root := &yamlLib.Node{Kind: yamlLib.SequenceNode, Tag: "!!seq"}
	for _, chunk := range chunks {
		entry := &yamlLib.Node{Kind: yamlLib.MappingNode, Tag: "!!map"}

		if !chunk.TS.IsZero() {
			entry.Content = append(entry.Content, stringNode("ts"), stringNode(chunk.TS.UTC().Format(time.RFC3339Nano)))
		}

		dataNode := &yamlLib.Node{Kind: yamlLib.MappingNode, Tag: "!!map"}
		for _, field := range chunk.Data {
			dataNode.Content = append(dataNode.Content, stringNode(field.Key), stringNode(field.Value))
		}
		entry.Content = append(entry.Content, stringNode("data"), dataNode)
		root.Content = append(root.Content, entry)
	}
	return root
}

func streamChunksToLegacyBody(chunks []HTTPStreamChunk, kind streamBodyKind) string {
	if len(chunks) == 0 {
		return ""
	}

	switch kind {
	case streamBodyKindSSE:
		frames := make([]string, 0, len(chunks))
		for _, chunk := range chunks {
			lines := make([]string, 0, len(chunk.Data))
			for _, field := range chunk.Data {
				key := strings.TrimSpace(field.Key)
				if strings.EqualFold(key, "comment") {
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
		lines := make([]string, 0, len(chunks))
		for _, chunk := range chunks {
			if raw, ok := chunk.ValueByKey("raw"); ok {
				lines = append(lines, raw)
				continue
			}
			if len(chunk.Data) > 0 {
				lines = append(lines, chunk.Data[0].Value)
			}
		}
		return strings.Join(lines, "\n")
	}
}

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

			if strings.HasPrefix(line, ":") {
				fields = append(fields, HTTPStreamDataField{
					Key:   "comment",
					Value: strings.TrimPrefix(line, ":"),
				})
				continue
			}

			key, value, ok := strings.Cut(line, ":")
			if !ok {
				key = strings.TrimSpace(line)
				value = ""
			} else if strings.HasPrefix(value, " ") {
				value = value[1:]
			}
			if key == "" {
				continue
			}
			normalizedKey := strings.ToLower(strings.TrimSpace(key))
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

func parseRawBodyToChunks(body string, ts time.Time, ignoreEmpty bool) []HTTPStreamChunk {
	body = normalizeModelLineEndings(body)
	if body == "" {
		return nil
	}

	content := make([]string, 0)
	scanner := bufio.NewScanner(strings.NewReader(body))
	scanner.Buffer(make([]byte, 0, 64*1024), maxModelStreamTokenSize)
	for scanner.Scan() {
		line := scanner.Text()
		if ignoreEmpty && strings.TrimSpace(line) == "" {
			continue
		}
		content = append(content, line)
	}

	if err := scanner.Err(); err != nil {
		content = nil
	}

	if len(content) == 0 {
		content = append(content, body)
	}

	chunks := make([]HTTPStreamChunk, 0, len(content))
	for _, line := range content {
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

func looksLikeSSEBodyForModel(body string) bool {
	body = normalizeModelLineEndings(body)
	return strings.Contains(body, "\n\n") && (strings.Contains(body, "\ndata:") || strings.HasPrefix(body, "data:") || strings.Contains(body, "\nevent:") || strings.HasPrefix(body, "event:"))
}

func looksLikeLineDelimitedStreamBody(body string) bool {
	body = normalizeModelLineEndings(body)
	body = strings.TrimSuffix(body, "\n")
	if strings.TrimSpace(body) == "" {
		return false
	}
	return strings.Contains(body, "\n")
}

func normalizeModelLineEndings(value string) string {
	value = strings.ReplaceAll(value, "\r\n", "\n")
	value = strings.ReplaceAll(value, "\r", "\n")
	return value
}

func getHeaderValueCaseInsensitiveModel(headers map[string]string, key string) string {
	key = strings.ToLower(strings.TrimSpace(key))
	for k, v := range headers {
		if strings.ToLower(strings.TrimSpace(k)) == key {
			return v
		}
	}
	return ""
}

func parseStreamChunkTime(raw string) time.Time {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}
	}
	if parsed, err := time.Parse(time.RFC3339Nano, raw); err == nil {
		return parsed
	}
	if parsed, err := time.Parse(time.RFC3339, raw); err == nil {
		return parsed
	}
	return time.Time{}
}

func yamlNodeToString(node *yamlLib.Node) string {
	if node == nil {
		return ""
	}
	if node.Kind == yamlLib.ScalarNode {
		return node.Value
	}
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

func stringNode(value string) *yamlLib.Node {
	return &yamlLib.Node{
		Kind:  yamlLib.ScalarNode,
		Tag:   "!!str",
		Value: value,
	}
}

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
