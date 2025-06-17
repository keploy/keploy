package grpc

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"strings"

	"go.keploy.io/server/v2/pkg/core/proxy/integrations/util"
	"go.keploy.io/server/v2/pkg/models"
	"google.golang.org/protobuf/encoding/protowire"
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
		DecodedData: prettyPrintWire(data, 0), // <-- new
	}
}

// createPayloadFromLengthPrefixedMessage extracts the raw message payload from a GrpcLengthPrefixedMessage.
func createPayloadFromLengthPrefixedMessage(msg models.GrpcLengthPrefixedMessage) ([]byte, error) {
	// The data is stored as a string, so we just cast it back to a byte slice.
	// Return a copy so the caller can mutate safely.
	buf := make([]byte, len(msg.DecodedData))
	copy(buf, msg.DecodedData)
	return buf, nil
}

// prettyPrintWire renders *any* protobuf wire payload without needing
// the .proto file.  It is good enough for inspection & matching.
func prettyPrintWire(b []byte, indent int) string {
	var buf bytes.Buffer
	writeIndent := func() { buf.WriteString(strings.Repeat("  ", indent)) }

	for len(b) > 0 {
		num, wt, n := protowire.ConsumeTag(b)
		if n < 0 { // malformed → raw hex
			buf.WriteString(hex.EncodeToString(b))
			break
		}
		b = b[n:]
		writeIndent()
		buf.WriteString(fmt.Sprintf("%d: ", num))

		switch wt {
		case protowire.VarintType:
			v, m := protowire.ConsumeVarint(b)
			b = b[m:]
			buf.WriteString(fmt.Sprintf("%d\n", v))
		case protowire.Fixed32Type:
			v, m := protowire.ConsumeFixed32(b)
			b = b[m:]
			buf.WriteString(fmt.Sprintf("%d\n", v))
		case protowire.Fixed64Type:
			v, m := protowire.ConsumeFixed64(b)
			b = b[m:]
			buf.WriteString(fmt.Sprintf("%d\n", v))
		case protowire.BytesType:
			v, m := protowire.ConsumeBytes(b)
			b = b[m:]
			// try nested-message first
			nested := prettyPrintWire(v, indent+1)
			if strings.Contains(nested, ":") {
				buf.WriteString("{\n")
				buf.WriteString(nested)
				writeIndent()
				buf.WriteString("}\n")
			} else if util.IsASCII(string(v)) {
				buf.WriteString(fmt.Sprintf("{\"%s\"}\n", string(v)))
			} else {
				buf.WriteString("0x" + hex.EncodeToString(v) + "\n")
			}
		default:
			buf.WriteString(hex.EncodeToString(b) + "\n")
			b = nil
		}
	}
	return strings.TrimRight(buf.String(), "\n")
}
