package variables

import (
	"net/http"
	"net/url"
	"strings"

	"go.keploy.io/server/v3/pkg/models"
)

// InRequest reports, for r, a recorded HTTP request, whether r holds one
// value at a place: the holds that ReadPlace reads a place of r's mock's
// variables block with, in its replay and in its learn step alike. A URL
// path segment that unescapes; a query name with one value (r's URLParams,
// where it has any, before its URL's query, as the matcher reads them); a
// header with one value, its name in any case; a body path at one JSON
// scalar. No request (nil), or one whose URL does not parse, holds none.
func InRequest(r *models.HTTPReq) func(Place) bool {
	if r == nil {
		return func(Place) bool { return false }
	}
	var (
		read     bool
		ok       bool
		segments []string
		query    url.Values
		header   http.Header
	)
	view := func() bool {
		if read {
			return ok
		}
		read = true
		u, err := url.Parse(r.URL)
		if err != nil {
			return false
		}
		segments = strings.Split(strings.TrimPrefix(u.EscapedPath(), "/"), "/")
		query = url.Values{}
		if len(r.URLParams) > 0 {
			for k, v := range r.URLParams {
				query.Set(k, v)
			}
		} else {
			query = u.Query()
		}
		header = http.Header{}
		for k, v := range r.Header {
			header.Set(k, v)
		}
		ok = true
		return true
	}
	return func(p Place) bool {
		if !view() {
			return false
		}
		switch p.Part {
		case PartBody:
			_, held := JSONLeafAt(r.Body, p.Path)
			return held
		case PartPath:
			if p.Index < 0 || p.Index >= len(segments) {
				return false
			}
			_, err := url.PathUnescape(segments[p.Index])
			return err == nil
		case PartQuery:
			return len(query[p.Name]) == 1
		case PartHeader:
			return len(header.Values(p.Name)) == 1
		}
		return false
	}
}
