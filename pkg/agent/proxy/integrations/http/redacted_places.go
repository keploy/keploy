package http

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"hash/maphash"
	"io"
	"maps"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
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
// redacted; none when m is nil or no HTTP call (placesFor).
func placesOf(m *models.Mock) redactedPlaces {
	if m == nil {
		return placesFor(nil, nil)
	}
	return placesFor(m.Spec.HTTPReq, m.Noise)
}

// placesFor reads the places of the recorded request r redacted by the
// patterns noise. They are a function of r's content and the patterns'
// texts, and are kept by those (placesKept): the agent stages each test's
// pool from copies of the recordings, so a recording is read once a run,
// whichever copy of it a test's pool holds. A recording with no patterns
// redacted nothing: it is read for its path and how its body is read
// (fieldsIn) only, with no digest. The places returned are read only:
// poolPlaces adds to a copy.
func placesFor(r *models.HTTPReq, noise []string) redactedPlaces {
	if r == nil || len(noise) == 0 {
		return redactedPlacesOf(r, noise)
	}
	key := placesKey(r, noise)
	if p, ok := placesKept.get(key); ok {
		return p
	}
	placesReads.Add(1)
	p := redactedPlacesOf(r, noise)
	placesKept.put(key, p, p.size(noise))
	return p
}

// placesReads counts the recorded requests read for their places
// (placesFor): one a recording with patterns, however many calls miss, and
// however many tests' pools hold a copy of it.
var placesReads atomic.Int64

// placesKey is the digest of what a recording's places are read from: r's
// URL, query parameters, headers and body, and the patterns' texts, each
// written with its length, a map's pairs in key order. It is two 64-bit
// hashes (hash/maphash) under two seeds drawn at start: no two recordings'
// contents meet by chance in 128 bits, and the seeds are not known outside
// the process.
func placesKey(r *models.HTTPReq, noise []string) placesDigest {
	var h [2]maphash.Hash
	h[0].SetSeed(placesSeeds[0])
	h[1].SetSeed(placesSeeds[1])
	write := func(s string) {
		var n [8]byte
		binary.BigEndian.PutUint64(n[:], uint64(len(s)))
		for i := range h {
			_, _ = h[i].Write(n[:])
			_, _ = h[i].WriteString(s)
		}
	}
	pairs := func(m map[string]string) {
		keys := make([]string, 0, 16)
		for k := range m {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		write(strconv.Itoa(len(keys)))
		for _, k := range keys {
			write(k)
			write(m[k])
		}
	}
	write(r.URL)
	pairs(r.URLParams)
	pairs(r.Header)
	write(r.Body)
	write(strconv.Itoa(len(noise)))
	for _, pat := range noise {
		write(pat)
	}
	return placesDigest{h[0].Sum64(), h[1].Sum64()}
}

// placesDigest is a recording's digest (placesKey).
type placesDigest [2]uint64

// placesSeeds are placesKey's seeds.
var placesSeeds = [2]maphash.Seed{maphash.MakeSeed(), maphash.MakeSeed()}

// maxKeptPlaces bounds the bytes the places kept hold (size): past it, kept
// places are dropped, any of them, until the new ones fit.
const maxKeptPlaces = 32 << 20

// placesKept are the places read, by their recording's digest (placesKey).
var placesKept = keptPlaces{byKey: map[placesDigest]keptEntry{}, max: maxKeptPlaces}

// keptPlaces are places by digest, holding at most max bytes (size).
type keptPlaces struct {
	mu    sync.RWMutex
	byKey map[placesDigest]keptEntry
	bytes int
	max   int
}

type keptEntry struct {
	places redactedPlaces
	size   int
}

func (k *keptPlaces) get(key placesDigest) (redactedPlaces, bool) {
	k.mu.RLock()
	defer k.mu.RUnlock()
	e, ok := k.byKey[key]
	return e.places, ok
}

// put keeps p, of size bytes, by key, dropping kept places until it fits;
// places larger than the bound are not kept.
func (k *keptPlaces) put(key placesDigest, p redactedPlaces, size int) {
	if size > k.max {
		return
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if old, ok := k.byKey[key]; ok {
		delete(k.byKey, key)
		k.bytes -= old.size
	}
	for other, e := range k.byKey {
		if k.bytes+size <= k.max {
			break
		}
		delete(k.byKey, other)
		k.bytes -= e.size
	}
	k.byKey[key] = keptEntry{places: p, size: size}
	k.bytes += size
}

// size is about the bytes p holds, read by the patterns noise: its names,
// its recorded path and the patterns' texts, each with what a map entry or
// a compiled pattern costs.
func (p redactedPlaces) size(noise []string) int {
	n := 512 + len(p.recordedPath) + 64*len(p.segments)
	for _, m := range []map[string]bool{p.headers, p.query, p.fields, p.keys} {
		for name := range m {
			n += 64 + len(name)
		}
	}
	for _, pat := range noise {
		n += 256 + 4*len(pat)
	}
	return n
}

// poolPlaces are the places closest's recorded request redacted, with those
// of every recording of pool added: a miss report names the call redacted
// by every place a recording it was compared with redacted, so a nearer
// recording that redacted nothing does not show the secret another hid,
// and the closest recording's places only add to them. A recording of the
// pool with no patterns redacted nothing and is not read. A path segment's
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
		if m == nil || len(m.Noise) == 0 {
			continue
		}
		q := placesOf(m)
		p.add(q, q.pathSegs == n)
	}
	return p
}

// heldPlaces are the places a miss of a call to livePath (escaped) and its
// logs are redacted by: poolPlaces of pool and closest, with the test set's
// recordings that answered a call and redacted something
// (integrations.AnsweredRedacting): a read of the pool no longer holds a
// per-test recording once it answered, while a call that repeats it carries
// the secret it redacted again. Every place (unreadPlaces) when the store
// did not keep them all, or does not tell them (no store, or a store or
// wrapper without integrations.AnsweredRedactingReader).
func heldPlaces(mockDb integrations.MockMemDb, closest *models.Mock, pool []*models.Mock, livePath string) redactedPlaces {
	answered, complete := integrations.AnsweredRedacting(mockDb, models.HTTP)
	if !complete {
		return unreadPlaces()
	}
	if len(answered) > 0 {
		pool = append(pool[:len(pool):len(pool)], answered...)
	}
	return poolPlaces(closest, pool, livePath)
}

// storePlaces are heldPlaces of closest and the pool mockDb holds now, its
// per-test recordings and its session's as the strict pass reads them
// (storeReads), what a test kept for its repeats and is over with included
// (a place it redacted is redacted still), for a log line of a pass that
// read no pool; every place (unreadPlaces) when the store cannot be read.
func storePlaces(mockDb integrations.MockMemDb, closest *models.Mock, livePath string) redactedPlaces {
	_, _, combined, err := storeReads(mockDb)
	if err != nil {
		return unreadPlaces()
	}
	return heldPlaces(mockDb, closest, FilterHTTPMocks(combined), livePath)
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
	p.headers, p.query, p.segments = own(p.headers), own(p.query), own(p.segments)
	p.fields, p.keys = own(p.fields), own(p.keys)
	return p
}

// own is a copy of m, a map even when m is nil.
func own[K comparable](m map[K]bool) map[K]bool {
	c := make(map[K]bool, len(m))
	maps.Copy(c, m)
	return c
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
	p := redactedPlaces{values: util.NewNoiseChecker(noise), pathSegs: -1}
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
	p.headers, p.query, p.segments = map[string]bool{}, map[string]bool{}, map[int]bool{}
	p.fields, p.keys = map[string]bool{}, map[string]bool{}
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
