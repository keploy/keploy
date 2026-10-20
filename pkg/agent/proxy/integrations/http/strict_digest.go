package http

import (
	"encoding/binary"
	"encoding/json"
	"hash"
	"hash/fnv"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"go.keploy.io/server/v3/pkg/models"
)

// THE REQUEST DIGEST.
//
// A call record's full round would pair with a recording it drifted from is
// first looked for among the staged recordings the open view does not show
// (elsewhereRecording): it may be one of them, made where it was not
// recorded. Most calls of that round drift, and a set may stage thousands of
// recordings of one operation, so each lookup compares the call only with
// the recordings whose digest may be the call's
// (integrations.ElsewhereQuery): a recording's is computed once per staging,
// the call's once per lookup.
//
// The digest is a necessary condition of the strict comparison (strictSame),
// never a sufficient one: a recording strictSame takes for the call has the
// call's digest, part by part, and a recording of the call's digest is still
// compared whole. Its parts read what strictSame reads, as it reads it:
//
//   - the request line: the method; the path with keploy.yml's url noise
//     masked (pathDiffs); and the query's parameters, each with its values
//     masked the same way, in any order (queryDiffs), less keploy's own
//     parameters and those a presigned SigV4 URL's grammar reads;
//   - the headers: every one strictSame compares, by its name in any case,
//     its Content-Type by its media types and its User-Agent by its first
//     product's name (headerDiffs), less the Authorization and the payload
//     hash, which are read by their SigV4 grammar;
//   - the body: a JSON body as bodyDiffs compares one (every object member
//     by its name, in any order; every array element in its place; every
//     scalar by value, a number by its value: 1.50 is 1.5), with what
//     keploy.yml's request-body noise names left out, member or element,
//     whatever its value (bodyDiffs excuses no more); any other body byte
//     for byte.
//
// A part strictSame reads with noise of the recording's own has none: every
// part of a recording that learned a field (its learned noise may name any
// of them), the headers and the body of one with values the obfuscator
// redacted, and the body of one recorded as a form or a multipart form,
// which bodyDiffs compares field by field. A part either side has none of is
// not read: what it holds is compared whole.

// requestDigest is a request's digest, part by part (see above); a part's ok
// false when it has none.
type requestDigest struct {
	line, head, body       uint64
	lineOK, headOK, bodyOK bool
}

// rulesOut reports whether a recording of digest d cannot be a call of
// digest live: a part both have differs.
func (d requestDigest) rulesOut(live requestDigest) bool {
	return d.lineOK && live.lineOK && d.line != live.line ||
		d.headOK && live.headOK && d.head != live.head ||
		d.bodyOK && live.bodyOK && d.body != live.body
}

// liveRequestDigest is the live request's digest under keploy.yml's noise n.
func liveRequestDigest(input *req, n strictNoise) requestDigest {
	path := ""
	var q url.Values
	if input.url != nil {
		path, q = input.url.Path, input.url.Query()
	}
	d := requestDigest{line: lineDigest(input.method, path, q, n), lineOK: true}
	d.head, d.headOK = liveHeaderDigest(input.header, n)
	d.body, d.bodyOK = bodyDigest(input.body, n)
	return d
}

// recordedRequestDigest is m's recorded request's digest under keploy.yml's
// noise n: none for a recording with no request, and none of a part
// strictSame reads with noise of m's own.
func recordedRequestDigest(m *models.Mock, n strictNoise) requestDigest {
	r := m.Spec.HTTPReq
	if r == nil || len(m.Spec.ReqBodyNoise) > 0 {
		return requestDigest{}
	}
	path, q := recordedURL(r)
	d := requestDigest{line: lineDigest(string(r.Method), path, q, n), lineOK: true}
	if len(m.Noise) > 0 {
		return d
	}
	d.head, d.headOK = headerDigest(recordedHeaders(r.Header), n), true
	d.body, d.bodyOK = recordedBodyDigest(m, n)
	return d
}

