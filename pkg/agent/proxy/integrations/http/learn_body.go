package http

import (
	"encoding/binary"
	"hash/fnv"
	"io"
	"sort"
	"strings"

	"go.keploy.io/server/v3/pkg/models"
)

// learnPlainPrint is the print of m's recorded request as the learn step reads
// it (rp, m's recordedParts, under url noise n.url), where the learn step's
// comparison of a call with m (learn.Compare, as exactGroup and record's full
// round read it) finds no difference in media type, query parameters, or body
// only where the call has that print (learnLivePrint), ok false where it may
// find none in a request of another print: where m learned noise
// (ReqBodyNoise), which may excuse leaves of its query or body, and where its
// recorded body is a form or a multipart one (read key by key and part by
// part, as learnNoise.shape reads them). keploy.yml's noise of request bodies,
// the live side's other excuse, is learnLivePrint's. A recording that carries
// obfuscation noise (Mock.Noise), whose values are not compared, is read
// whatever its print: an index files it under its segment count too
// (learnsWide), which every call of its path reads (joinWild / wild cursor).
// The clocks an earlier learning or a declared rule names only keep learned
// noise from excusing a leaf (httpKnown.ExcusedPair), and m learned none.
func learnPlainPrint(m *models.Mock, rp httpParts, n learnNoise) (uint64, bool) {
	if len(m.Spec.ReqBodyNoise) > 0 {
		return 0, false
	}
	switch rp.mediaType {
	case "application/x-www-form-urlencoded", "multipart/form-data":
		return 0, false
	}
	return learnPartsPrint(rp, n)
}

// learnLivePrint is the print of a learned call's media type, masked query
// parameters, and body (learnPartsPrint), ok false where keploy.yml declares
// noise of request bodies (!n.body.Empty()), which may excuse a leaf of the
// body (httpKnown.ExcusedPair), or where the body is a form or multipart: the
// call then reads every recording of its path.
func (n learnNoise) learnLivePrint(live httpParts) (uint64, bool) {
	if !n.body.Empty() {
		return 0, false
	}
	switch live.mediaType {
	case "application/x-www-form-urlencoded", "multipart/form-data":
		return 0, false
	}
	return learnPartsPrint(live, n)
}

// learnPartsPrint combines the single canonical body print (learnPrint) with
// the request's media type and url-noise-masked query parameters (skipping
// keploy* params and joining repeated values with NUL, matching
// learnNoise.shape). It is the single derivation of learn.Compare exactness
// shared by replay's learnIndex/learnOwned/learnKeyIndexOf and record's
// full-round exact fast path.
func learnPartsPrint(p httpParts, n learnNoise) (uint64, bool) {
	bodySum, ok := learnPrint(p.body)
	if !ok {
		return 0, false
	}
	h := fnv.New64a()
	var buf [binary.MaxVarintLen64 + 1]byte
	tag := func(t byte, sz int) {
		buf[0] = t
		_, _ = h.Write(buf[:1+binary.PutUvarint(buf[1:], uint64(sz))])
	}
	// 1. Media type (fixed leaf in learnNoise.shape).
	tag('M', len(p.mediaType))
	_, _ = io.WriteString(h, p.mediaType)
	// 2. Masked query parameters (skipping keploy*, matching learnNoise.shape).
	if len(p.query) > 0 {
		keys := make([]string, 0, len(p.query))
		for k := range p.query {
			if strings.HasPrefix(strings.ToLower(k), "keploy") {
				continue
			}
			keys = append(keys, k)
		}
		sort.Strings(keys)
		tag('Q', len(keys))
		for _, k := range keys {
			tag('k', len(k))
			_, _ = io.WriteString(h, k)
			vs := p.query[k]
			if len(vs) == 1 {
				mv := maskURLNoise(vs[0], n.url)
				tag('v', len(mv))
				_, _ = io.WriteString(h, mv)
				continue
			}
			masked := make([]string, len(vs))
			for i, v := range vs {
				masked[i] = maskURLNoise(v, n.url)
			}
			sort.Strings(masked)
			joined := strings.Join(masked, "\x00")
			tag('m', len(joined))
			_, _ = io.WriteString(h, joined)
		}
	} else {
		tag('Q', 0)
	}
	// 3. Canonical body print (learnPrint -> printBody).
	var sumBuf [9]byte
	sumBuf[0] = 'B'
	binary.LittleEndian.PutUint64(sumBuf[1:], bodySum)
	_, _ = h.Write(sumBuf[:])
	return h.Sum64(), true
}

// learnPrint is bodyPrint of a body as the learn step's shape reads it
// (learnNoise.shape, outside a form and a multipart body): bytes that do not
// decode as one JSON value are compared whole, as text, and printed whole;
// two JSON documents are exact only where they hold the same leaves
// (learn.JSONLeaves: each scalar by its type and text, each empty container
// by its type, at its path), which, where no key holds '.' or '[', is
// the same members, the same elements in the same order and scalars of one
// text, which bodyPrint prints alike. ok is false where a key of the
// document holds one of those, so that two documents of other members may
// share their leaves' paths ({"a.b":1} and {"a":{"b":1}}, {"a[0]":1} and
// {"a":[1]}), or bodyPrint refuses the document.
func learnPrint(body []byte) (uint64, bool) {
	v, isJSON := decodeJSONDoc(body)
	if !pathKeys(v) {
		return 0, false
	}
	return printBody(body, v, isJSON, nil)
}

// pathKeys reports whether no key of the decoded JSON value v holds '.' or
// '[', which each leaf's path (learn.JSONLeaves) is split at: each path then
// names one place of v (a ']' in a key ends nothing). Of no value (a body
// that is no JSON document) it reports true.
func pathKeys(v interface{}) bool {
	switch t := v.(type) {
	case map[string]interface{}:
		for k, child := range t {
			if strings.ContainsAny(k, ".[") || !pathKeys(child) {
				return false
			}
		}
	case []interface{}:
		for _, child := range t {
			if !pathKeys(child) {
				return false
			}
		}
	}
	return true
}
