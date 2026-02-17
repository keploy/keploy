// Package pkg provides utility functions for Keploy.
package pkg

import (
	"bytes"
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
