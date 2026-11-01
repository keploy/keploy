package models

// The field paths of a gRPC direction's messages, as a test case's shape keeps
// them. A message is held as protoscope text (DecodedData): one field per
// "number: value", a length-delimited value (a string, bytes, an embedded
// message, a packed or repeated field) written in braces or as a `hex`
// literal, a scalar as a token.
//
// Only length-delimited fields count. proto3 leaves a scalar at its default
// (0, false, an enum's first value) off the wire, so a scalar's presence
// says what a value was, not what the message is. A length-delimited field
// counts by its number and, for an embedded message, its own length-delimited
// fields, "1.2" being field 2 of the message in field 1. Protoscope prints a
// string whose bytes happen to parse as protobuf as a message, so such a
// string counts as one only when it parses into length-delimited fields of
// its own, which is rare.
//
// A direction's entries are the union of its messages' (a stream's length
// and order are no shape), plus "$:m" when it carries any message at all, so
// a direction with no message differs from one whose messages hold no
// length-delimited field.

// protoscopeMaxDepth bounds how deep the shape follows embedded messages; the
// fields of a deeper one do not count. It keeps a crafted message from
// exhausting the stack.
const protoscopeMaxDepth = 64

// grpcMessagesShape is the entries of a direction's messages.
func grpcMessagesShape(msgs []GrpcLengthPrefixedMessage) []string {
	if len(msgs) == 0 {
		return nil
	}
	set := map[string]struct{}{}
	addShape(set, "$", "m")
	for _, m := range msgs {
		protoscopeFields(m.DecodedData, 0, "$", set, 0)
	}
	return sortedKeys(set)
}

// protoscopeFields adds the length-delimited fields of the protoscope block
// that starts at i, under path, and returns the position after the block's
// closing '}' (or the end of the text).
func protoscopeFields(data string, i int, path string, set map[string]struct{}, depth int) int {
	n := len(data)
	for i < n {
		c := data[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			i++
		case c == '}':
			return i + 1
		case c == '#':
			// A remark (protoscope appends one to fixed-width scalars) runs to
			// the end of its line.
			for i < n && data[i] != '\n' {
				i++
			}
		case c == '"':
			i = skipProtoscopeString(data, i)
		case c == '{':
			// A block with no field number: skip it whole.
			i = skipProtoscopeBlock(data, i+1)
		case isDigitByte(c):
			i = protoscopeField(data, i, path, set, depth)
		default:
			// Any other token (a long-form marker, a scalar's residue).
			for i < n && !isProtoscopeSpace(data[i]) && data[i] != '}' {
				i++
			}
		}
	}
	return i
}

// protoscopeField reads one "number: value" field at i and returns the
// position after it, adding it when its value is length-delimited.
func protoscopeField(data string, i int, path string, set map[string]struct{}, depth int) int {
	n := len(data)
	start := i
	for i < n && isDigitByte(data[i]) {
		i++
	}
	field := path + "." + data[start:i]
	for i < n && data[i] == ' ' {
		i++
	}
	if i < n && data[i] == ':' {
		i++
	}
	for i < n && data[i] == ' ' {
		i++
	}
	if i < n && data[i] == '!' { // a group
		i++
	}
	if i >= n {
		return i
	}
	switch data[i] {
	case '{':
		addShape(set, field, "l")
		i++
		for i < n && isProtoscopeSpace(data[i]) {
			i++
		}
		if i < n && data[i] == '"' {
			// A string: one or more quoted literals, then '}'.
			for i < n && data[i] != '}' {
				if data[i] == '"' {
					i = skipProtoscopeString(data, i)
					continue
				}
				i++
			}
			return min(i+1, n)
		}
		if depth >= protoscopeMaxDepth {
			return skipProtoscopeBlock(data, i)
		}
		return protoscopeFields(data, i, field, set, depth+1)
	case '`':
		addShape(set, field, "l")
		i++
		for i < n && data[i] != '`' {
			i++
		}
		return min(i+1, n)
	}
	// A scalar: its token runs to whitespace or the enclosing block's '}'.
	for i < n && !isProtoscopeSpace(data[i]) && data[i] != '}' {
		i++
	}
	return i
}

// skipProtoscopeString returns the position after the quoted literal at i.
func skipProtoscopeString(data string, i int) int {
	n := len(data)
	i++
	for i < n {
		switch data[i] {
		case '\\':
			i += 2
			continue
		case '"':
			return i + 1
		}
		i++
	}
	return n
}

// skipProtoscopeBlock returns the position after the '}' that closes the
// block whose content starts at i.
func skipProtoscopeBlock(data string, i int) int {
	n := len(data)
	depth := 1
	for i < n && depth > 0 {
		switch data[i] {
		case '"':
			i = skipProtoscopeString(data, i)
			continue
		case '{':
			depth++
		case '}':
			depth--
		}
		i++
	}
	return i
}

func isProtoscopeSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }
