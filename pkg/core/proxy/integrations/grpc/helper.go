package grpc

import (
	"go.keploy.io/server/v2/pkg/models"
)

// createLengthPrefixedMessage creates a GrpcLengthPrefixedMessage from a raw message payload.
// The gRPC framework handles the actual 5-byte wire protocol prefix. This struct
// is for Keploy's internal representation and matching.
func createLengthPrefixedMessage(data []byte) models.GrpcLengthPrefixedMessage {
	// The original implementation stored the raw bytes as a string, which can
	// safely hold binary data in Go. We will follow this for consistency with
	// the existing fuzzy matching logic.
	return models.GrpcLengthPrefixedMessage{
		// Compression flag is 0 for uncompressed.
		CompressionFlag: 0,
		// MessageLength is the length of the raw data.
		MessageLength: uint32(len(data)),
		// DecodedData holds the raw data, cast to a string.
		DecodedData: string(data),
	}
}

// createPayloadFromLengthPrefixedMessage extracts the raw message payload from a GrpcLengthPrefixedMessage.
func createPayloadFromLengthPrefixedMessage(msg models.GrpcLengthPrefixedMessage) ([]byte, error) {
	// The data is stored as a string, so we just cast it back to a byte slice.
	return []byte(msg.DecodedData), nil
}
