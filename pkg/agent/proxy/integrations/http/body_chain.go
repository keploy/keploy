package http

import (
	"bytes"
	"encoding/base64"
	"strings"
	"unicode/utf8"

	"go.keploy.io/server/v3/pkg/matcher/learn"
)

// A request body that is JSON under a whole-body encoding (a browser
// beacon's base64 of a percent-escaped JSON document) is read as that JSON,
// so the learn step's words are the document's and the replay compares it
// field by field, as a JSON body: what changed from run to run is learned
// at its field, and every other field stays compared. The body's encoding
// is read as a chain of layers, in one fixed order: base64, then percent
// escapes, then JSON, each layer but JSON taken only where it holds.
//
// A layer counts only where it decodes the WHOLE text and the text is the
// one form that layer writes for what it decodes to, so no two texts read
// as one document:
//
//   - base64: the standard alphabet with its padding, as btoa writes it,
//     with no line break (the decoder skips CR and LF) and no bit set past
//     the last byte (base64.Encoding.Strict).
//   - percent: escapes as encodeURIComponent writes them, every byte but
//     A-Z a-z 0-9 - _ . ! ~ * ' ( ) escaped, in upper-case hex, and those
//     never escaped. (A text with no escape is all of those bytes, so it is
//     never JSON's object or array: the layer changes nothing there.)
//
// The end is JSON: valid UTF-8 (the JSON decoder would read each invalid
// byte as U+FFFD, so two texts would read as one) holding one object or
// array (a scalar would be the whole body as one leaf). Anything else, at
// any layer, leaves the body compared whole, as before. No body that is
// JSON as it is reads by a chain: JSON's object or array opens with '{',
// '[' or a blank, none of them a letter of base64 or a byte percent escapes
// keep; a JSON value that is base64 opens with a digit, which decodes to a
// first byte of 0xD0 or more; and one whose bytes percent escapes all keep
// is a scalar.
//
// Each layer decodes into one buffer of its own, no larger than the text
// it decodes; nothing is read past the body's bytes.

// bodyChainRoot is the name a decoded body's fields are read under: the
// learned noise (`body(base64>percent>json).id`) and a miss name the
// chain, so a reader sees the body was decoded and how.
func bodyChainRoot(chain string) string {
	return "body(" + chain + ")"
}

// bodyRootOf is the root a body field's name (a walk's shape: body.items[],
// body(base64>json).id) stands under: "body", or a chain's root. A chain's
// name holds no '.' and no '['.
func bodyRootOf(name string) string {
	if i := strings.IndexAny(name, ".["); i >= 0 {
		return name[:i]
	}
	return name
}

// decodedBody is body read through its chain (see above): the chain's
// name, its layers joined by '>' and ending in json, and the document.
// ok is false where body is no JSON object or array under one or more of
// the layers.
func decodedBody(body []byte) (chain string, doc interface{}, ok bool) {
	var layers []string
	text := body
	if b, ok := canonicalBase64(text); ok {
		layers, text = append(layers, "base64"), b
	}
	if p, ok := canonicalPercent(text); ok {
		layers, text = append(layers, "percent"), p
	}
	if len(layers) == 0 || !utf8.Valid(text) {
		return "", nil, false
	}
	v, ok := learn.DecodeJSON(text)
	if !ok {
		return "", nil, false
	}
	switch v.(type) {
	case map[string]interface{}, []interface{}:
		return strings.Join(append(layers, "json"), ">"), v, true
	}
	return "", nil, false
}

// decodedLeaves adds the leaves of doc, a body read through chain, to out:
// the leaves a JSON body has, each at its path under the chain's root
// (bodyChainRoot). A leaf's noise name stays a JSON body's, the dialect
// keploy.yml's body noise is written in, so keploy.yml reaches the field
// as it reaches a JSON body's, in the learn step and at replay alike.
func decodedLeaves(chain string, doc interface{}, out map[string]learn.Leaf) {
	root := bodyChainRoot(chain)
	leaves := map[string]learn.Leaf{}
	learn.JSONLeaves("body", doc, leaves)
	for p, l := range leaves {
		l.Path = root + strings.TrimPrefix(p, "body")
		out[l.Path] = l
	}
}

// canonicalBase64 is text decoded as standard padded base64, ok only where
// text is that encoding's one text for what it decodes to.
func canonicalBase64(text []byte) ([]byte, bool) {
	if bytes.ContainsAny(text, "\r\n") {
		return nil, false
	}
	enc := base64.StdEncoding.Strict()
	out := make([]byte, enc.DecodedLen(len(text)))
	n, err := enc.Decode(out, text)
	if err != nil {
		return nil, false
	}
	return out[:n], true
}

// canonicalPercent is text with its percent escapes decoded, ok only where
// text is the text encodeURIComponent writes for what it decodes to.
func canonicalPercent(text []byte) ([]byte, bool) {
	out := make([]byte, 0, len(text))
	for i := 0; i < len(text); i++ {
		c := text[i]
		if c != '%' {
			if !uriComponentKept(c) {
				return nil, false
			}
			out = append(out, c)
			continue
		}
		if i+2 >= len(text) {
			return nil, false
		}
		hi, hok := upperHex(text[i+1])
		lo, lok := upperHex(text[i+2])
		if !hok || !lok || uriComponentKept(hi<<4|lo) {
			return nil, false
		}
		out = append(out, hi<<4|lo)
		i += 2
	}
	return out, true
}

// uriComponentKept reports whether encodeURIComponent writes c as it is.
func uriComponentKept(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte("-_.!~*'()", c) >= 0
}

// upperHex is the value of c, an upper-case hex digit.
func upperHex(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	}
	return 0, false
}
