package http

import (
	"bufio"
	"bytes"
	"encoding/json"
	"encoding/xml"
	"io"
	"net/http"
	"regexp"
	"strings"

	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
)

// Checks if the response is gzipped
func isGZipped(check io.ReadCloser, l *zap.Logger) (bool, *bufio.Reader) {
	bufReader := bufio.NewReader(check)
	peekedBytes, err := bufReader.Peek(2)
	if err != nil && err != io.EOF {
		l.Debug("failed to peek the response", zap.Error(err))
		return false, nil
	}
	if len(peekedBytes) < 2 {
		return false, nil
	}
	if peekedBytes[0] == 0x1f && peekedBytes[1] == 0x8b {
		return true, bufReader
	}
	return false, nil
}

// hasCompleteHeaders checks if the given byte slice contains the complete HTTP headers
func hasCompleteHeaders(httpChunk []byte) bool {
	// Define the sequence for header end: "\r\n\r\n"
	headerEndSequence := []byte{'\r', '\n', '\r', '\n'}

	// Check if the byte slice contains the header end sequence
	return bytes.Contains(httpChunk, headerEndSequence)
}

// extract the request metadata from the request
func GetReqMeta(req *http.Request) map[string]string {
	reqMeta := map[string]string{}
	if req != nil {
		// get request metadata
		reqMeta = map[string]string{
			"method": req.Method,
			"url":    req.URL.String(),
			"host":   req.Host,
		}
	}
	return reqMeta
}

func IsPassThrough(logger *zap.Logger, req *http.Request, destPort uint, opts models.OutgoingOptions) bool {
	passThrough := false

	for _, bypass := range opts.Rules {
		if bypass.Host != "" {
			regex, err := regexp.Compile(bypass.Host)
			if err != nil {
				utils.LogError(logger, err, "failed to compile the host regex", zap.Any("metadata", GetReqMeta(req)))
				continue
			}
			passThrough = regex.MatchString(req.Host)
			if !passThrough {
				continue
			}
		}
		if bypass.Path != "" {
			regex, err := regexp.Compile(bypass.Path)
			if err != nil {
				utils.LogError(logger, err, "failed to compile the path regex", zap.Any("metadata", GetReqMeta(req)))
				continue
			}
			passThrough = regex.MatchString(req.URL.String())
			if !passThrough {
				continue
			}
		}

		if passThrough {
			if bypass.Port == 0 || bypass.Port == destPort {
				return true
			}
			passThrough = false
		}
	}

	return passThrough
}

func IsJSON(body []byte) bool {
	var js interface{}
	return json.Unmarshal(body, &js) == nil
}

func IsXML(data []byte) bool {
	var xm xml.Name
	return xml.Unmarshal(data, &xm) == nil
}

// IsCSV checks if data can be parsed as CSV by looking for common characteristics
func IsCSV(data []byte) bool {
	// Very simple CSV check: look for commas in the first line
	content := string(data)
	if lines := strings.Split(content, "\n"); len(lines) > 0 {
		return strings.Contains(lines[0], ",")
	}
	return false
}

type ContentType string

// Constants for different content types.
const (
	Unknown   ContentType = "Unknown"
	JSON      ContentType = "JSON"
	XML       ContentType = "XML"
	CSV       ContentType = "CSV"
	HTML      ContentType = "HTML"
	TextPlain ContentType = "TextPlain"
)

func guessContentType(data []byte) ContentType {
	// Use net/http library's DetectContentType for basic MIME type detection
	mimeType := http.DetectContentType(data)

	// Additional checks to further specify the content type
	switch {
	case IsJSON(data):
		return JSON
	case IsXML(data):
		return XML
	case strings.Contains(mimeType, "text/html"):
		return HTML
	case strings.Contains(mimeType, "text/plain"):
		if IsCSV(data) {
			return CSV
		}
		return TextPlain
	}

	return Unknown
}

func encode(buffer []byte) string {
	//Encode the buffer to string
	encoded := string(buffer)
	return encoded
}
func decode(encoded string) ([]byte, error) {
	// decode the string to a buffer.
	data := []byte(encoded)
	return data, nil
}