// requestDigestOf names what a request digest reads besides the request:
// keploy.yml's noise n (integrations.ElsewhereQuery's DigestOf).
func requestDigestOf(n strictNoise) string {
	var b strings.Builder
	b.WriteString("http-request\x00")
	headers := make([]string, 0, len(n.header))
	for h := range n.header {
		headers = append(headers, h)
	}
	sort.Strings(headers)
	for _, h := range headers {
		b.WriteString(h)
		b.WriteByte(0)
	}
	b.WriteByte(1)
	for _, re := range n.url {
		b.WriteString(re.String())
		b.WriteByte(0)
	}
	b.WriteByte(1)
	b.WriteString(n.body.Signature())
	return b.String()
}

// lineDigest is a request line's digest: its method, its path with url noise
// masked (as pathDiffs reads it) and its query (as queryDiffs reads one).
func lineDigest(method, path string, q url.Values, n strictNoise) uint64 {
	d := digester{h: fnv.New64a(), noise: n}
	d.text(method)
	d.text(strings.TrimPrefix(maskURLNoise(path, n.url), "/"))
	names := make([]string, 0, len(q))
	for name := range q {
		if strings.HasPrefix(strings.ToLower(name), "keploy") || sigV4QueryGrammar[name] {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	d.text(strconv.Itoa(len(names)))
	for _, name := range names {
		vals := maskValues(q[name], n.url)
		sort.Strings(vals)
		d.text(name)
		d.text(strconv.Itoa(len(vals)))
		for _, v := range vals {
			d.text(v)
		}
	}
	return d.h.Sum64()
}

// sigV4QueryGrammar are the query parameters a presigned SigV4 URL's grammar
// reads (presignedDiffs) when both sides are presigned: a digest leaves them
// out, as either side may be.
var sigV4QueryGrammar = map[string]bool{amzCredential: true, amzDate: true, amzSignature: true, amzSecurityToken: true}

// recordedHeaders are a recording's headers by their lowercased names, the
// value of the first name in order for names that differ in case only, as
// headerDiffs reads them.
func recordedHeaders(rec map[string]string) map[string]string {
	out := make(map[string]string, len(rec))
	for _, name := range sortedStringKeys(rec) {
		ln := strings.ToLower(name)
		if _, dup := out[ln]; !dup {
			out[ln] = rec[name]
		}
	}
	return out
}

// liveHeaderDigest is a live request's headers' digest (headerDigest), each
// header's lines joined with "," as headerDiffs joins them; none when two of
// its names differ in case only, whose lines headerDiffs joins in no set
// order.
func liveHeaderDigest(live http.Header, n strictNoise) (uint64, bool) {
	hs := make(map[string]string, len(live))
	for name, vs := range live {
		ln := strings.ToLower(name)
		if _, dup := hs[ln]; dup {
			return 0, false
		}
		hs[ln] = strings.Join(vs, ",")
	}
	return headerDigest(hs, n), true
}

// headerDigest is the digest of headers (by lowercased name) as headerDiffs
// compares them: every header it compares by value, by name, a Content-Type
// by its media types and a User-Agent by its first product's name. Left out:
// what it does not compare (Host, Content-Length, keploy's own headers, the
// headers keploy.yml's and the static list's noise name), and the
// Authorization and the payload hash, which it reads by their SigV4 grammar.
func headerDigest(hs map[string]string, n strictNoise) uint64 {
	names := make([]string, 0, len(hs))
	for name := range hs {
		switch {
		case name == "host", name == "content-length", strings.HasPrefix(name, "keploy"), n.header[name],
			name == "authorization", name == "x-amz-content-sha256":
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	d := digester{h: fnv.New64a(), noise: n}
	d.text(strconv.Itoa(len(names)))
	for _, name := range names {
		v := hs[name]
		switch name {
		case "content-type":
			v = mediaTypeKey(v)
		case "user-agent":
			v = productName(v)
		}
		d.text(name)
		d.text(v)
	}
	return d.h.Sum64()
}

// bodyDigest is body's digest under keploy.yml's request-body noise (n.body);
// ok false when it has none: a number whose exponent keploy does not read.
func bodyDigest(body []byte, n strictNoise) (uint64, bool) {
	h := fnv.New64a()
	v, isJSON := decodeJSONDoc(body)
	if !isJSON {
		h.Write([]byte{'R'})
		h.Write(body)
		return h.Sum64(), true
	}
	h.Write([]byte{'J'})
	d := digester{h: h, noise: n}
	if !d.value(v, "") {
		return 0, false
	}
	return h.Sum64(), true
}

// recordedBodyDigest is m's recorded body's digest (bodyDigest); ok false for
// a recording that has none (see above).
func recordedBodyDigest(m *models.Mock, n strictNoise) (uint64, bool) {
	r := m.Spec.HTTPReq
	if r == nil || len(m.Spec.ReqBodyNoise) > 0 || len(m.Noise) > 0 {
		return 0, false
	}
	// bodyDiffs reads the recording's Content-Type by headerValue, any one
	// of names that differ in case only: none of them may be a form's.
	for name, v := range r.Header {
		if !strings.EqualFold(name, "Content-Type") {
			continue
		}
		switch mediaTypeKey(v) {
		case "application/x-www-form-urlencoded", "multipart/form-data":
			return 0, false
		}
	}
	return bodyDigest([]byte(r.Body), n)
}

// digester writes a decoded JSON value's canonical form to h.
type digester struct {
	h     hash.Hash64
	noise strictNoise
	buf   [binary.MaxVarintLen64]byte
}

// text writes s, its length first, so that no two sequences of texts write
// the same bytes.
func (d *digester) text(s string) {
	n := binary.PutUvarint(d.buf[:], uint64(len(s)))
	d.h.Write(d.buf[:n])
	d.h.Write([]byte(s))
}

// value writes v, found at risk (the request matcher's dialect, relative to
// the body: items[].id; "" at the root); false when a number in it cannot be
// read.
func (d *digester) value(v interface{}, risk string) bool {
	switch t := v.(type) {
	case map[string]interface{}:
		keys := make([]string, 0, len(t))
		for k := range t {
			child := k
			if risk != "" {
				child = risk + "." + k
			}
			if !d.noise.body.Names(child) {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		d.h.Write([]byte{'{'})
		d.text(strconv.Itoa(len(keys)))
		for _, k := range keys {
			child := k
			if risk != "" {
				child = risk + "." + k
			}
			d.text(k)
			if !d.value(t[k], child) {
				return false
			}
		}
		d.h.Write([]byte{'}'})
	case []interface{}:
		d.h.Write([]byte{'['})
		d.text(strconv.Itoa(len(t)))
		elem := risk + "[]"
		named := d.noise.body.Names(elem)
		for _, e := range t {
			if named {
				d.h.Write([]byte{'_'})
				continue
			}
			if !d.value(e, elem) {
				return false
			}
		}
		d.h.Write([]byte{']'})
	case string:
		d.h.Write([]byte{'s'})
		d.text(t)
	case json.Number:
		c, ok := canonicalNumber(t.String())
		if !ok {
			return false
		}
		d.h.Write([]byte{'n'})
		d.text(c)
	case bool:
		if t {
			d.h.Write([]byte{'t'})
		} else {
			d.h.Write([]byte{'f'})
		}
	case nil:
		d.h.Write([]byte{'z'})
	default:
		return false
	}
	return true
}

// canonicalNumber is a JSON number's value as one text: two numbers of one
// value (sameScalar: 1.50 and 1.5, 1e2 and 100, -0 and 0) have one text, and
// two of different values two. Its digits without leading or trailing
// zeros, and the power of ten they are scaled by. ok false for a text that
// is no JSON number, or whose exponent does not fit in 62 bits.
func canonicalNumber(s string) (string, bool) {
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	mant, exp := s, int64(0)
	if i := strings.IndexAny(s, "eE"); i >= 0 {
		e, err := strconv.ParseInt(strings.TrimPrefix(s[i+1:], "+"), 10, 64)
		if err != nil || e > 1<<62 || e < -(1<<62) {
			return "", false
		}
		mant, exp = s[:i], e
	}
	intPart, frac := mant, ""
	if i := strings.IndexByte(mant, '.'); i >= 0 {
		intPart, frac = mant[:i], mant[i+1:]
	}
	digits := intPart + frac
	if digits == "" {
		return "", false
	}
	for _, c := range digits {
		if c < '0' || c > '9' {
			return "", false
		}
	}
	exp -= int64(len(frac))
	digits = strings.TrimLeft(digits, "0")
	if digits == "" {
		return "0", true
	}
	trimmed := strings.TrimRight(digits, "0")
	exp += int64(len(digits) - len(trimmed))
	sign := ""
	if neg {
		sign = "-"
	}
	return sign + trimmed + "e" + strconv.FormatInt(exp, 10), true
}
