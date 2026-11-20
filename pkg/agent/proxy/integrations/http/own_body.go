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
	rv, rok := recorded.Decoded()
	lv, lok := live.Decoded()
	switch {
	case rok && lok:
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
	case isFormURLEncoded(m.Spec.HTTPReq.Header):
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

// formKeysOnlyLive is the drift ("body."-prefixed, known so too) of the
// keys the live form body holds that rec, the recorded one, holds neither
// as written nor by the name they decode to (formKeyName), each named as
// formReqBodyNoise names a key: by its decoded name where an entry with
// patterns guards it (formGuards), else as written. A key known with no
// patterns is no drift, as formReqBodyNoise skips one. nil when there is
// none.
func formKeysOnlyLive(rec, live string, known map[string][]string) map[string][]string {
	recRaw, recNames := map[string]bool{}, map[string]bool{}
	for _, k := range formRawKeys(rec) {
		recRaw[k], recNames[formKeyName(k)] = true, true
	}
	guards := formGuards(known)
	var out map[string][]string
	for _, k := range formRawKeys(live) {
		if recRaw[k] || recNames[formKeyName(k)] {
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
