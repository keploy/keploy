package http

import (
	"strings"
	"sync"
	"sync/atomic"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mocknoise"
	"go.keploy.io/server/v3/pkg/matcher"
	"go.keploy.io/server/v3/pkg/models"
)

// DiffBothWays compares the recorded and the live HTTP request body both
// ways (mocknoise.BothWaysDiffer). JSON bodies (both as pkg.IsJSON reads
// them, the branch Diff takes) are compared whole
// (matcher.DecodedJSONFieldsDiffering): a field only one side holds, an
// array of another length or with another element anywhere, differs.
// Form-urlencoded bodies are compared key by key as Diff compares them
// (formReqBodyNoise), and a key only the live body holds (formKeysOnlyLive)
// differs too. Any other body has no fields to compare (comparable false).
func (httpNoiseAdapter) DiffBothWays(m *models.Mock, recorded, live *mocknoise.JSONBody, known map[string][]string, typeStrict map[string]struct{}, valIsNoise func(string) bool) (map[string][]string, bool) {
	if m == nil || m.Spec.HTTPReq == nil {
		return nil, false
	}
	rv, lv, asJSON, asForm := bodiesCompared(m, recorded, live)
	switch {
	case asJSON:
		var out map[string][]string
		for _, p := range matcher.DecodedJSONFieldsDiffering(rv, lv, known, typeStrict, valIsNoise) {
			if out == nil {
				out = map[string][]string{}
			}
			if p == "" {
				// The two are not the same kind of document: no field
				// of either names where they differ.
				out["body"] = nil
				continue
			}
			out["body."+p] = []string{}
		}
		return out, true
	case asForm:
		rec, call := string(recorded.Raw()), string(live.Raw())
		prefixed := mocknoise.AddBodyPrefix(known)
		out := formReqBodyNoise(rec, call, prefixed, valIsNoise)
		for k, v := range formKeysOnlyLive(rec, call, prefixed) {
			if out == nil {
				out = map[string][]string{}
			}
			out[k] = v
		}
		return out, true
	default:
		return nil, false
	}
}

// bodiesCompared is how DiffBothWays compares recorded and live, m's
// recorded request body (m records a request) and the call's: as JSON
// documents where both decode as JSON (asJSON, rv and lv their decoded
// values), else as forms where m's recorded request is form-urlencoded
// (asForm), else not at all.
func bodiesCompared(m *models.Mock, recorded, live *mocknoise.JSONBody) (rv, lv any, asJSON, asForm bool) {
	rv, rok := recorded.Decoded()
	lv, lok := live.Decoded()
	if rok && lok {
		return rv, lv, true, false
	}
	return nil, nil, false, isFormURLEncoded(m.Spec.HTTPReq.Header)
}

// fieldsOnOneSide names, "body."-prefixed as DiffBothWays names drift, the
// fields only one of recorded and live, m's recorded request body and the
// call's, holds, whichever side it is, read with no noise and compared as
// DiffBothWays compares the two (bodiesCompared): JSON fields as
// matcher.DecodedJSONFieldsOnOneSide names them, or form keys whose decoded
// name (formKeyName) the other form holds by no key, each by both names its
// drift can have, as written and decoded. None where the two have no fields
// to compare.
func fieldsOnOneSide(m *models.Mock, recorded, live *mocknoise.JSONBody) map[string]bool {
	rv, lv, asJSON, asForm := bodiesCompared(m, recorded, live)
	out := map[string]bool{}
	switch {
	case asJSON:
		for _, p := range matcher.DecodedJSONFieldsOnOneSide(rv, lv) {
			out["body."+p] = true
		}
	case asForm:
		formKeysOnOneSide(string(recorded.Raw()), string(live.Raw()), out)
		formKeysOnOneSide(string(live.Raw()), string(recorded.Raw()), out)
	}
	return out
}

