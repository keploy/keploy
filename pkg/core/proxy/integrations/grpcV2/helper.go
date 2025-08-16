//go:build linux

package grpcV2

import (
	"go.keploy.io/server/v2/pkg/models"
	"github.com/protocolbuffers/protoscope"
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
		// DecodedData holds the text representation of the wire data.
		DecodedData: prettyPrintWire(data, 0),
	}
}

// createPayloadFromLengthPrefixedMessage decodes the pretty-printed data
// from a message back into its raw binary payload.
func createPayloadFromLengthPrefixedMessage(msg models.GrpcLengthPrefixedMessage) ([]byte, error) {
	return parsePrettyWire(msg.DecodedData)
}

// prettyPrintWire renders any protobuf wire payload without needing the .proto file.
// It uses protoscope for a robust, standardized text format.
func prettyPrintWire(b []byte, _ int) string {
	// The indent argument is ignored as protoscope handles formatting automatically.
	return protoscope.Write(b, protoscope.WriterOptions{})
}

// parsePrettyWire decodes the human-readable text format from protoscope
// back into its binary wire format.
func parsePrettyWire(s string) ([]byte, error) {
	scanner := protoscope.NewScanner(s)
	// The scanner.Exec() method handles the entire parsing process.
	return scanner.Exec()
}
