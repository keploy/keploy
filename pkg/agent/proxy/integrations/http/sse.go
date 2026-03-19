// Package http provides SSE (Server-Sent Events) and chunked streaming support
// for recording and replaying outgoing HTTP streams.
package http

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"go.keploy.io/server/v3/pkg"
	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	pUtil "go.keploy.io/server/v3/pkg/agent/proxy/util"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// streamType indicates the kind of streaming response detected.
type streamType int

const (
	streamNone        streamType = iota
	streamSSE                    // Content-Type: text/event-stream
	streamChunkedText            // Content-Type: text/plain + Transfer-Encoding: chunked
)

// isStreamingResponse detects whether the raw HTTP response bytes indicate a streaming response.
// It returns the stream type found.
func isStreamingResponse(resp []byte) streamType {
	// Find end of headers
	headerEnd := bytes.Index(resp, []byte("\r\n\r\n"))
	if headerEnd == -1 {
		return streamNone
	}
	headerSection := strings.ToLower(string(resp[:headerEnd]))

	// Check for SSE
	if containsHeader(headerSection, "content-type", "text/event-stream") {
		return streamSSE
	}

	// Check for chunked text/plain
	if containsHeader(headerSection, "content-type", "text/plain") &&
		containsHeader(headerSection, "transfer-encoding", "chunked") {
		return streamChunkedText
	}

	return streamNone
}

// containsHeader checks if a lowercased header section contains a header with the given key and value substring.
func containsHeader(headerSection, key, valueSubstr string) bool {
	for _, line := range strings.Split(headerSection, "\r\n") {
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			continue
		}
		if strings.TrimSpace(parts[0]) == key && strings.Contains(strings.TrimSpace(parts[1]), valueSubstr) {
			return true
		}
	}
	return false
}

// parseSSEFrames parses raw SSE text into structured frames.
// SSE spec: fields are "data:", "event:", "id:", "retry:" separated by "\n\n".
func parseSSEFrames(raw []byte) []models.SSEFrame {
	var frames []models.SSEFrame

	// Split by double newline (event boundary)
	events := splitSSEEvents(string(raw))

	for _, event := range events {
		event = strings.TrimSpace(event)
		if event == "" {
			continue
		}

		var frame models.SSEFrame
		hasData := false

		lines := strings.Split(event, "\n")
		var dataLines []string

		for _, line := range lines {
			if strings.HasPrefix(line, ":") {
				// Comment line, skip
				continue
			}

			colonIdx := strings.Index(line, ":")
			if colonIdx == -1 {
				// Field with no value
				continue
			}

			field := line[:colonIdx]
			value := line[colonIdx+1:]
			// Remove optional leading space after colon
			if len(value) > 0 && value[0] == ' ' {
				value = value[1:]
			}

			switch field {
			case "data":
				dataLines = append(dataLines, value)
				hasData = true
			case "event":
				frame.Event = value
			case "id":
				frame.ID = value
			case "retry":
				if n, err := strconv.Atoi(value); err == nil {
					frame.Retry = n
				}
			}
		}

		if hasData {
			frame.Data = strings.Join(dataLines, "\n")
		}

		// Only add frames that have at least some content
		if hasData || frame.Event != "" || frame.ID != "" {
			frames = append(frames, frame)
		}
	}

	return frames
}

// splitSSEEvents splits raw SSE text by double newlines (event boundaries).
// Handles both \n\n and \r\n\r\n.
func splitSSEEvents(raw string) []string {
	// Normalize line endings
	normalized := strings.ReplaceAll(raw, "\r\n", "\n")
	return strings.Split(normalized, "\n\n")
}

// formatSSEFrame formats a single SSEFrame back to SSE wire format text.
func formatSSEFrame(frame models.SSEFrame) []byte {
	var buf bytes.Buffer

	if frame.ID != "" {
		buf.WriteString("id: ")
		buf.WriteString(frame.ID)
		buf.WriteByte('\n')
	}
	if frame.Event != "" {
		buf.WriteString("event: ")
		buf.WriteString(frame.Event)
		buf.WriteByte('\n')
	}
	if frame.Retry > 0 {
		buf.WriteString("retry: ")
		buf.WriteString(strconv.Itoa(frame.Retry))
		buf.WriteByte('\n')
	}

	// Data can be multi-line — each line gets its own "data:" prefix
	dataLines := strings.Split(frame.Data, "\n")
	for _, dl := range dataLines {
		buf.WriteString("data: ")
		buf.WriteString(dl)
		buf.WriteByte('\n')
	}

	// Terminate with empty line (event boundary)
	buf.WriteByte('\n')

	return buf.Bytes()
}

