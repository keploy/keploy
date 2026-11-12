package models

import (
	"net/url"
	"strings"
)

// ParseRequestURL parses a recorded HTTP request's URL (HTTPReq.URL). A
// path-only recording is read as a request line is (url.ParseRequestURI, as
// http.ReadRequest reads the call's): "//fees/state" is a path, not the host
// "fees" with the path "/state". Any other is read with url.Parse. Every
// reader of a recorded request's URL reads it so, or a call whose path begins
// with "//" (a base URL ending in "/" joined with a path beginning with one)
// is read as two different requests at record and at replay.
//
// A path-only recording keeps a "#" and what follows it in its path or its
// query, as a request line does (a recorder writes a "#" in a path as "%23",
// and a client never sends a fragment).
func ParseRequestURL(raw string) (*url.URL, error) {
	if strings.HasPrefix(raw, "/") {
		return url.ParseRequestURI(raw)
	}
	return url.Parse(raw)
}
