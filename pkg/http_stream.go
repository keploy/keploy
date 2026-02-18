// Package pkg provides utility functions for Keploy.
package pkg

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"

	"go.keploy.io/server/v3/pkg/models"
)

func DetectHTTPStreamType(resp *http.Response) (models.HTTPStreamType, bool) {
	if resp == nil {
		return "", false
	}

	if IsSSEContentType(resp.Header.Get("Content-Type")) {
		return models.HTTPStreamTypeSSE, true
	}

	if IsChunkedTransferEncoding(resp.TransferEncoding) {
		return models.HTTPStreamTypeHTTP, true
	}

	return "", false
}

func IsChunkedTransferEncoding(transferEncoding []string) bool {
	for _, v := range transferEncoding {
		if strings.EqualFold(strings.TrimSpace(v), "chunked") {
			return true
		}
	}
	return false
}

func IsSSEContentType(contentType string) bool {
	if contentType == "" {
		return false
	}

	ct := strings.ToLower(strings.TrimSpace(contentType))
	if idx := strings.IndexByte(ct, ';'); idx >= 0 {
		ct = strings.TrimSpace(ct[:idx])
	}
	return ct == "text/event-stream"
}

// ExtractSSEEvents extracts complete SSE events separated by a double newline.
// It returns normalized event payloads and any trailing partial bytes.
func ExtractSSEEvents(buffer []byte) ([]string, []byte) {
	events := make([]string, 0)
	rest := buffer

	for {
		idx, delimLen := findSSEDelimiter(rest)
		if idx < 0 {
			break
		}

		evt := NormalizeSSEEventData(string(rest[:idx]))
		if evt != "" {
			events = append(events, evt)
		}

		rest = rest[idx+delimLen:]
	}

	remaining := make([]byte, len(rest))
	copy(remaining, rest)
	return events, remaining
}

func NormalizeSSEEventData(data string) string {
	normalized := strings.ReplaceAll(data, "\r\n", "\n")
	normalized = strings.TrimRight(normalized, "\n")
	return normalized
}

func findSSEDelimiter(data []byte) (int, int) {
	idxLF := bytes.Index(data, []byte("\n\n"))
	idxCRLF := bytes.Index(data, []byte("\r\n\r\n"))

	switch {
	case idxLF == -1 && idxCRLF == -1:
		return -1, 0
	case idxLF == -1:
		return idxCRLF, 4
	case idxCRLF == -1:
		return idxLF, 2
	case idxLF < idxCRLF:
		return idxLF, 2
	default:
		return idxCRLF, 4
	}
}

// StreamEventsToComparableBody converts stream events to deterministic JSON so
// normal response comparison and body-noise assertions can be reused.
func StreamEventsToComparableBody(streamType models.HTTPStreamType, events []models.HTTPStreamEvent) (string, error) {
	if len(events) == 0 {
		return "", nil
	}

	switch streamType {
	case models.HTTPStreamTypeSSE:
		return sseEventsToJSON(events)
	default:
		return httpStreamEventsToJSON(events)
	}
}

func sseEventsToJSON(events []models.HTTPStreamEvent) (string, error) {
	out := make([]map[string]interface{}, 0, len(events))
	for _, evt := range events {
		parsed := parseSSEEvent(evt.Data)
		out = append(out, parsed)
	}

	b, err := json.Marshal(out)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func httpStreamEventsToJSON(events []models.HTTPStreamEvent) (string, error) {
	out := make([]interface{}, 0, len(events))
	for _, evt := range events {
		data := strings.TrimSpace(evt.Data)
		var parsed interface{}
		if json.Valid([]byte(data)) && json.Unmarshal([]byte(data), &parsed) == nil {
			out = append(out, parsed)
		} else {
			out = append(out, evt.Data)
		}
	}

	b, err := json.Marshal(out)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func parseSSEEvent(event string) map[string]interface{} {
	res := map[string]interface{}{}
	lines := strings.Split(NormalizeSSEEventData(event), "\n")
	dataLines := make([]string, 0, 1)

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, ":") {
			continue
		}

		parts := strings.SplitN(line, ":", 2)
		key := strings.TrimSpace(parts[0])
		val := ""
		if len(parts) == 2 {
			val = strings.TrimLeft(parts[1], " ")
		}

		if key == "data" {
			dataLines = append(dataLines, val)
			continue
		}
		res[key] = val
	}

	if len(dataLines) > 0 {
		joined := strings.Join(dataLines, "\n")
		var parsed interface{}
		if json.Valid([]byte(joined)) && json.Unmarshal([]byte(joined), &parsed) == nil {
			res["data"] = parsed
		} else {
			res["data"] = joined
		}
	}

	if len(res) == 0 {
		res["raw"] = NormalizeSSEEventData(event)
	}
	return res
}

func SSEEventType(event string) string {
	parsed := parseSSEEvent(event)
	v, _ := parsed["event"].(string)
	return v
}