// encodeSSE handles recording of an SSE or chunked-text stream.
// It tees data to the client while capturing frames, then sends a mock with
// serialized frames in the body. InsertMock will split frames into a stream file.
func (h *HTTP) encodeSSE(
	ctx context.Context,
	finalReq []byte,
	clientConn, destConn net.Conn,
	mocks chan<- *models.Mock,
	reqTimestamp time.Time,
	initialResp []byte,
	destPort uint,
	opts models.OutgoingOptions,
	sType streamType,
) error {
	// Parse the request to build the reference mock
	req, err := http.ReadRequest(bufio.NewReader(bytes.NewReader(finalReq)))
	if err != nil {
		utils.LogError(h.Logger, err, "failed to parse HTTP request for SSE recording")
		return err
	}
	req.Header.Set("Host", req.Host)

	var reqBody []byte
	if req.Body != nil {
		reqBody, err = io.ReadAll(req.Body)
		if err != nil {
			utils.LogError(h.Logger, err, "failed to read request body for SSE recording")
			return err
		}
		if req.Header.Get("Content-Encoding") != "" {
			reqBody, err = pkg.Decompress(h.Logger, req.Header.Get("Content-Encoding"), reqBody)
			if err != nil {
				utils.LogError(h.Logger, err, "failed to decompress request body for SSE recording")
				return err
			}
		}
	}

	// Check passthrough
	if utils.IsPassThrough(h.Logger, req, destPort, opts) {
		h.Logger.Debug("SSE request is a passThrough", zap.Any("metadata", utils.GetReqMeta(req)))
		return nil
	}

	// Forward the initial response (headers + any partial body) to client
	_, err = clientConn.Write(initialResp)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		utils.LogError(h.Logger, err, "failed to write initial SSE response to client")
		return err
	}

	// Parse the response headers
	respParsed, err := http.ReadResponse(bufio.NewReader(bytes.NewReader(initialResp)), req)
	if err != nil {
		utils.LogError(h.Logger, err, "failed to parse HTTP response for SSE recording")
		return err
	}

	streamTypeName := "sse"
	if sType == streamChunkedText {
		streamTypeName = "chunked-text"
	}

	h.Logger.Info("Recording SSE stream",
		zap.String("url", req.URL.String()),
		zap.String("streamType", streamTypeName))

	// Accumulate all frames during the stream
	var allFrames []models.SSEFrame
	var frameBuf bytes.Buffer
	lastFrameTime := time.Now()

	// Find the body portion of the initial response (after headers)
	headerEnd := bytes.Index(initialResp, []byte("\r\n\r\n"))
	if headerEnd != -1 && headerEnd+4 < len(initialResp) {
		initialBody := initialResp[headerEnd+4:]
		frameBuf.Write(initialBody)
	}

	// Stream loop: read from destConn, tee to clientConn, parse frames
	for {
		select {
		case <-ctx.Done():
			flushed := h.flushSSEFrames(&frameBuf, &lastFrameTime, sType, h.Logger)
			allFrames = append(allFrames, flushed...)
			goto sendMock
		default:
		}

		chunk, err := pUtil.ReadBytes(ctx, h.Logger, destConn)
		if err != nil {
			if err == io.EOF {
				h.Logger.Debug("SSE stream ended (EOF)")
				if len(chunk) > 0 {
					_, _ = clientConn.Write(chunk)
					frameBuf.Write(chunk)
				}
				flushed := h.flushSSEFrames(&frameBuf, &lastFrameTime, sType, h.Logger)
				allFrames = append(allFrames, flushed...)
				goto sendMock
			}
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				h.Logger.Debug("SSE stream read timeout, flushing frames")
				flushed := h.flushSSEFrames(&frameBuf, &lastFrameTime, sType, h.Logger)
				allFrames = append(allFrames, flushed...)
				goto sendMock
			}
			utils.LogError(h.Logger, err, "failed to read SSE stream from destination")
			return err
		}

		// Tee to client
		_, writeErr := clientConn.Write(chunk)
		if writeErr != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			utils.LogError(h.Logger, writeErr, "failed to write SSE chunk to client")
			return writeErr
		}

		frameBuf.Write(chunk)

		// Extract complete frames
		frames := h.extractCompleteFrames(&frameBuf, &lastFrameTime, sType)
		allFrames = append(allFrames, frames...)

		for _, frame := range frames {
			h.Logger.Debug("Captured SSE frame",
				zap.String("event", frame.Event),
				zap.String("id", frame.ID),
				zap.Int64("delayMs", frame.DelayMs),
				zap.Int("totalFrames", len(allFrames)))
		}
	}

