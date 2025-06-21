//go:build linux

package grpc

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
// own logical line.  This makes the downstream parser tolerant of lines such
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
// the .proto file.  It is good enough for inspection & matching.
func prettyPrintWire(b []byte, indent int) string {
	var buf bytes.Buffer
	indentPrefix := strings.Repeat("  ", indent)
	writeIndent := func() { buf.WriteString(indentPrefix) }

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
			// render printable ASCII as   1: "foo"
			// (no extra braces that confuse the round-trip parser)
			if isPrintableASCII(v) {
				buf.WriteString(fmt.Sprintf("%q\n", string(v))) // -> "foo"
				break
			}
			// otherwise *then* try interpreting it as a nested wire-message
			if nested := prettyPrintWire(v, indent+1); strings.Contains(nested, ":") {
				buf.WriteString("{\n")
				buf.WriteString(nested)
				writeIndent()
				buf.WriteString("}\n")
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
	var out []byte

	for *idx < len(lines) {
		line := strings.TrimSpace(lines[*idx])
		*idx++

		if line == "" { // skip blanks
			continue
		}
		if line == "}" { // end of embedded message
			return out, nil
		}

		colon := strings.IndexByte(line, ':')
		if colon == -1 {
			return nil, fmt.Errorf("pretty decode: malformed line %q", line)
		}

		// ── field number ───────────────────────────────────────────
		fieldStr := strings.TrimSpace(line[:colon])
		n64, err := strconv.ParseUint(fieldStr, 10, 64)
		if err != nil || n64 == 0 || n64 > maxProtoNum {
			return nil, fmt.Errorf("pretty decode: invalid field %q", fieldStr)
		}
		num := protowire.Number(n64)

		rest := strings.TrimSpace(line[colon+1:])

		// Try each encoding in a stable order; DO NOT mutate `rest`
		// until we've decided what it is.

		// 1️⃣ start of embedded message
		if rest == "{" {
			sub, err := parseMsg(lines, idx)
			if err != nil {
				return nil, err
			}
			out = append(out, protowire.AppendTag(nil, num, protowire.BytesType)...)
			out = protowire.AppendBytes(out, sub)
			continue
		}

		// 2️⃣ ASCII string
		//     legacy form  {"foo"}
		//     new form     "foo"
		if strings.HasPrefix(rest, "{\"") || strings.HasPrefix(rest, "\"") {
			// Strip the optional leading '{'
			if rest[0] == '{' {
				rest = rest[1:]
			}
			// Find the last closing quote on this line.
			endQuote := strings.LastIndex(rest, "\"")
			if endQuote == -1 {
				return nil, fmt.Errorf("pretty decode: unterminated quote in %q", rest)
			}

			// Extract the bytes inside {" … "}
			str := rest[1:endQuote]
			out = append(out, protowire.AppendTag(nil, num, protowire.BytesType)...)
			out = protowire.AppendBytes(out, []byte(str))
			continue
		}

		// 3️⃣ hex blob 0xCAFEBABE   (allow trailing } for inline close)
		hexRest := strings.TrimSuffix(rest, "}")
		if strings.HasPrefix(hexRest, "0x") || strings.HasPrefix(hexRest, "0X") {
			bin, err := hex.DecodeString(hexRest[2:])
			if err == nil {
				out = append(out, protowire.AppendTag(nil, num, protowire.BytesType)...)
				out = protowire.AppendBytes(out, bin)
				if hexRest != rest { // had an inline '}'
					return out, nil
				}
				continue
			}
		}

		// 4️⃣ varint (optionally followed by inline '}')
		trailingClose := strings.HasSuffix(rest, "}")
		if trailingClose {
			rest = strings.TrimSpace(rest[:len(rest)-1])
		}
		val, err := strconv.ParseUint(rest, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("pretty decode: %w", err)
		}
		out = append(out, protowire.AppendTag(nil, num, protowire.VarintType)...)
		out = protowire.AppendVarint(out, val)
		if trailingClose {
			return out, nil
		}
	}
	return out, nil
}

// ---------------------------------------------------------------------
// replace the old “cast-back” helper with the decoder above
func createPayloadFromLengthPrefixedMessage(msg models.GrpcLengthPrefixedMessage) ([]byte, error) {
	return parsePrettyWire(msg.DecodedData)
}
