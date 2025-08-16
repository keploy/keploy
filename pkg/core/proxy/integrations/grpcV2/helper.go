//go:build linux

package grpcV2

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"

	"go.keploy.io/server/v2/pkg/models"
	"google.golang.org/protobuf/encoding/protowire"
)

// tokenizePrettyLines splits the pretty-printed wire output into a slice in
// which **every** closing brace that is outside a quoted string becomes its
// own logical line. This makes the downstream parser tolerant of lines such
// as:
//
//	1: {"foo"}  }}
//
// which previously produced “pretty decode: strconv.ParseUint … invalid
// syntax”.
func tokenizePrettyLines(s string) []string {
	raw := strings.Split(strings.TrimSpace(s), "\n")
	var out []string

	for _, l := range raw {
		inQuotes := false
		var buf strings.Builder

		for _, r := range l {
			switch r {
			case '"':
				inQuotes = !inQuotes
				buf.WriteRune(r)
			case '}':
				if inQuotes {
					buf.WriteRune(r)
					continue
				}
				if tok := strings.TrimSpace(buf.String()); tok != "" {
					out = append(out, tok)
				}
				out = append(out, "}") // the brace itself
				buf.Reset()
			default:
				buf.WriteRune(r)
			}
		}
		if tok := strings.TrimSpace(buf.String()); tok != "" {
			out = append(out, tok)
		}
	}
	return out
}

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

// prettyPrintWire renders *any* protobuf wire payload without needing
// the .proto file. It is good enough for inspection & matching.
// This function implements a schema-less disassembly of the wire format,
// producing a human-readable output that can be losslessly parsed back
// into the original byte sequence by parsePrettyWire.
func prettyPrintWire(b []byte, indent int) string {
	var out strings.Builder
	indentStr := strings.Repeat("  ", indent)

	// spew.Dump(b)

	for len(b) > 0 {
		// protowire.ConsumeField is the key helper. It parses a complete
		// field (tag + value) and returns its total length, which simplifies
		// advancing our byte slice `b`.
		num, typ, n := protowire.ConsumeField(b)
		if n < 0 {
			// If we can't parse a valid field, the rest of the buffer is
			// treated as raw, unparsable data. We'll represent it as a
			// hex literal, similar to Protoscope's `...` syntax.
			out.WriteString(fmt.Sprintf("%s`%s`\n", indentStr, hex.EncodeToString(b)))
			break
		}

		// Isolate the raw bytes for just the value part of the field.
		// We need to re-parse the tag to find out how long it was.
		_, _, tagN := protowire.ConsumeTag(b)
		valueBytes := b[tagN:n]

		// Write the field number and a colon, e.g., "1: ".
		out.WriteString(fmt.Sprintf("%s%d: ", indentStr, num))

		switch typ {
		case protowire.VarintType:
			// For varints, we just decode and print the uint64 value.
			// Without a schema, we can't know if it's an int32, sint64, bool, etc.
			// so we stick to the basic numeric representation.
			v, _ := protowire.ConsumeVarint(valueBytes)
			out.WriteString(fmt.Sprintf("%d\n", v))

		case protowire.Fixed32Type:
			// For fixed 32-bit values, we represent them as a hex literal
			// with an 'i32' suffix. This is unambiguous and can represent
			// fixed32, sfixed32, or float.
			v, _ := protowire.ConsumeFixed32(valueBytes)
			out.WriteString(fmt.Sprintf("0x%xi32\n", v))

		case protowire.Fixed64Type:
			// Similarly, fixed 64-bit values get a hex literal with an
			// 'i64' suffix for fixed64, sfixed64, or double.
			v, _ := protowire.ConsumeFixed64(valueBytes)
			out.WriteString(fmt.Sprintf("0x%xi64\n", v))

		case protowire.BytesType:
			// This is the most complex type, as it can be a string, bytes,
			// a nested message, or a packed repeated field.
			innerBytes, _ := protowire.ConsumeBytes(valueBytes)

			// Heuristic: If the content is all printable ASCII, format it as a
			// quoted string. This is a good guess for string fields.
			if isPrintableASCII(innerBytes) {
				// Use strconv.Quote for safe, standard-library string escaping.
				out.WriteString(strconv.Quote(string(innerBytes)) + "\n")
			} else {
				// Otherwise, we assume it's a nested message or packed field.
				// We format it recursively inside curly braces `{}`.
				// If it's not a valid message, the recursive call will just
				// render it as a hex literal inside the braces, which is fine.
				out.WriteString("{\n")
				out.WriteString(prettyPrintWire(innerBytes, indent+1))
				out.WriteString(fmt.Sprintf("%s}\n", indentStr))
			}

		case protowire.StartGroupType:
			// Groups are a deprecated feature but we handle them for completeness.
			// The valueBytes contain the group's content AND its end marker.
			// We find the end marker and recursively print the content inside `!{}`.
			endTagLen := protowire.SizeTag(num)
			contentBytes := valueBytes[:len(valueBytes)-endTagLen]

			out.WriteString("!{\n")
			out.WriteString(prettyPrintWire(contentBytes, indent+1))
			out.WriteString(fmt.Sprintf("%s}\n", indentStr))

		case protowire.EndGroupType:
			// This case should not be hit if ConsumeField is used, as it consumes
			// the entire group. This is a defensive measure.
			out.WriteString("<Unexpected EndGroup>\n")

		default:
			// Unknown wire types are dumped as hex.
			out.WriteString(fmt.Sprintf("<UnknownType %d> `%s`\n", typ, hex.EncodeToString(valueBytes)))
		}

		// Advance the buffer to the next field.
		b = b[n:]
	}
	return out.String()
}

// isPrintableASCII returns true only if every byte is between 0x20 and 0x7E.
// This excludes control characters like 0x08 that confused the earlier test.
func isPrintableASCII(b []byte) bool {
	for _, c := range b {
		if c < 0x20 || c > 0x7e {
			return false
		}
	}
	return len(b) > 0
}

const maxProtoNum = uint64(protowire.MaxValidNumber) // 1<<29 - 1

func parsePrettyWire(s string) ([]byte, error) {
	if strings.TrimSpace(s) == "" {
		return nil, nil // nothing to decode
	}
	lines := tokenizePrettyLines(s)
	var idx int
	return parseMsg(lines, &idx)
}

func parseMsg(lines []string, idx *int) ([]byte, error) {
	var buf bytes.Buffer

	for *idx < len(lines) {
		line := lines[*idx]
		*idx++

		if line == "}" || line == "!}" {
			return buf.Bytes(), nil // End of the current message/group
		}

		parts := strings.SplitN(line, ": ", 2)
		if len(parts) != 2 {
			// This could be a raw hex or string literal without a field number,
			// which can happen inside a packed field.
			if strings.HasPrefix(line, "`") && strings.HasSuffix(line, "`") {
				// Hex literal: `...`
				data, err := hex.DecodeString(line[1 : len(line)-1])
				if err != nil {
					return nil, fmt.Errorf("pretty decode: invalid hex literal on line %d: %s", *idx, line)
				}
				buf.Write(data)
			} else if strings.HasPrefix(line, "\"") && strings.HasSuffix(line, "\"") {
				// String literal: "..."
				s, err := strconv.Unquote(line)
				if err != nil {
					return nil, fmt.Errorf("pretty decode: invalid string literal on line %d: %s", *idx, line)
				}
				buf.Write([]byte(s))
			} else {
				// Assume it's a varint inside a packed field.
				val, err := strconv.ParseUint(line, 10, 64)
				if err != nil {
					return nil, fmt.Errorf("pretty decode: expected packed varint on line %d: %s", *idx, line)
				}
				buf.Write(protowire.AppendVarint(nil, val))
			}
			continue
		}

		// We have a standard "field_number: value" line.
		fieldNumStr, valueStr := parts[0], parts[1]
		fieldNum, err := strconv.ParseUint(fieldNumStr, 10, 64)
		if err != nil || fieldNum > maxProtoNum {
			return nil, fmt.Errorf("pretty decode: invalid field number on line %d: %s", *idx, fieldNumStr)
		}

		switch {
		case valueStr == "{":
			// Length-prefixed message/bytes
			content, err := parseMsg(lines, idx)
			if err != nil {
				return nil, err
			}
			buf.Write(protowire.AppendTag(nil, protowire.Number(fieldNum), protowire.BytesType))
			buf.Write(protowire.AppendBytes(nil, content))

		case valueStr == "!{":
			// Start of a group
			content, err := parseMsg(lines, idx)
			if err != nil {
				return nil, err
			}
			buf.Write(protowire.AppendTag(nil, protowire.Number(fieldNum), protowire.StartGroupType))
			buf.Write(content)
			buf.Write(protowire.AppendTag(nil, protowire.Number(fieldNum), protowire.EndGroupType))

		case strings.HasPrefix(valueStr, "0x") && strings.HasSuffix(valueStr, "i32"):
			// Fixed32 value
			val, err := strconv.ParseUint(valueStr[2:len(valueStr)-3], 16, 32)
			if err != nil {
				return nil, fmt.Errorf("pretty decode: invalid i32 on line %d: %s", *idx, valueStr)
			}
			buf.Write(protowire.AppendTag(nil, protowire.Number(fieldNum), protowire.Fixed32Type))
			buf.Write(protowire.AppendFixed32(nil, uint32(val)))

		case strings.HasPrefix(valueStr, "0x") && strings.HasSuffix(valueStr, "i64"):
			// Fixed64 value
			val, err := strconv.ParseUint(valueStr[2:len(valueStr)-3], 16, 64)
			if err != nil {
				return nil, fmt.Errorf("pretty decode: invalid i64 on line %d: %s", *idx, valueStr)
			}
			buf.Write(protowire.AppendTag(nil, protowire.Number(fieldNum), protowire.Fixed64Type))
			buf.Write(protowire.AppendFixed64(nil, val))

		case strings.HasPrefix(valueStr, "\""):
			// Quoted string value
			s, err := strconv.Unquote(valueStr)
			if err != nil {
				return nil, fmt.Errorf("pretty decode: invalid string on line %d: %s", *idx, valueStr)
			}
			buf.Write(protowire.AppendTag(nil, protowire.Number(fieldNum), protowire.BytesType))
			buf.Write(protowire.AppendBytes(nil, []byte(s)))

		default:
			// Assume it's a varint
			val, err := strconv.ParseUint(valueStr, 10, 64)
			if err != nil {
				return nil, fmt.Errorf("pretty decode: invalid varint on line %d: %s", *idx, valueStr)
			}
			buf.Write(protowire.AppendTag(nil, protowire.Number(fieldNum), protowire.VarintType))
			buf.Write(protowire.AppendVarint(nil, val))
		}
	}

	return buf.Bytes(), nil
}

// ---------------------------------------------------------------------
// replace the old “cast-back” helper with the decoder above
func createPayloadFromLengthPrefixedMessage(msg models.GrpcLengthPrefixedMessage) ([]byte, error) {
	return parsePrettyWire(msg.DecodedData)
}
