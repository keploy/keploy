package http

import (
	"bytes"
	"fmt"
	"net/http"
	"net/url"
	"strconv"

	"golang.org/x/net/http/httpguts"
)

// headerFields calls each with the name and value of every field of head, a
// message's header section as messageHead returns it (its start line, its
// field lines, the empty line that ends it), in order, and returns
// errFramingRefused for a section net/http's reader refuses (textproto's
// ReadMIMEHeader) or ends elsewhere. Lines end at an LF, the one CR before it
// dropped; the start line is no field. Each line is trimmed of the spaces and
// tabs at its ends, and a value of those at its start, as net/http's reader
// trims them (RFC 9110 §5.5); a name is passed as it was written before its
// colon, spaces and all (parseHeaders refuses a framing field written so).
//
// A line that begins with a space or a tab continues the line before it
// (obsolete line folding, RFC 9112 §5.2): it is joined to that line's value
// with a space, as net/http's reader joins it and as the RFC has a user agent
// do, and is never a field of its own. So "X-A: b\r\n Content-Length: -5" is
// the one field X-A, whose value is "b Content-Length: -5": a message framed
// by a Content-Length read off a folded line was framed where its peer does
// not frame it.
//
// It refuses, as net/http's reader does:
//   - a folded line right after the start line, which continues no field;
//   - a line with no colon (before the lines folded onto it);
//   - a name that is empty or holds a byte that is not a token byte or a
//     space (a tab before the colon is one);
//   - a value byte that is a control byte other than a tab (a CR left in a
//     line, a NUL) or DEL.
//
// The section ends at its first empty line, as messageHead ends head there.
func headerFields(head []byte, each func(name, value []byte)) error {
	var name, value []byte
	field := false // name and value are a field, not yet given to each
	give := func() error {
		if !field {
			return nil
		}
		if !validFieldName(name) || !httpguts.ValidHeaderFieldValue(string(value)) {
			return fmt.Errorf("%w: a field line net/http's reader refuses", errFramingRefused)
		}
		each(name, bytes.TrimLeft(value, " \t"))
		return nil
	}
	_, rest, _ := bytes.Cut(head, []byte("\n")) // the start line is no field
	for len(rest) > 0 {
		var line []byte
		line, rest, _ = bytes.Cut(rest, []byte("\n"))
		line = bytes.TrimSuffix(line, []byte("\r"))
		if len(line) == 0 {
			break // the empty line that ends the section
		}
		if line[0] == ' ' || line[0] == '\t' {
			if !field {
				return fmt.Errorf("%w: a folded line right after the start line", errFramingRefused)
			}
			// The cap keeps append from writing into head after value.
			value = append(append(value[:len(value):len(value)], ' '), trimWhiteSpace(line)...)
			continue
		}
		if err := give(); err != nil {
			return err
		}
		if name, value, field = bytes.Cut(trimWhiteSpace(line), []byte(":")); !field {
			return fmt.Errorf("%w: a field line with no colon", errFramingRefused)
		}
	}
	return give()
}

// validFieldName reports whether name, a field's name as written before its
// colon, is one net/http's reader takes: token bytes, and spaces, which make
// it a name no other field has (its client reads "Content-Length : 5" as a
// field "Content-Length ", and its server refuses the request).
func validFieldName(name []byte) bool {
	for _, c := range name {
		if c != ' ' && !httpguts.IsTokenRune(rune(c)) {
			return false
		}
	}
	return len(name) > 0
}

// trimWhiteSpace trims b of the spaces and tabs at its ends, the white space a
// field line is written with (RFC 9110 §5.6.3) and all net/http's reader trims
// of one: a CR left in a line (one ended "\r\r\n") is no white space.
func trimWhiteSpace(b []byte) []byte {
	return bytes.Trim(b, " \t")
}

// asciiEqualFold reports whether a and b are the same ASCII text but for case,
// as net/http compares a field name or a coding: bytes.EqualFold folds Unicode
// too, so it took "chunKed" (a Kelvin sign) for chunked.
func asciiEqualFold(a []byte, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if lowerASCII(a[i]) != lowerASCII(b[i]) {
			return false
		}
	}
	return true
}

func lowerASCII(c byte) byte {
	if 'A' <= c && c <= 'Z' {
		return c + 'a' - 'A'
	}
	return c
}

// startLine is what net/http's reader reads off the start line of head, a
// request's (response false) or a response's header section: its HTTP
// version, the request's method, and ok=false for a start line it refuses,
// as ReadRequest and ReadResponse refuse them. A request line is a method (a
// token), a space, a request target that url.ParseRequestURI takes (CONNECT's
// authority form read as a host), a space and the version; a status line is
// the version, a space and a three-character status code that strconv.Atoi
// reads as zero or more.
func startLine(head []byte, response bool) (major, minor int, method string, ok bool) {
	line, _, _ := bytes.Cut(head, []byte("\n"))
	line = bytes.TrimSuffix(line, []byte("\r"))
	// A line short of a space has an empty version (and status code), which
	// net/http does not read either.
	var proto []byte
	if response {
		var status []byte
		proto, status, _ = bytes.Cut(line, []byte(" "))
		code, _, _ := bytes.Cut(bytes.TrimLeft(status, " "), []byte(" "))
		if n, err := strconv.Atoi(string(code)); len(code) != 3 || err != nil || n < 0 {
			return 0, 0, "", false
		}
	} else {
		m, rest, _ := bytes.Cut(line, []byte(" "))
		target, version, _ := bytes.Cut(rest, []byte(" "))
		method, proto = string(m), version
		if !httpguts.ValidHeaderFieldName(method) {
			return 0, 0, "", false
		}
		rawurl := string(target)
		if method == http.MethodConnect && !bytes.HasPrefix(target, []byte("/")) {
			rawurl = "http://" + rawurl
		}
		if _, err := url.ParseRequestURI(rawurl); err != nil {
			return 0, 0, "", false
		}
	}
	if major, minor, ok = http.ParseHTTPVersion(string(proto)); !ok {
		return 0, 0, "", false
	}
	return major, minor, method, true
}
