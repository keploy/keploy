package mocknoise

import (
	"encoding/json"
	"io"
	"net/url"
	"reflect"
	"strings"
)

// IsUUID reports whether v is written as a UUID: 8-4-4-4-12 hex digits.
func IsUUID(v string) bool { return isUUID(v) }

// IsMintedUUID reports whether v is a UUID of a kind an application generates
// anew on every run: time-based (versions 1, 6, 7) or random (version 4), in
// the RFC 9562 variant. A name-based UUID (versions 3 and 5) is derived from
// its input, so it is the same on every run of the same input and a different
// one means the input changed; the nil and max UUIDs are constants.
//
// It is the one shape a replay may take for minted without being told: every
// other app-random shape (a hex digest, a long token) is as often derived or
// configured as it is minted.
func IsMintedUUID(v string) bool {
	if !isUUID(v) {
		return false
	}
	switch v[14] {
	case '1', '4', '6', '7':
	default:
		return false
	}
	switch v[19] {
	case '8', '9', 'a', 'b', 'A', 'B':
		return true
	}
	return false
}

// AlignStrings walks a recorded and a live HTTP request together and calls
// pair for every place both hold a string, and alone for every recorded string
// at a place the live request has none to set beside it.
//
// The places are those a value can be read off whole: a URL path segment
// (when both paths have the same number of segments), a query value (when the
// key has one value on both sides) and a string in a JSON body — objects by
// key, arrays of one length by position. A body that is not one JSON object or
// array has no such places.
//
// It is the one way two requests are lined up: the agent decides with it which
// ids a request made anew.
func AlignStrings(recURL, recBody, liveURL, liveBody string, pair func(recorded, live string), alone func(recorded string)) {
	alignPlaces(placesOf(recURL, recBody), placesOf(liveURL, liveBody), pair, alone)
}

// WalkStrings calls visit with every string of one request that AlignStrings
// can set beside another request's: the strings at its places when it is lined
// up with itself. A value that stands anywhere else — in a header, in a body
// that is not one JSON document (a form, NDJSON), under a query key that
// repeats, inside a longer string — is at no place, and can never be told from
// the value another request holds there.
//
// It is AlignStrings' own walk, so what it lists and what AlignStrings pairs
// cannot disagree.
func WalkStrings(rawURL, body string, visit func(string)) {
	p := placesOf(rawURL, body)
	alignPlaces(p, p, func(s, _ string) { visit(s) }, func(string) {})
}

// places are the parts of one request AlignStrings walks.
type places struct {
	hasURL   bool // the URL parsed
	segments []string
	query    url.Values
	doc      any // the body, when it is one JSON object or array
}

func placesOf(rawURL, body string) places {
	u, err := url.Parse(rawURL)
	if err != nil {
		u = &url.URL{}
	}
	p := places{hasURL: err == nil, segments: strings.Split(strings.Trim(u.Path, "/"), "/"), query: u.Query()}
	p.doc, _ = decodeJSONContainer(body)
	return p
}

func alignPlaces(rec, live places, pair func(recorded, live string), alone func(recorded string)) {
	if rec.hasURL {
		for i, seg := range rec.segments {
			if len(rec.segments) == len(live.segments) {
				pair(seg, live.segments[i])
			} else {
				alone(seg)
			}
		}
		for k, rv := range rec.query {
			if lv := live.query[k]; len(rv) == 1 && len(lv) == 1 {
				pair(rv[0], lv[0])
				continue
			}
			for _, v := range rv {
				alone(v)
			}
		}
	}
	if rec.doc == nil {
		return
	}
	alignJSON(rec.doc, live.doc, pair, alone) // a live body that is not JSON: every recorded string is alone
}

func alignJSON(rec, live any, pair func(string, string), alone func(string)) {
	switch rv := rec.(type) {
	case map[string]any:
		lv, _ := live.(map[string]any)
		for k, rc := range rv {
			if lc, ok := lv[k]; ok {
				alignJSON(rc, lc, pair, alone)
			} else {
				alignJSON(rc, nil, pair, alone)
			}
		}
	case []any:
		lv, _ := live.([]any)
		for i, rc := range rv {
			if len(lv) == len(rv) {
				alignJSON(rc, lv[i], pair, alone)
			} else {
				alignJSON(rc, nil, pair, alone)
			}
		}
	case string:
		if lv, ok := live.(string); ok {
			pair(rv, lv)
		} else {
			alone(rv)
		}
	}
}

// decodeJSONContainer parses a body that is one JSON object or array, keeping
// numbers as written.
func decodeJSONContainer(body string) (any, bool) {
	t := strings.TrimLeft(body, " \t\r\n")
	if t == "" || (t[0] != '{' && t[0] != '[') {
		return nil, false
	}
	dec := json.NewDecoder(strings.NewReader(body))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, false
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, false
	}
	return v, true
}

// SameJSONBut reports whether two request bodies are the same JSON document
// but for the fields at the given paths: every other key is on both sides,
// arrays are of one length and agree position by position, and scalars are of
// one type and written the same.
//
// paths are request-body noise as the engine keeps and is handed it
// (Engine.KnownNoise): root-relative, "a.b", the elements of an array as
// "a[]", compared without regard to case. A path sets aside that field and
// everything under it, and only that field: "id" is the root's id — not
// "userId", and not "item.id". An entry that carries value patterns does not
// describe a whole field and sets nothing aside; neither does an empty path.
//
// DetectJSONDrift answers another question — which fields drift — and is loose
// where that needs it to be: it reads a path as a substring of a field's, folds
// an array into its last element, and does not count a field only one side
// has. None of that can decide that two requests are the same request.
func SameJSONBut(recorded, live string, paths map[string][]string) bool {
	a, ok := decodeJSONContainer(recorded)
	if !ok {
		return false
	}
	b, ok := decodeJSONContainer(live)
	if !ok {
		return false
	}
	aside := make(map[string]bool, len(paths))
	for p, patterns := range paths {
		if p != "" && len(patterns) == 0 { // the empty path names no field
			aside[strings.ToLower(p)] = true
		}
	}
	return sameJSONBut(a, b, "", aside)
}

func sameJSONBut(a, b any, path string, aside map[string]bool) bool {
	if aside[strings.ToLower(path)] {
		return true
	}
	switch av := a.(type) {
	case map[string]any:
		bv, ok := b.(map[string]any)
		if !ok {
			return false
		}
		child := func(k string) string {
			if path == "" {
				return k
			}
			return path + "." + k
		}
		for k, ac := range av {
			bc, ok := bv[k]
			if !ok {
				if !aside[strings.ToLower(child(k))] {
					return false
				}
				continue
			}
			if !sameJSONBut(ac, bc, child(k), aside) {
				return false
			}
		}
		for k := range bv {
			if _, ok := av[k]; !ok && !aside[strings.ToLower(child(k))] {
				return false
			}
		}
		return true
	case []any:
		bv, ok := b.([]any)
		if !ok || len(av) != len(bv) {
			return false
		}
		for i := range av {
			if !sameJSONBut(av[i], bv[i], path+"[]", aside) {
				return false
			}
		}
		return true
	default:
		// Scalars: json.Number, string, bool or nil — the same type, written
		// the same.
		return reflect.TypeOf(a) == reflect.TypeOf(b) && a == b
	}
}
