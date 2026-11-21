package http

import (
	"strings"

	"go.keploy.io/server/v3/pkg/models"
)

// learnPlainPrint is the print of m's recorded body as the learn step reads
// it (rp, m's recordedParts), where the learn step's comparison of a call
// with m (learn.Compare, as exactGroup reads it) finds no difference in the
// body only where the call's body has that print (learnPrint), ok false
// where it may find none in a body of another print: where m learned noise
// (ReqBodyNoise), which excuses the leaves it names, and where its recorded
// body is a form or a multipart one (read key by key and part by part, as
// learnNoise.shape reads them). keploy.yml's noise of request bodies, the
// live side's other excuse, is learnLivePrint's. A recording that carries
// obfuscation noise (Mock.Noise), whose values are not compared, is read
// whatever its print: an index files it under its segment count too
// (learnsWide), which every call of its path reads (joinWild). The clocks an
// earlier learning or a declared rule names only keep learned noise from
// excusing a leaf (httpKnown.ExcusedPair), and m learned none.
func learnPlainPrint(m *models.Mock, rp httpParts) (uint64, bool) {
	if len(m.Spec.ReqBodyNoise) > 0 {
		return 0, false
	}
	switch rp.mediaType {
	case "application/x-www-form-urlencoded", "multipart/form-data":
		return 0, false
	}
	return learnPrint(rp.body)
}

// learnLivePrint is the print of a learned call's body (learnPrint), ok
// false where keploy.yml declares noise of request bodies, which may excuse
// a leaf of the body (httpKnown.ExcusedPair): the call then reads every
// recording of its path.
func (n learnNoise) learnLivePrint(live httpParts) (uint64, bool) {
	if !n.body.Empty() {
		return 0, false
	}
	return learnPrint(live.body)
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
