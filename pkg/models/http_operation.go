package models

import (
	"sync/atomic"
)

// An HTTP request can carry an operation of its own inside its body: a
// GraphQL request posts every query and mutation to one URL. keploy names an
// HTTP call by its method and URL, so without a reader every such operation
// is one call ("POST /query"). A reader registered here (enterprise's
// GraphQL parser) names the operation, and the call carries the name in a
// field of its own, beside its summary: a miss's Operation
// (MockMismatchReport, UnmatchedCall), a recording's MockOperation. No
// summary, shape or identity holds it, so every reader of a summary reads it
// as before, and nothing reads a label back out of text. A line written for
// a person names it after the call (WithHTTPOperation):
// "POST /query {query GetSubscription}".
//
// With no reader registered, nothing reads a body here, and no call carries
// an operation.

// HTTPOperation is what a registered reader found inside an HTTP request.
type HTTPOperation struct {
	// Label names the operation, as validOperationLabel takes it: "query
	// GetSubscription", "mutation CreateApp".
	Label string
}

// httpOperationReader is the registered reader, nil when there is none.
var httpOperationReader atomic.Pointer[func(*HTTPReq) (HTTPOperation, bool)]

// RegisterHTTPOperation registers of as the reader of the operation an HTTP
// request carries: false when the request carries none it reads. Call it
// from an init function. There is one reader: a nil one, or a second, panics.
// The returned function removes it (tests).
func RegisterHTTPOperation(of func(*HTTPReq) (HTTPOperation, bool)) (unregister func()) {
	if of == nil {
		panic("models: RegisterHTTPOperation with a nil reader")
	}
	p := &of
	if !httpOperationReader.CompareAndSwap(nil, p) {
		panic("models: a second HTTP operation reader registered")
	}
	return func() { httpOperationReader.CompareAndSwap(p, nil) }
}

// HTTPOperationOf is the operation r carries, as the registered reader reads
// it: false when no reader is registered, r is nil, the reader reads none in
// it, or the label it gives is not one validOperationLabel takes (then r is
// named as if it carried none).
func HTTPOperationOf(r *HTTPReq) (HTTPOperation, bool) {
	of := httpOperationReader.Load()
	if of == nil || r == nil {
		return HTTPOperation{}, false
	}
	op, ok := (*of)(r)
	if !ok || !validOperationLabel(op.Label) {
		return HTTPOperation{}, false
	}
	return op, true
}

// HTTPOperationLabel is the label of the operation r carries
// (HTTPOperationOf), or "" when it carries none.
func HTTPOperationLabel(r *HTTPReq) string {
	op, _ := HTTPOperationOf(r)
	return op.Label
}

// MockOperation is the label of the operation m's HTTP request carries
// (HTTPOperationLabel), or "" when m is nil, has no HTTP request, or its
// request carries none.
func MockOperation(m *Mock) string {
	if m == nil {
		return ""
	}
	return HTTPOperationLabel(m.Spec.HTTPReq)
}

// WithHTTPOperation is a line's name for an HTTP call: the call's summary or
// shape followed by the label of the operation it carries, call alone when
// label is "". It is for a person to read; nothing parses it.
func WithHTTPOperation(call, label string) string {
	if label == "" {
		return call
	}
	return call + " {" + label + "}"
}

// Named is m as a line for a person names it: its summary followed by the
// label of the operation it carries (WithHTTPOperation).
func (m UnmatchedCall) Named() string {
	return WithHTTPOperation(m.ActualSummary, m.Operation)
}

// SanitizedOperation is the operation a sanitized call keeps: label when it
// is one validOperationLabel takes, else "". The reader's labels are checked
// when they are read (HTTPOperationOf); a call decoded from elsewhere, or
// kept by another sanitizer, is checked again with this.
func SanitizedOperation(label string) string {
	if !validOperationLabel(label) {
		return ""
	}
	return label
}

// maxOperationLabel is the longest label an operation is named by.
const maxOperationLabel = 200

// validOperationLabel reports whether l can name an operation in a summary and
// a shape: at most maxOperationLabel bytes; it starts with a letter and does
// not end with a space; it holds only letters, digits, '_', ' ', ',', ':',
// '.', '{' and '}' (the names a document gives and the words around them, no
// value the request sent); and its braces pair up in order, so the brace a
// line closes it with (WithHTTPOperation) ends it.
func validOperationLabel(l string) bool {
	if l == "" || len(l) > maxOperationLabel || !isLetterByte(l[0]) || l[len(l)-1] == ' ' {
		return false
	}
	depth := 0
	for i := 0; i < len(l); i++ {
		c := l[i]
		switch {
		case c == '{':
			depth++
		case c == '}':
			if depth--; depth < 0 {
				return false
			}
		case isLetterByte(c), c >= '0' && c <= '9', c == '_', c == ' ', c == ',', c == ':', c == '.':
		default:
			return false
		}
	}
	return depth == 0
}

// isLetterByte reports whether c is an ASCII letter.
func isLetterByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}