sendMock:
	// Serialize all frames as YAML for storage
	framesYAML, err := serializeSSEFrames(allFrames)
	if err != nil {
		utils.LogError(h.Logger, err, "failed to serialize SSE frames")
		return err
	}

	connID := ctx.Value(models.ClientConnectionIDKey).(string)

	// Build the mock with frames in the body — InsertMock will split to stream file
	refMock := &models.Mock{
		Version: models.GetVersion(),
		Name:    "mocks",
		Kind:    models.HTTP,
		Spec: models.MockSpec{
			Metadata: map[string]string{
				"name":       "Http",
				"type":       "sse-stream",
				"operation":  string(req.Method),
				"connID":     connID,
				"streamType": streamTypeName,
			},
			HTTPReq: &models.HTTPReq{
				Method:     models.Method(req.Method),
				ProtoMajor: req.ProtoMajor,
				ProtoMinor: req.ProtoMinor,
				URL:        req.URL.String(),
				Header:     pkg.ToYamlHTTPHeader(req.Header),
				Body:       string(reqBody),
				URLParams:  pkg.URLParams(req),
			},
			HTTPResp: &models.HTTPResp{
				StatusCode: respParsed.StatusCode,
				Header:     pkg.ToYamlHTTPHeader(respParsed.Header),
				Body:       string(framesYAML), // frames serialized as YAML — InsertMock splits to file
			},
			Created:          time.Now().Unix(),
			ReqTimestampMock: reqTimestamp,
			ResTimestampMock: time.Now(),
		},
	}

	h.Logger.Info("SSE recording complete",
		zap.String("url", req.URL.String()),
		zap.Int("totalFrames", len(allFrames)))

	// Send mock to the channel
	if opts.Synchronous {
		if mgr := syncMock.Get(); mgr != nil {
			mgr.AddMock(refMock)
		}
	} else {
		mocks <- refMock
	}

	return nil
}

// extractCompleteFrames extracts complete frames from the buffer based on stream type.
// For SSE: frames are delimited by \n\n
// For chunked text: each chunk is a frame
// Returns extracted frames and updates the buffer to contain only incomplete data.
func (h *HTTP) extractCompleteFrames(buf *bytes.Buffer, lastFrameTime *time.Time, sType streamType) []models.SSEFrame {
	data := buf.String()
	var frames []models.SSEFrame

	switch sType {
	case streamSSE:
		// Normalize line endings
		normalized := strings.ReplaceAll(data, "\r\n", "\n")

		// Split by double newline
		parts := strings.Split(normalized, "\n\n")
		if len(parts) <= 1 {
			// No complete frame yet
			return nil
		}

		// All parts except the last are complete frames
		for i := 0; i < len(parts)-1; i++ {
			eventText := strings.TrimSpace(parts[i])
			if eventText == "" {
				continue
			}
			parsed := parseSSEFrames([]byte(eventText + "\n\n"))
			now := time.Now()
			for j := range parsed {
				parsed[j].DelayMs = now.Sub(*lastFrameTime).Milliseconds()
				*lastFrameTime = now
			}
			frames = append(frames, parsed...)
		}

		// Keep the incomplete last part in the buffer
		buf.Reset()
		buf.WriteString(parts[len(parts)-1])

	case streamChunkedText:
		// For chunked text, treat each accumulated chunk as a single frame
		now := time.Now()
		if data != "" {
			frame := models.SSEFrame{
				Data:    data,
				DelayMs: now.Sub(*lastFrameTime).Milliseconds(),
			}
			*lastFrameTime = now
			frames = append(frames, frame)
			buf.Reset()
		}
	}

	return frames
}

// flushSSEFrames flushes any remaining data in the buffer as frames.
func (h *HTTP) flushSSEFrames(buf *bytes.Buffer, lastFrameTime *time.Time, sType streamType, logger *zap.Logger) []models.SSEFrame {
	remaining := strings.TrimSpace(buf.String())
	if remaining == "" {
		return nil
	}

	var frames []models.SSEFrame
	now := time.Now()
	switch sType {
	case streamSSE:
		parsed := parseSSEFrames([]byte(remaining))
		for i := range parsed {
			parsed[i].DelayMs = now.Sub(*lastFrameTime).Milliseconds()
			*lastFrameTime = now
		}
		frames = parsed
		if len(parsed) > 0 {
			logger.Debug("Flushed remaining SSE frames", zap.Int("count", len(parsed)))
		}
	case streamChunkedText:
		frame := models.SSEFrame{
			Data:    remaining,
			DelayMs: now.Sub(*lastFrameTime).Milliseconds(),
		}
		*lastFrameTime = now
		frames = append(frames, frame)
		logger.Debug("Flushed remaining chunked text frame")
	}

	buf.Reset()
	return frames
}

// replaySSE replays an SSE or chunked-text stream mock back to the client with original timing.
func (h *HTTP) replaySSE(
	ctx context.Context,
	clientConn net.Conn,
	stub *models.Mock,
	frames []models.SSEFrame,
) error {
	// Build status line
	resp := stub.Spec.HTTPResp
	statusLine := fmt.Sprintf("HTTP/%d.%d %d %s\r\n",
		stub.Spec.HTTPReq.ProtoMajor, stub.Spec.HTTPReq.ProtoMinor,
		resp.StatusCode, http.StatusText(resp.StatusCode))

	// Build headers
	header := pkg.ToHTTPHeader(resp.Header)
	var headers string
	for key, values := range header {
		// Remove Content-Length for streaming responses
		if key == "Content-Length" {
			continue
		}
		for _, value := range values {
			headers += fmt.Sprintf("%s: %s\r\n", key, value)
		}
	}

	// Write status line + headers immediately
	headerData := statusLine + headers + "\r\n"
	_, err := clientConn.Write([]byte(headerData))
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		utils.LogError(h.Logger, err, "failed to write SSE headers to client")
		return err
	}

	h.Logger.Debug("SSE replay: sent headers", zap.Int("frameCount", len(frames)))

	// Determine stream type from metadata
	streamTypeName := stub.Spec.Metadata["streamType"]
	isSSE := streamTypeName != "chunked-text"

	// Stream frames with timing
	for i, frame := range frames {
		// Preserve original timing
		if frame.DelayMs > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(frame.DelayMs) * time.Millisecond):
			}
		}

		var frameData []byte
		if isSSE {
			frameData = formatSSEFrame(frame)
		} else {
			// Chunked text: send raw data
			frameData = []byte(frame.Data)
		}

		_, err := clientConn.Write(frameData)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			utils.LogError(h.Logger, err, "failed to write SSE frame to client",
				zap.Int("frame", i))
			return err
		}

		h.Logger.Debug("SSE replay: sent frame",
			zap.Int("frame", i),
			zap.Int64("delayMs", frame.DelayMs))
	}

	h.Logger.Info("SSE replay completed", zap.Int("totalFrames", len(frames)))
	return nil
}

// serializeSSEFrames serializes SSE frames to YAML bytes for storage.
func serializeSSEFrames(frames []models.SSEFrame) ([]byte, error) {
	var buf bytes.Buffer
	for _, frame := range frames {
		// Write each frame as a simple text block with metadata
		buf.WriteString(fmt.Sprintf("---\nid: %q\nevent: %q\ndata: %q\nretry: %d\ndelay_ms: %d\n",
			frame.ID, frame.Event, frame.Data, frame.Retry, frame.DelayMs))
	}
	return buf.Bytes(), nil
}

// deserializeSSEFrames deserializes SSE frames from YAML-like text.
func deserializeSSEFrames(data []byte) ([]models.SSEFrame, error) {
	var frames []models.SSEFrame

	// Split by --- separator
	docs := strings.Split(string(data), "---\n")
	for _, doc := range docs {
		doc = strings.TrimSpace(doc)
		if doc == "" {
			continue
		}

		var frame models.SSEFrame
		for _, line := range strings.Split(doc, "\n") {
			parts := strings.SplitN(line, ": ", 2)
			if len(parts) != 2 {
				continue
			}
			key := parts[0]
			value := parts[1]

			switch key {
			case "id":
				frame.ID = unquote(value)
			case "event":
				frame.Event = unquote(value)
			case "data":
				frame.Data = unquote(value)
			case "retry":
				if n, err := strconv.Atoi(value); err == nil {
					frame.Retry = n
				}
			case "delay_ms":
				if n, err := strconv.ParseInt(value, 10, 64); err == nil {
					frame.DelayMs = n
				}
			}
		}
		frames = append(frames, frame)
	}

	return frames, nil
}

// unquote removes surrounding quotes from a string if present.
func unquote(s string) string {
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		unquoted, err := strconv.Unquote(s)
		if err == nil {
			return unquoted
		}
	}
	return s
}

