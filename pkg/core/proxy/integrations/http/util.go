package http

import (
	"bufio"
	"bytes"
	"io"
	"net/http"
	"regexp"

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
