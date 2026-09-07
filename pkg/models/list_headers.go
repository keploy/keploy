package models

import "strings"

// unorderedListHeaders lists HTTP headers (lowercased) whose value is a
// comma-separated list (RFC 9110 §5.6.1 "#rule") in which the ORDER of the
// elements carries no meaning. Two such headers that hold the same elements in
// a different order are the same header, so comparing them positionally
// reports a difference where the protocol says there is none.
//
// This is the shape that made keploy/keploy#4349 fail: a server that builds
// `Allow` from an unordered collection — Django/DRF, Flask, Spring and Go's own
// http.ServeMux all do — emits "POST, OPTIONS" on one run and "OPTIONS, POST"
// on the next, and the replayed test failed on a header the application never
// actually changed.
//
// Membership is deliberately narrow. A header belongs here only when its
// grammar makes order meaningless, NOT merely when it happens to be a list:
//
//   - Via, Content-Encoding and Transfer-Encoding are ordered lists — the
//     sequence is the proxy chain, or the order the codings were applied — so
//     reordering them is a real difference and they stay out.
//   - Accept, Accept-Encoding and Accept-Language rank equal-q values by
//     position, so order is significant there too.
//   - Set-Cookie, WWW-Authenticate and Link may carry commas inside a single
//     element; they are not safely splittable and are excluded regardless of
//     whether order matters.
var unorderedListHeaders = map[string]struct{}{
	"allow":                          {}, // RFC 9110 §10.2.1 — set of methods
	"vary":                           {}, // RFC 9110 §12.5.5 — set of field names
	"trailer":                        {}, // RFC 9110 §6.6.2 — set of field names
	"connection":                     {}, // RFC 9110 §7.6.1 — set of connection options
	"accept-ranges":                  {}, // RFC 9110 §14.3 — set of range units
	"cache-control":                  {}, // RFC 9111 §5.2 — set of directives
	"access-control-allow-methods":   {}, // Fetch — set of methods
	"access-control-allow-headers":   {}, // Fetch — set of field names
	"access-control-expose-headers":  {}, // Fetch — set of field names
	"access-control-request-headers": {}, // Fetch — set of field names
}

// IsUnorderedListHeader reports whether name is EXACTLY one of the headers
// whose comma-separated elements may be reordered without changing meaning,
// compared case-insensitively.
//
// Exactness matters for the same reason it does in IsVolatileResponseHeader:
// matcher.SubstringKeyMatch resolves noise keys with strings.Contains, so
// routing this through a noise map would also catch unrelated headers that
// merely contain one of these names.
func IsUnorderedListHeader(name string) bool {
	_, ok := unorderedListHeaders[strings.ToLower(name)]
	return ok
}