// formKeysOnOneSide adds to out each key the form body holds whose decoded
// name (formKeyName) other, the other form, holds by no key, by both names
// a form key's drift can have (formReqBodyNoise, formKeysOnlyLive): as
// written, and by its decoded name where an entry with patterns guards it.
func formKeysOnOneSide(body, other string, out map[string]bool) {
	names := map[string]bool{}
	for _, k := range formRawKeys(other) {
		names[formKeyName(k)] = true
	}
	for _, k := range formRawKeys(body) {
		if !names[formKeyName(k)] {
			out["body."+k] = true
			out["body."+formKeyName(k)] = true
		}
	}
}

// formKeysOnlyLive is the drift ("body."-prefixed, known so too) of the
// keys the live form body holds that rec, the recorded one, holds by no key
// that decodes to the same name (formKeyName), each named as
// formReqBodyNoise names a key: by its decoded name where an entry with
// patterns guards it (formGuards), else as written. A key known with no
// patterns is no drift, as formReqBodyNoise skips one. nil when there is
// none.
func formKeysOnlyLive(rec, live string, known map[string][]string) map[string][]string {
	recNames := map[string]bool{}
	for _, k := range formRawKeys(rec) {
		recNames[formKeyName(k)] = true
	}
	guards := formGuards(known)
	var out map[string][]string
	for _, k := range formRawKeys(live) {
		if recNames[formKeyName(k)] {
			continue
		}
		key := "body." + k
		if len(guards(k)) > 0 {
			key = "body." + formKeyName(k)
		} else if pats, ok := known[key]; ok && len(pats) == 0 {
			continue
		}
		if out == nil {
			out = map[string][]string{}
		}
		out[key] = []string{}
	}
	return out
}

// formRawKeys are a form body's keys as written, in body order, a repeated
// key as often as it is written, split as formReqBodyNoise splits them.
func formRawKeys(body string) []string {
	var out []string
	for _, seg := range strings.Split(body, "&") {
		if seg == "" {
			continue
		}
		if i := strings.IndexByte(seg, '='); i >= 0 {
			seg = seg[:i]
		}
		out = append(out, seg)
	}
	return out
}

// ownBodiesMaxBytes bounds the recorded request bodies ownBodies keeps:
// past it, it starts again from none. A recording's body read whole takes a
// few times its bytes.
const ownBodiesMaxBytes = 8 << 20

// ownBodies keeps, for sameCallBodies, each recording's request body read
// once (mocknoise.NewReadJSONBody), so a call that reaches the fuzzy
// fallback reads only its own body, not every recording's again. A
// recording is kept by its pointer, which a store gives the same on every
// read while it holds the recording, beside the body it was read from: a
// recording whose body is not that one is read again. Up to
// ownBodiesMaxBytes of bodies, so recordings a store let go of are let go
// of here too. Safe for concurrent use: a kept body is only read.
type ownBodies struct {
	mu    sync.Mutex
	bytes int
	of    map[*models.Mock]ownBody
}

type ownBody struct {
	body string
	read *mocknoise.JSONBody
}

// ownBodyReads counts the recorded bodies ownBodies read, so a row can
// count that each is read once.
var ownBodyReads atomic.Int64

// read is m's recorded request body, read once (false: m records no
// request).
func (o *ownBodies) read(m *models.Mock) (*mocknoise.JSONBody, bool) {
	if m.Spec.HTTPReq == nil {
		return nil, false
	}
	body := m.Spec.HTTPReq.Body
	o.mu.Lock()
	b, ok := o.of[m]
	o.mu.Unlock()
	if ok && b.body == body {
		return b.read, true
	}
	ownBodyReads.Add(1)
	read := mocknoise.NewReadJSONBody([]byte(body))
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.of == nil || o.bytes+len(body) > ownBodiesMaxBytes {
		o.of, o.bytes = map[*models.Mock]ownBody{}, 0
	}
	o.bytes += len(body)
	o.of[m] = ownBody{body: body, read: read}
	return read, true
}
