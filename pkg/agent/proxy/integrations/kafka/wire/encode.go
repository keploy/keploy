package wire

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"fmt"

	"go.keploy.io/server/v3/pkg/models/kafka"
	"go.uber.org/zap"
)

// EncodeRequest encodes a Kafka request struct back to bytes
func EncodeRequest(ctx context.Context, logger *zap.Logger, request *kafka.Request) ([]byte, error) {
	// Encode header
	headerBytes, err := encodeRequestHeader(&request.Header)
	if err != nil {
		return nil, fmt.Errorf("failed to encode request header: %w", err)
	}

	// Decode body from base64 if it's a string
	var bodyBytes []byte
	switch body := request.Body.(type) {
	case string:
		bodyBytes, err = base64.StdEncoding.DecodeString(body)
		if err != nil {
			return nil, fmt.Errorf("failed to decode request body from base64: %w", err)
		}
	case []byte:
		bodyBytes = body
	default:
		return nil, fmt.Errorf("unsupported request body type: %T", request.Body)
	}

	// Combine header and body
	payload := append(headerBytes, bodyBytes...)

	// Add length prefix
	result := make([]byte, 4+len(payload))
	binary.BigEndian.PutUint32(result[0:4], uint32(len(payload)))
	copy(result[4:], payload)

	logger.Debug("Encoded Kafka request",
		zap.String("apiKey", request.APIKeyName),
		zap.Int32("correlationId", request.Header.CorrelationID),
		zap.Int("totalLen", len(result)),
	)

	return result, nil
}

// EncodeResponse encodes a Kafka response struct back to bytes
func EncodeResponse(ctx context.Context, logger *zap.Logger, response *kafka.Response) ([]byte, error) {
	// Encode header (just correlation ID for basic responses)
	headerBytes := make([]byte, 4)
	binary.BigEndian.PutUint32(headerBytes, uint32(response.Header.CorrelationID))

	// Decode body from base64 if it's a string
	var bodyBytes []byte
	var err error
	switch body := response.Body.(type) {
	case string:
		bodyBytes, err = base64.StdEncoding.DecodeString(body)
		if err != nil {
			return nil, fmt.Errorf("failed to decode response body from base64: %w", err)
		}
	case []byte:
		bodyBytes = body
	case nil:
		bodyBytes = []byte{}
	default:
		return nil, fmt.Errorf("unsupported response body type: %T", response.Body)
	}

	// Combine header and body
	payload := append(headerBytes, bodyBytes...)

	// Add length prefix
	result := make([]byte, 4+len(payload))
	binary.BigEndian.PutUint32(result[0:4], uint32(len(payload)))
	copy(result[4:], payload)

	logger.Debug("Encoded Kafka response",
		zap.Int32("correlationId", response.Header.CorrelationID),
		zap.Int("totalLen", len(result)),
	)

	return result, nil
}

// encodeRequestHeader encodes a Kafka request header to bytes
func encodeRequestHeader(header *kafka.RequestHeader) ([]byte, error) {
	// Calculate header size: 2 (apiKey) + 2 (version) + 4 (correlationID) + 2 (clientID len) + clientID
	clientIDBytes := []byte(header.ClientID)
	headerSize := 2 + 2 + 4 + 2 + len(clientIDBytes)

	result := make([]byte, headerSize)
	offset := 0

	// API Key (2 bytes)
	binary.BigEndian.PutUint16(result[offset:offset+2], uint16(header.ApiKey))
	offset += 2

	// API Version (2 bytes)
	binary.BigEndian.PutUint16(result[offset:offset+2], uint16(header.ApiVersion))
	offset += 2

	// Correlation ID (4 bytes)
	binary.BigEndian.PutUint32(result[offset:offset+4], uint32(header.CorrelationID))
	offset += 4

	// Client ID length (2 bytes) + Client ID
	binary.BigEndian.PutUint16(result[offset:offset+2], uint16(len(clientIDBytes)))
	offset += 2
	copy(result[offset:], clientIDBytes)

	return result, nil
}
