package wire

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"sync"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/kafka"
	"go.uber.org/zap"
)

// DecodeContext holds state information for decoding Kafka messages
type DecodeContext struct {
	Mode models.Mode
	// PendingRequests maps correlation IDs to their requests for response matching
	PendingRequests sync.Map // map[int32]*RequestInfo
}

// RequestInfo stores request metadata for matching with responses
type RequestInfo struct {
	Header  *RequestHeader
	APIName string
}

// NewDecodeContext creates a new DecodeContext for tracking Kafka message state
func NewDecodeContext(mode models.Mode) *DecodeContext {
	return &DecodeContext{
		Mode: mode,
	}
}

// DecodeRequest decodes a Kafka request from raw bytes
func DecodeRequest(ctx context.Context, logger *zap.Logger, data []byte, decodeCtx *DecodeContext) (*kafka.Request, error) {
	if len(data) < 4 {
		return nil, fmt.Errorf("data too short: need at least 4 bytes for length, got %d", len(data))
	}

	// Read length prefix (first 4 bytes, not included in payload)
	messageLen := int32(binary.BigEndian.Uint32(data[0:4]))

	// Validate message length
	if len(data) < int(4+messageLen) {
		return nil, fmt.Errorf("incomplete message: expected %d bytes, got %d", messageLen, len(data)-4)
	}

	payload := data[4 : 4+messageLen]

	// Parse request header
	header, headerSize, err := ParseRequestHeader(payload)
	if err != nil {
		return nil, fmt.Errorf("failed to parse request header: %w", err)
	}

	// Get API key name
	apiKeyName := ApiKeyToName(header.ApiKey)

	// Extract body (everything after header)
	body := payload[headerSize:]

	// Store request info for response matching
	decodeCtx.PendingRequests.Store(header.CorrelationID, &RequestInfo{
		Header:  header,
		APIName: apiKeyName,
	})

	// For now, store body as base64-encoded string
	// In the future, we can add specific decoders for each API type
	bodyEncoded := base64.StdEncoding.EncodeToString(body)

	request := &kafka.Request{
		Header: kafka.RequestHeader{
			ApiKey:        header.ApiKey,
			ApiVersion:    header.ApiVersion,
			CorrelationID: header.CorrelationID,
			ClientID:      header.ClientID,
		},
		APIKeyName: apiKeyName,
		Body:       bodyEncoded,
	}

	logger.Debug("Decoded Kafka request",
		zap.String("apiKey", apiKeyName),
		zap.Int16("apiVersion", header.ApiVersion),
		zap.Int32("correlationId", header.CorrelationID),
		zap.String("clientId", header.ClientID),
	)

	return request, nil
}

// DecodeResponse decodes a Kafka response from raw bytes
func DecodeResponse(ctx context.Context, logger *zap.Logger, data []byte, decodeCtx *DecodeContext) (*kafka.Response, *RequestInfo, error) {
	if len(data) < 4 {
		return nil, nil, fmt.Errorf("data too short: need at least 4 bytes for length, got %d", len(data))
	}

	// Read length prefix (first 4 bytes, not included in payload)
	messageLen := int32(binary.BigEndian.Uint32(data[0:4]))

	// Validate message length
	if len(data) < int(4+messageLen) {
		return nil, nil, fmt.Errorf("incomplete message: expected %d bytes, got %d", messageLen, len(data)-4)
	}

	payload := data[4 : 4+messageLen]

	// Parse response header (just correlation ID for now)
	if len(payload) < 4 {
		return nil, nil, fmt.Errorf("payload too short for response header: need at least 4 bytes, got %d", len(payload))
	}

	correlationID := int32(binary.BigEndian.Uint32(payload[0:4]))

	// Look up the matching request
	reqInfoVal, found := decodeCtx.PendingRequests.LoadAndDelete(correlationID)
	if !found {
		logger.Warn("No matching request found for response", zap.Int32("correlationId", correlationID))
	}

	var reqInfo *RequestInfo
	if found {
		reqInfo = reqInfoVal.(*RequestInfo)
	}

	// Extract body (everything after header)
	body := payload[4:]

	// For now, store body as base64-encoded string
	bodyEncoded := base64.StdEncoding.EncodeToString(body)

	response := &kafka.Response{
		Header: kafka.ResponseHeader{
			CorrelationID: correlationID,
		},
		Body: bodyEncoded,
	}

	logger.Debug("Decoded Kafka response",
		zap.Int32("correlationId", correlationID),
		zap.Int("bodyLen", len(body)),
	)

	return response, reqInfo, nil
}

// ApiKeyToName converts an API key to its string name
func ApiKeyToName(apiKey kafka.ApiKey) string {
	if name, ok := kafka.ApiKeyToString[apiKey]; ok {
		return name
	}
	return fmt.Sprintf("Unknown(%d)", apiKey)
}

// IsRequest checks if the buffer looks like a Kafka request
// This is used for traffic matching
func IsRequest(buffer []byte) bool {
	if len(buffer) < 12 {
		return false
	}

	// Skip 4-byte length prefix
	payload := buffer[4:]
	if len(payload) < 8 {
		return false
	}

	// Check API key (valid range is 0-67 as of Kafka 3.x)
	apiKey := int16(binary.BigEndian.Uint16(payload[0:2]))
	if apiKey < 0 || apiKey > 67 {
		return false
	}

	// Check API version (should be reasonable, typically 0-15)
	apiVersion := int16(binary.BigEndian.Uint16(payload[2:4]))
	if apiVersion < 0 || apiVersion > 20 {
		return false
	}

	return true
}
