package http

import "bytes"

// headerFields calls each with the name and value of every field of head, a
// message's header section as messageHead returns it, in order. Lines end at
// an LF, a CR before it dropped; the section ends at its first empty line. A
// name and a value are trimmed of white space. The start line is read as the
// other lines are: with no colon (a status line, most request lines) it is no
// field, and with one it is a field no caller looks for (its name holds the
// method and a space).
//
// A line that begins with a space or a tab continues the line before it
// (obsolete line folding, RFC 9112 §5.2): it is joined to that line's value
// with a space, as net/http's reader joins it and as the RFC has a user agent
// do, and is never a field of its own. So "X-A: b\r\n Content-Length: -5" is
// the one field X-A, whose value is "b Content-Length: -5": a message framed
// by a Content-Length read off a folded line was framed where its peer does
// not frame it. A folded line that continues a line with no colon, such as
// a status line, continues no field and is passed over with it (RFC 9112
// §2.2 has a recipient that does not refuse such lines ignore them; net/http
// refuses the message).
func headerFields(head []byte, each func(name, value []byte)) {
	var name, value []byte
	field := false // name and value are a field, not a line with no colon
	for _, line := range bytes.Split(head, []byte("\n")) {
		line = bytes.TrimRight(line, "\r")
		if len(line) == 0 {
			break
		}
		if line[0] == ' ' || line[0] == '\t' {
			// The cap keeps append from writing into head after value.
			value = append(append(value[:len(value):len(value)], ' '), bytes.TrimSpace(line)...)
			continue
		}
		if field {
			each(bytes.TrimSpace(name), bytes.TrimSpace(value))
		}
		name, value, field = bytes.Cut(line, []byte(":"))
	}
	if field {
		each(bytes.TrimSpace(name), bytes.TrimSpace(value))
	}
}
