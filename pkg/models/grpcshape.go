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
// fields, field 2 of the message in field 1 being under field 1. Protoscope
// prints a string whose bytes happen to parse as protobuf as a message, so
// such a string counts as one only when it parses into length-delimited
// fields of its own, which is rare.
//
// A direction's shape is the union of its messages' (a stream's length and
// order are no shape), its root marked "m" when it carries any message at
// all, so a direction with no message differs from one whose messages hold
// no length-delimited field. A message is read in full, however deep it
// nests, without recursion.

// grpcMessagesShape is the shape of a direction's messages, written out
// (shapeNode.String): "" for none.
func grpcMessagesShape(msgs []GrpcLengthPrefixedMessage) string {
	if len(msgs) == 0 {
		return ""
	}
	root := &shapeNode{types: shapeMessage}
	for _, m := range msgs {
		protoscopeShape(m.DecodedData, root)
	}
	return root.String()
}

// protoscopeShape adds the length-delimited fields of a message's protoscope
// text to root.
func protoscopeShape(data string, root *shapeNode) {
	stack := []*shapeNode{root} // the embedded messages open at i
	n := len(data)
	for i := 0; i < n; {
		c := data[i]
		switch {
		case isProtoscopeSpace(c):
			i++
		case c == '}':
			if len(stack) > 1 {
				stack = stack[:len(stack)-1]
			}
			i++
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
			var opened *shapeNode
			i, opened = protoscopeField(data, i, stack[len(stack)-1])
			if opened != nil {
				stack = append(stack, opened)
			}
		default:
			// Any other token (a long-form marker, a scalar's residue).
			for i < n && !isProtoscopeSpace(data[i]) && data[i] != '}' {
				i++
			}
		}
	}
}

// protoscopeField reads one "number: value" field at i, under parent, and
// returns the position after what it read, adding the field when its value
// is length-delimited. When the value is an embedded message, it returns the
// field's node, whose fields follow at that position, up to its '}'.
func protoscopeField(data string, i int, parent *shapeNode) (int, *shapeNode) {
	n := len(data)
	start := i
	for i < n && isDigitByte(data[i]) {
		i++
	}
	number := data[start:i]
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
		return i, nil
	}
	switch data[i] {
	case '{':
		field := parent.field(number)
		field.types |= shapeLengthDelimited
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
			return min(i+1, n), nil
		}
		return i, field
	case '`':
		parent.field(number).types |= shapeLengthDelimited
		i++
		for i < n && data[i] != '`' {
			i++
		}
		return min(i+1, n), nil
	}
	// A scalar: its token runs to whitespace or the enclosing block's '}'.
	for i < n && !isProtoscopeSpace(data[i]) && data[i] != '}' {
		i++
	}
	return i, nil
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
	return min(i, n)
}

func isProtoscopeSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }
