package http

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"weak"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/util"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/templates"
	"go.keploy.io/server/v3/pkg/variables"
	"go.keploy.io/server/v3/utils"
)

// redactedPlaces are the places of a recorded request whose value the
// obfuscator redacted: a header, a query parameter, a URL path segment, the
// whole body, or a body field (a JSON field, a form key, a multipart field)
// whose recorded value, or form segment ("key=value"), matches one of the
// mock's patterns (Mock.Noise): a JSON string or number field, and a JSON
// string, or a multipart field's data, that holds a value a pattern names
// anywhere (holdsPattern).
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
	// segments are URL path segments by position, counted on the escaped
	// path (the path's leading "/" dropped), as a test variable's place
	// (url.path[i]) counts them: a %2F stays inside its segment, on the
	// recording's side and on the call's.
	segments map[int]bool
	// recordedPath is the recorded URL's escaped path, and pathSegs the
	// count of its segments (pathSegments); -1 when it does not parse.
	recordedPath string
	pathSegs     int
	// urlUnread: the recorded URL does not parse, so its places cannot be
	// found, and the mock redacted something (it holds a pattern): the
	// call's path and query values are redacted whole.
	urlUnread bool
	// body is a recorded body redacted whole.
	body bool
	// fieldsIn is how the recorded body was read for its fields: as JSON,
	// a form or a multipart form.
	fieldsIn bodyForm
	// fields are body fields by the name a field diff gives them under
	// "body.": a JSON field's path with each array position written "[]",
	// a root array's too ("[].id", as matcher.JSONFieldDiffs and the strict
	// comparison name it), a form key or a multipart field's name, decoded.
	fields map[string]bool
	// keys are the last keys of fields (fieldKeys): the obfuscator redacts
	// a body field by its key's name, so a live field under one of them is
	// redacted wherever it sits, though the recording has it elsewhere.
	keys map[string]bool
}

// placesOf reads the places of m's recorded request the obfuscator
// redacted; none when m is nil or no HTTP call. A recording's places are
// read once (placesMemo): they are a function of its recorded request and
// its patterns, which a recording of the pool never changes (a served view
// is a copy of it). The places returned are read only: poolPlaces adds to a
// copy.
func placesOf(m *models.Mock) redactedPlaces {
	if m == nil {
		return redactedPlacesOf(nil, nil)
	}
	key := weak.Make(m)
	if e, ok := placesMemo.Load(key); ok {
		if e := e.(*memoPlaces); e.req == m.Spec.HTTPReq && sameNoise(e.noise, m.Noise) {
			return e.places
		}
	}
	placesReads.Add(1)
	p := redactedPlacesOf(m.Spec.HTTPReq, m.Noise)
	if _, replaced := placesMemo.Swap(key, &memoPlaces{req: m.Spec.HTTPReq, noise: m.Noise, places: p}); !replaced {
		// The entry goes with its recording.
		runtime.AddCleanup(m, func(k weak.Pointer[models.Mock]) { placesMemo.Delete(k) }, key)
	}
	return p
}

// placesMemo holds each recording's places once read, by the recording: a
// weak pointer, so it keeps no recording of a pool that is gone. A miss
// report, and a debug line, over a pool of n recordings unions n sets read
// once, not n recorded requests read again for each call.
var placesMemo sync.Map // weak.Pointer[models.Mock] -> *memoPlaces

// placesReads counts the recorded requests read for their places
// (placesOf): one a recording, however many calls miss.
var placesReads atomic.Int64

// memoPlaces are a recording's places, with the recorded request and the
// patterns they were read from: a recording given another request or other
// patterns is read again.
type memoPlaces struct {
	req    *models.HTTPReq
	noise  []string
	places redactedPlaces
}

// sameNoise reports whether a and b are the same patterns: one slice.
func sameNoise(a, b []string) bool {
	return len(a) == len(b) && (len(a) == 0 || &a[0] == &b[0])
}

// poolPlaces are the places closest's recorded request redacted, with those
// of every recording of pool added: a miss report names the call redacted
// by every place a recording it was compared with redacted, so a nearer
// recording that redacted nothing does not show the secret another hid,
// and the closest recording's places only add to them. A path segment's
// place is taken from a recording of the pool whose path has as many
// segments as the call's, livePath (escaped), only: a path of another
// shape says nothing of the call's segment i. The patterns, the recorded
// path and how a body is read for its fields (fieldsIn) stay closest's,
// whatever its patterns: a pattern names the placeholder a recording
// wrote, which no call's secret is.
func poolPlaces(closest *models.Mock, pool []*models.Mock, livePath string) redactedPlaces {
	p := placesOf(closest).clone()
	n := len(pathSegments(livePath))
	for _, m := range pool {
		q := placesOf(m)
		p.add(q, q.pathSegs == n)
	}
	return p
}

// unreadPlaces are the places of a pool that cannot be read: any place may
// be one a recording redacted, so the call's path, its query values and its
// body are redacted whole.
func unreadPlaces() redactedPlaces {
	p := placesOf(nil)
	p.urlUnread, p.body = true, true
	return p
}

// shapeSummary is a miss's summary with no recording at hand, "METHOD
// shape": the call's path named by its shape (utils.URLPathShape), as a log
// made where no recording is at hand names it.
func shapeSummary(request *http.Request) string {
	return request.Method + " " + utils.URLPathShape(request.URL)
}

// clone is p with maps of its own, which add may add to.
func (p redactedPlaces) clone() redactedPlaces {
	p.headers, p.query, p.segments = maps.Clone(p.headers), maps.Clone(p.query), maps.Clone(p.segments)
	p.fields, p.keys = maps.Clone(p.fields), maps.Clone(p.keys)
	return p
}

// add adds q's places to p's, its path segments' when segments is true.
func (p *redactedPlaces) add(q redactedPlaces, segments bool) {
	maps.Copy(p.headers, q.headers)
	maps.Copy(p.query, q.query)
	if segments {
		maps.Copy(p.segments, q.segments)
	}
	maps.Copy(p.fields, q.fields)
	maps.Copy(p.keys, q.keys)
	p.body = p.body || q.body
	p.urlUnread = p.urlUnread || q.urlUnread
}

// redactedPlacesOf reads the places of r the obfuscator redacted, by the
// mock's patterns noise.
func redactedPlacesOf(r *models.HTTPReq, noise []string) redactedPlaces {
	p := redactedPlaces{values: util.NewNoiseChecker(noise), pathSegs: -1, headers: map[string]bool{}, query: map[string]bool{}, segments: map[int]bool{}, fields: map[string]bool{}, keys: map[string]bool{}}
	if r == nil {
		return p
	}
	// The recorded path, and how the recorded body is read for fields, are
	// read whatever the patterns: a report redacted by another recording's
	// places names this recording's path and reads the call's body as this
	// recording's.
	u, err := parseRecordedURL(r.URL)
	if err == nil {
		p.recordedPath = u.EscapedPath()
		p.pathSegs = len(pathSegments(p.recordedPath))
	}
	ct := headerValue(r.Header, "Content-Type")
	p.fieldsIn = fieldsForm(ct)
	if p.values == nil {
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
	p.urlUnread = err != nil
	if err == nil {
		for k, vs := range u.Query() {
			for _, v := range vs {
				if p.noisy(v) {
					p.query[k] = true
				}
			}
		}
		for i, seg := range pathSegments(p.recordedPath) {
			if p.noisy(seg) || p.noisy(unescapeSegment(seg)) {
				p.segments[i] = true
			}
		}
	}
	within := anchorFreeOf(noise)
	if p.noisy(r.Body) {
		p.body = true
		return p
	}
	switch p.fieldsIn {
	case bodyFormURLEncoded:
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
				p.addField(key)
			}
		}
	case bodyMultipart:
		parts, _ := multipartParts(ct, []byte(r.Body))
		for _, part := range parts {
			// A part's data is not read for fields: one that holds a
			// redacted value anywhere is redacted whole.
			if p.noisy(string(part.data)) || holds(within, string(part.data)) {
				p.addField(part.name)
			}
		}
	default:
		v, err := decodeJSONNumbers(r.Body)
		if err != nil {
			// Not read for fields (XML, SOAP, text): a body that holds a
			// value one of the mock's patterns names, at a place that
			// cannot be found, is redacted whole.
			p.body = holds(within, r.Body)
			return p
		}
		p.collectJSON(v, "", within)
		// A redacted string at the root has no name of its own: the body
		// is redacted whole (a redacted number at the root is the whole
		// body, which the patterns name above).
		p.body = p.fields[""]
	}
	return p
}

// fieldsForm is how a recorded body of content type ct is read for its
// fields: a form, a multipart form, else JSON.
func fieldsForm(ct string) bodyForm {
	switch mediaTypeKey(ct) {
	case "application/x-www-form-urlencoded":
		return bodyFormURLEncoded
	case "multipart/form-data":
		return bodyMultipart
	}
	return bodyJSON
}

// addField adds the body field name, and its last key (keys).
func (p redactedPlaces) addField(name string) {
	p.fields[name] = true
	p.keys[lastKey(name)] = true
}

// lastKey is the last key a recorded body field's name holds: "assertion"
// of "items[].assertion", "tags" of "tags[]", "password" of the form key
// "user[password]"; "" of a root array's elements ("[]"), which no key of
// fieldKeys is.
func lastKey(name string) string {
	name = strings.TrimRight(name, "[]")
	return name[strings.LastIndexAny(name, ".[]")+1:]
}

// fieldKeys are the keys a body field's name holds, in order, split at "."
// and at the brackets of an array position or a form key ("items[0].id"
// and "items[].id" hold items, 0 and id; "user[password]" user and
// password), so a key reads the same in each dialect a field is named in.
func fieldKeys(name string) []string {
	return strings.FieldsFunc(name, func(r rune) bool { return r == '.' || r == '[' || r == ']' })
}

// holdsPattern reports whether text holds a value one of the patterns
// names, read anywhere in it: each pattern with its anchors (a leading "^",
// a trailing unquoted "$") dropped, as the obfuscator anchors the value it
// wrote on the value alone.
func holdsPattern(patterns []string, text string) bool {
	return holds(anchorFreeOf(patterns), text)
}

// holds reports whether text holds a match of one of the anchor-free
// patterns within (anchorFreeOf).
func holds(within []*regexp.Regexp, text string) bool {
	for _, re := range within {
		if re.MatchString(text) {
			return true
		}
	}
	return false
}

// anchorFreeOf is each of patterns with its anchors dropped, compiled
// (anchorFree); a pattern that holds nothing else, or whose anchor-free
// form does not compile, is left out.
func anchorFreeOf(patterns []string) []*regexp.Regexp {
	var out []*regexp.Regexp
	for _, pat := range patterns {
		if re := anchorFree(pat); re != nil {
			out = append(out, re)
		}
	}
	return out
}

// anchorFree is pat with its anchors dropped, compiled once a pattern, nil
// when nothing else is left or it does not compile. The cache is bounded
// as util's pattern cache is: a full one is emptied.
func anchorFree(pat string) *regexp.Regexp {
	anchorFreeMu.RLock()
	re, ok := anchorFreeCache[pat]
	anchorFreeMu.RUnlock()
	if ok {
		return re
	}
	inner := strings.TrimPrefix(pat, "^")
	if strings.HasSuffix(inner, "$") && !quoted(inner, len(inner)-1) {
		inner = inner[:len(inner)-1]
	}
	re = nil
	if inner != "" {
		anchorFreeCompiles.Add(1)
		if c, err := regexp.Compile(inner); err == nil {
			re = c
		}
	}
	anchorFreeMu.Lock()
	if len(anchorFreeCache) >= maxAnchorFree {
		anchorFreeCache = map[string]*regexp.Regexp{}
	}
	anchorFreeCache[pat] = re
	anchorFreeMu.Unlock()
	return re
}

const maxAnchorFree = 1024

var (
	anchorFreeMu    sync.RWMutex
	anchorFreeCache = map[string]*regexp.Regexp{}
	// anchorFreeCompiles counts the patterns compiled (anchorFree): one a
	// pattern, however many strings are read with it.
	anchorFreeCompiles atomic.Int64
)

// quoted reports whether the byte at i of a pattern is quoted: an odd count
// of backslashes is before it.
func quoted(pat string, i int) bool {
	n := 0
	for j := i - 1; j >= 0 && pat[j] == '\\'; j-- {
		n++
	}
	return n%2 == 1
}

// noisy reports whether v matches one of the mock's patterns.
func (p redactedPlaces) noisy(v string) bool {
	return v != "" && p.values != nil && p.values.IsNoisy(v)
}

// decodeJSONNumbers is body decoded as one JSON value, each number read as
// the literal the body writes (json.Number).
func decodeJSONNumbers(body string) (interface{}, error) {
	d := json.NewDecoder(strings.NewReader(body))
	d.UseNumber()
	var v interface{}
	if err := d.Decode(&v); err != nil {
		return nil, err
	}
	// One value, and nothing after it (json.Unmarshal's rule).
	if _, err := d.Token(); err != io.EOF {
		return nil, errors.New("invalid JSON: data after the value")
	}
	return v, nil
}

// collectJSON adds each string and number of v the obfuscator redacted, by
// its path. The obfuscator redacts a number under a key it reads as a
// secret's (a pin, an otp) or one a pattern names, writes another number in
// its place and anchors a pattern on that number's literal. It redacts a
// string that holds a URL in the URL's structure (a query value, a path
// segment), anchoring a pattern on that part alone, so a string that holds
// a value a pattern names anywhere (holdsPattern) is redacted whole.
func (p redactedPlaces) collectJSON(v interface{}, path string, within []*regexp.Regexp) {
	switch t := v.(type) {
	case map[string]interface{}:
		for k, child := range t {
			cp := k
			if path != "" {
				cp = path + "." + k
			}
			p.collectJSON(child, cp, within)
		}
	case []interface{}:
		for _, child := range t {
			p.collectJSON(child, jsonElemPath(path), within)
		}
	case string:
		if p.noisy(t) || holds(within, t) {
			p.addField(path)
		}
	case json.Number:
		if p.noisy(t.String()) {
			p.addField(path)
		}
	}
}

// field reports whether a body field named name (a field diff's name under
// "body.") is a redacted field or inside one, is under a key the recording
// redacted a field by (keys) wherever it sits, or the body was redacted
// whole.
func (p redactedPlaces) field(name string) bool {
	if p.body {
		return true
	}
	for f := range p.fields {
		if name == f || strings.HasPrefix(name, f+".") || strings.HasPrefix(name, f+"[") {
			return true
		}
	}
	for _, k := range fieldKeys(name) {
		if p.keys[k] {
			return true
		}
	}
	return false
}

// queryKey reports whether the call's query parameter name is at a place the
// recording redacted, or the recorded URL could not be read for its places.
func (p redactedPlaces) queryKey(name string) bool {
	return p.urlUnread || p.query[name]
}

// segment reports whether the call's URL path segment i is at a place the
// recording redacted, or the recorded URL could not be read for its places.
func (p redactedPlaces) segment(i int) bool {
	return p.urlUnread || p.segments[i]
}

// covers reports whether a field diff's path names a redacted place. A
// test variable's place, or a word of one, reads in the variables grammar
// (variables.ParseAddress: header.X, url.query.name, url.path[i],
// body.items[0].id); a place a diff names in its own ("query.name", a
// "[]" for an array's elements) reads as the matcher writes it. A diff
// named "body" alone names the body whole: the strict comparison writes it
// with no values, with the two JSON types, or with the two values of a
// body that is one JSON scalar (a root string), which is covered when the
// recording redacted the body whole.
func (p redactedPlaces) covers(path string) bool {
	if a, err := variables.ParseAddress(path); err == nil && p.at(a.Place) {
		return true
	}
	switch {
	case path == "body":
		return p.body
	case strings.HasPrefix(path, "query."):
		return p.queryKey(strings.TrimPrefix(path, "query."))
	case strings.HasPrefix(path, "body."):
		return p.field(strings.TrimPrefix(path, "body."))
	}
	return false
}

// at reports whether a test variable's place is a redacted place, or inside
// one.
func (p redactedPlaces) at(place variables.Place) bool {
	switch place.Part {
	case variables.PartHeader:
		return p.headers[strings.ToLower(place.Name)]
	case variables.PartQuery:
		return p.queryKey(place.Name)
	case variables.PartPath:
		return p.segment(place.Index)
	case variables.PartBody:
		return p.field(bodyFieldName(place.Path))
	}
	return false
}

// bodyFieldName is a JSON body place's path as redactedPlaces.fields names
// it: each array position written "[]".
func bodyFieldName(path []templates.PathSeg) string {
	name := ""
	for _, seg := range path {
		if seg.IsIndex {
			name = jsonElemPath(name)
		} else {
			name = jsonChildPath(name, seg.Key)
		}
	}
	return name
}

// escapedPath is the escaped URL path escaped with each segment the
// recording redacted written valueRedacted, counted as segments counts them.
func (p redactedPlaces) escapedPath(escaped string) string {
	return p.redactPath(escaped, false)
}

// decodedPath is the escaped URL path escaped decoded, as URL.Path names
// it, with each segment the recording redacted written valueRedacted: the
// segments are counted escaped, then the others decoded one by one, so a
// %2F in a segment neither shifts the segments after it nor splits a
// redacted one.
func (p redactedPlaces) decodedPath(escaped string) string {
	return p.redactPath(escaped, true)
}

func (p redactedPlaces) redactPath(escaped string, decode bool) string {
	if p.urlUnread {
		return "/" + valueRedacted
	}
	segs := pathSegments(escaped)
	for i, seg := range segs {
		switch {
		case p.segments[i] && seg != "":
			// An empty segment holds no value to redact.
			segs[i] = valueRedacted
		case decode:
			segs[i] = unescapeSegment(seg)
		}
	}
	out := strings.Join(segs, "/")
	if strings.HasPrefix(escaped, "/") {
		out = "/" + out
	}
	return out
}

// unescapeSegment is an escaped path segment decoded; seg as it is when it
// does not decode.
func unescapeSegment(seg string) string {
	if d, err := url.PathUnescape(seg); err == nil {
		return d
	}
	return seg
}

// pathValue is a "path" diff's value v, which names a path decoded (URL.Path),
// with the segments the recording redacted written valueRedacted, read by
// the escaped path v was decoded from (decodedPath). A v that is not that
// path decoded cannot have its segments counted, so it is redacted whole
// when the recording redacted a segment.
func (p redactedPlaces) pathValue(v, escaped string) string {
	if v == "" || len(p.segments) == 0 && !p.urlUnread {
		return v
	}
	if decoded, err := url.PathUnescape(escaped); p.urlUnread || err == nil && decoded == v {
		return p.decodedPath(escaped)
	}
	return valueRedacted
}

// pathPrefix is the call's path, escaped, decoded (decodedPath) up to the
// first segment the recording redacted: a session call's pathPrefix that
// ends in "/" matches every path under it (models.SessionCallRule), so a
// hint that names it is one a user can paste, and names no secret.
func (p redactedPlaces) pathPrefix(escaped string) string {
	path := p.decodedPath(escaped)
	if i := strings.Index(path, valueRedacted); i >= 0 {
		return path[:i]
	}
	return path
}

// redactDiff redacts, in place, both values of a field diff at a place the
// recording redacted. The "path" diff names the recorded path and the
// call's, livePath escaped, decoded: each has its redacted segments written
// valueRedacted (pathValue), so the segments that differ still show.
func (p redactedPlaces) redactDiff(d *models.MockFieldDiff, livePath string) {
	if d.Path == "path" {
		d.Expected, d.Actual = p.pathValue(d.Expected, p.recordedPath), p.pathValue(d.Actual, livePath)
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
	if err != nil {
		return "", false
	}
	// A body cut before its close delimiter does not read as one, though
	// the reader ends it as one that ended (io.EOF) when the cut is inside
	// a part's header.
	if !strings.Contains(body, "--"+params["boundary"]+"--") {
		return "", false
	}
	mr := multipart.NewReader(strings.NewReader(body), params["boundary"])
	var b bytes.Buffer
	w := multipart.NewWriter(&b)
	// SetBoundary refuses a boundary RFC 2046 does not allow: none, or one
	// longer than 70 characters.
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

// summary is a miss's summary, "METHOD path", the path named decoded
// (decodedPath) from escaped, the call's escaped path, with the segments
// the recording redacted written valueRedacted.
func (p redactedPlaces) summary(method, escaped string) string {
	return method + " " + p.decodedPath(escaped)
}

// escapedSummary is summary with the path named escaped (escapedPath).
func (p redactedPlaces) escapedSummary(method, escaped string) string {
	return method + " " + p.escapedPath(escaped)
}

// bodyForm is how a body is read for its fields.
type bodyForm int

const (
	bodyJSON bodyForm = iota + 1
	bodyFormURLEncoded
	bodyMultipart
)

// redactFields renders body, the call's, with each field the recording
// redacted written valueRedacted, when the recording redacted a field: the
// body is read as the recording's was (fieldsIn). It reports false when the
// body does not read that way: a field's place cannot be found in it, so it
// is to be redacted whole.
func (p redactedPlaces) redactFields(body, contentType string) (string, bool) {
	switch p.fieldsIn {
	case bodyJSON:
		var v interface{}
		if json.Unmarshal([]byte(body), &v) != nil {
			return "", false
		}
		redactJSONValue(v, p, "")
		out, err := json.MarshalIndent(v, "", "  ")
		return string(out), err == nil
	case bodyFormURLEncoded:
		if mediaTypeKey(contentType) != "application/x-www-form-urlencoded" {
			return "", false
		}
		// redactQuery redacts a body that does not parse whole itself.
		return redactQuery(body, p.values, p.field), true
	case bodyMultipart:
		if mediaTypeKey(contentType) != "multipart/form-data" {
			return "", false
		}
		return p.redactMultipart(body, contentType)
	}
	return "", false
}

// jsonChildPath is the path of a JSON object's member key under path, in
// the dialect redactedPlaces.fields holds.
func jsonChildPath(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}

// jsonElemPath is the path of a JSON array's elements under path ("[]" at
// the root).
func jsonElemPath(path string) string {
	return path + "[]"
}
