package http

import (
	"bytes"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net/url"
	"strings"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/util"
	"go.keploy.io/server/v3/pkg/models"
)

// redactedPlaces are the places of a recorded request whose value the
// obfuscator redacted: a header, a query parameter, a URL path segment, the
// whole body, or a body field (a JSON field, a form key, a multipart field)
// whose recorded value, or form segment ("key=value"), matches one of the
// mock's patterns (Mock.Noise).
//
// The live call's value at such a place is the secret the recording hid, and
// it matches none of those patterns: they are anchored on the placeholder the
// obfuscator wrote, and a form key's on the recorded segment. So a report
// redacts the live side of these places by the place, never by the value:
// wherever a miss report carries the live call (its field diffs, the Diff line
// made of them, the summary's path and the rendered request), a redacted
// place shows valueRedacted on both sides.
type redactedPlaces struct {
	// values is the mock's patterns, which redact a value that matches
	// one of them wherever it is.
	values *util.NoiseChecker
	// headers are lowercased header names.
	headers map[string]bool
	query   map[string]bool
	// segments are URL path segments by position, as strictKnown.pathDiffs
	// counts them (the path's leading "/" dropped).
	segments map[int]bool
	// body is a recorded body redacted whole.
	body bool
	// fields are body fields by the name a field diff gives them under
	// "body.": a JSON field's path with each array position written "[]"
	// (matcher.JSONFieldDiffs, strictKnown.jsonDiffs), a form key or a
	// multipart field's name, decoded.
	fields map[string]bool
}

// redactedPlacesOf reads the places of r the obfuscator redacted, by the
// mock's patterns noise.
func redactedPlacesOf(r *models.HTTPReq, noise []string) redactedPlaces {
	p := redactedPlaces{values: util.NewNoiseChecker(noise), headers: map[string]bool{}, query: map[string]bool{}, segments: map[int]bool{}, fields: map[string]bool{}}
	if r == nil || p.values == nil {
		return p
	}
	for k, v := range r.Header {
		if p.noisy(v) {
			p.headers[strings.ToLower(k)] = true
		}
	}
	// The matcher reads the recorded query from URLParams, else from the
	// URL (mockQueryString): a place either holds is redacted.
	for k, v := range r.URLParams {
		if p.noisy(v) {
			p.query[k] = true
		}
	}
	if u, err := url.Parse(r.URL); err == nil {
		for k, vs := range u.Query() {
			for _, v := range vs {
				if p.noisy(v) {
					p.query[k] = true
				}
			}
		}
	}
	for i, seg := range pathSegments(mockURLPath(r.URL)) {
		if p.noisy(seg) {
			p.segments[i] = true
		}
	}
	if p.noisy(r.Body) {
		p.body = true
		return p
	}
	ct := headerValue(r.Header, "Content-Type")
	switch mediaTypeKey(ct) {
	case "application/x-www-form-urlencoded":
		for _, seg := range strings.Split(r.Body, "&") {
			raw, rawValue, _ := strings.Cut(seg, "=")
			key, err := url.QueryUnescape(raw)
			if err != nil {
				continue
			}
			value, err := url.QueryUnescape(rawValue)
			if err != nil {
				value = rawValue
			}
			if p.noisy(seg) || p.noisy(value) {
				p.fields[key] = true
			}
		}
	case "multipart/form-data":
		parts, _ := multipartParts(ct, []byte(r.Body))
		for _, part := range parts {
			if p.noisy(string(part.data)) {
				p.fields[part.name] = true
			}
		}
	default:
		var v interface{}
		if json.Unmarshal([]byte(r.Body), &v) == nil {
			p.collectJSON(v, "")
		}
		// A redacted string at the root, or in an array at the root, has
		// no name of its own: the body is redacted whole.
		p.body = p.fields[""]
	}
	return p
}

// noisy reports whether v matches one of the mock's patterns.
func (p redactedPlaces) noisy(v string) bool {
	return v != "" && p.values != nil && p.values.IsNoisy(v)
}

// collectJSON adds each string of v the obfuscator redacted, by its path.
func (p redactedPlaces) collectJSON(v interface{}, path string) {
	switch t := v.(type) {
	case map[string]interface{}:
		for k, child := range t {
			cp := k
			if path != "" {
				cp = path + "." + k
			}
			p.collectJSON(child, cp)
		}
	case []interface{}:
		cp := path
		if cp != "" {
			cp += "[]"
		}
		for _, child := range t {
			p.collectJSON(child, cp)
		}
	case string:
		if p.noisy(t) {
			p.fields[path] = true
		}
	}
}

// field reports whether a body field named name (a field diff's name under
// "body.") is a redacted field or inside one, or the body was redacted whole.
func (p redactedPlaces) field(name string) bool {
	if p.body {
		return true
	}
	for f := range p.fields {
		if name == f || strings.HasPrefix(name, f+".") || strings.HasPrefix(name, f+"[") {
			return true
		}
	}
	return false
}

// covers reports whether a field diff's path names a redacted place. A
// diff of the whole body is covered when any part of the body was redacted:
// its values would carry that part.
func (p redactedPlaces) covers(path string) bool {
	switch {
	case strings.HasPrefix(path, "header."):
		return p.headers[strings.ToLower(strings.TrimPrefix(path, "header."))]
	case strings.HasPrefix(path, "query."):
		return p.query[strings.TrimPrefix(path, "query.")]
	case path == "body":
		return p.body || len(p.fields) > 0
	case strings.HasPrefix(path, "body."):
		return p.field(strings.TrimPrefix(path, "body."))
	}
	return false
}

// path is a URL path with each segment the recording redacted written
// valueRedacted.
func (p redactedPlaces) path(path string) string {
	if len(p.segments) == 0 {
		return path
	}
	lead := strings.HasPrefix(path, "/")
	segs := pathSegments(path)
	for i := range segs {
		if p.segments[i] {
			segs[i] = valueRedacted
		}
	}
	out := strings.Join(segs, "/")
	if lead {
		out = "/" + out
	}
	return out
}

// redactDiff redacts, in place, both values of a field diff at a place the
// recording redacted; the "path" diff has its redacted segments written
// valueRedacted, so the segments that differ still show.
func (p redactedPlaces) redactDiff(d *models.MockFieldDiff) {
	if d.Path == "path" {
		d.Expected, d.Actual = p.path(d.Expected), p.path(d.Actual)
		return
	}
	if !p.covers(d.Path) {
		return
	}
	if d.Expected != "" {
		d.Expected = valueRedacted
	}
	if d.Actual != "" {
		d.Actual = valueRedacted
	}
}

// redactMultipart re-renders a multipart body read by contentType's
// boundary with each field the recording redacted written valueRedacted.
// It reports false when the body does not read as one.
func (p redactedPlaces) redactMultipart(body, contentType string) (string, bool) {
	_, params, err := mime.ParseMediaType(contentType)
	if err != nil || params["boundary"] == "" {
		return "", false
	}
	mr := multipart.NewReader(strings.NewReader(body), params["boundary"])
	var b bytes.Buffer
	w := multipart.NewWriter(&b)
	if err := w.SetBoundary(params["boundary"]); err != nil {
		return "", false
	}
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", false
		}
		data, err := io.ReadAll(part)
		if err != nil {
			return "", false
		}
		if p.field(part.FormName()) || isSensitiveHeader(part.FormName()) || p.noisy(string(data)) || looksLikeSecretValue(string(data)) {
			data = []byte(valueRedacted)
		}
		pw, err := w.CreatePart(part.Header)
		if err != nil {
			return "", false
		}
		if _, err := pw.Write(data); err != nil {
			return "", false
		}
	}
	if err := w.Close(); err != nil {
		return "", false
	}
	return b.String(), true
}

// summary is a miss's summary ("METHOD path") with the path's redacted
// segments written valueRedacted.
func (p redactedPlaces) summary(actualKey, path string) string {
	if len(p.segments) == 0 || path == "" {
		return actualKey
	}
	return strings.Replace(actualKey, " "+path, " "+p.path(path), 1)
}

// jsonChildPath is the path of a JSON object's member key under path, in
// the dialect redactedPlaces.fields holds.
func jsonChildPath(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}

// jsonElemPath is the path of a JSON array's elements under path.
func jsonElemPath(path string) string {
	if path == "" {
		return path
	}
	return path + "[]"
}
