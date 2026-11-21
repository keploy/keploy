package http

import (
	"maps"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"weak"

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
// differs too, each key's drift named by its decoded name, as the strict
// comparison names it, so the noise entry a report names for it is one
// keploy reads as covering it. Any other body has no fields to compare
// (comparable false).
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
		out := formReqBodyNoise(rec, call, prefixed, valIsNoise, true)
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
// name (formKeyName) the other form holds by no key, by that name, as
// DiffBothWays names a form key's drift. None where the two have no fields
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

// formKeysOnOneSide adds to out the decoded name (formKeyName) of each key
// the form body holds that other, the other form, holds by no key that
// decodes to it, as DiffBothWays names a form key's drift.
func formKeysOnOneSide(body, other string, out map[string]bool) {
	names := map[string]bool{}
	for _, k := range formRawKeys(other) {
		names[formKeyName(k)] = true
	}
	for _, k := range formRawKeys(body) {
		if !names[formKeyName(k)] {
			out["body."+formKeyName(k)] = true
		}
	}
}

// formKeysOnlyLive is the drift ("body."-prefixed, known so too) of the
// keys the live form body holds that rec, the recorded one, holds by no key
// that decodes to the same name (formKeyName), each named by that name, as
// formReqBodyNoise names a key by name. A key an entry with no patterns
// names is no drift, as formReqBodyNoise skips one: as written, or by its
// decoded name as the strict comparison reads a key only the live body
// holds (formKeyExcused). nil when there is none.
func formKeysOnlyLive(rec, live string, known map[string][]string) map[string][]string {
	recNames := map[string]bool{}
	for _, k := range formRawKeys(rec) {
		recNames[formKeyName(k)] = true
	}
	excused := formKeyExcused(known)
	var out map[string][]string
	for _, k := range formRawKeys(live) {
		if recNames[formKeyName(k)] {
			continue
		}
		if pats, ok := known["body."+k]; ok && len(pats) == 0 || excused(k, true) {
			continue
		}
		if out == nil {
			out = map[string][]string{}
		}
		out["body."+formKeyName(k)] = []string{}
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

// ownBodies keeps, for sameCallBodies, each recording's request body as
// the recording holds it, its print (ownRead.printOf) and, once a
// comparison has read it whole, that read (ownRead.json), so a call that
// reaches the fuzzy fallback reads only its own body, not every
// recording's again, however large the pool. A recording is kept by a weak
// pointer to it (weak.Make), which is the same for each read of the
// recording while a store holds it and keeps nothing alive, beside the body
// it was read from: a recording whose body is not that one is read again.
// What is kept of a recording goes when the recording does: the runtime
// calls forget once nothing holds the recording (runtime.AddCleanup), so
// the reads of a test set the store let go of are let go of with it, and
// the recordings a store holds are each read once, never again per call.
// Safe for concurrent use: a kept read is only read, and a print or a
// whole read is published whole (atomic.Pointer).
type ownBodies struct {
	mu sync.Mutex
	of map[weak.Pointer[models.Mock]]ownBody
}

type ownBody struct {
	body string
	read *ownRead
}

// ownRead is a recording's request body (body), its print as sameCallBodies
// last took it (printOf) and, once a comparison needed it, the body read
// whole (json). A recording only printed holds no read of its body: its
// print is a number, so the recordings a call is never compared with keep
// no decoded document alive for the garbage collector to walk.
type ownRead struct {
	body  string
	whole atomic.Pointer[mocknoise.JSONBody]
	print atomic.Pointer[ownPrint]
}

// ownBodyReads counts the recorded bodies read (decoded): one to print a
// recording, and one, kept, the first time a comparison reads it whole, so
// a row can count that neither grows with the calls.
var ownBodyReads atomic.Int64

// noRead is the read of a recording of no request: it holds no body.
var noRead = &ownRead{}

// read is m's recorded request body as ownBodies keeps it (false: m
// records no request, and its read is noRead).
func (o *ownBodies) read(m *models.Mock) (*ownRead, bool) {
	if m.Spec.HTTPReq == nil {
		return noRead, false
	}
	body, key := m.Spec.HTTPReq.Body, weak.Make(m)
	o.mu.Lock()
	defer o.mu.Unlock()
	b, kept := o.of[key]
	if kept && b.body == body {
		return b.read, true
	}
	if o.of == nil {
		o.of = map[weak.Pointer[models.Mock]]ownBody{}
	}
	read := &ownRead{body: body}
	o.of[key] = ownBody{body: body, read: read}
	if !kept {
		// One cleanup for each recording kept: a body read again replaces
		// what is kept under the key the cleanup forgets.
		runtime.AddCleanup(m, o.forget, key)
	}
	return read, true
}

// forget lets go of what o keeps of the recording key points to, which
// nothing holds any more.
func (o *ownBodies) forget(key weak.Pointer[models.Mock]) {
	o.mu.Lock()
	defer o.mu.Unlock()
	delete(o.of, key)
}

// json is r's body read whole (mocknoise.NewReadJSONBody), read the first
// time a comparison needs it and kept; nil for noRead, which holds none.
// Two calls that read it at once may each read it: the two reads are the
// same, and either is kept.
func (r *ownRead) json() *mocknoise.JSONBody {
	if r == noRead {
		return nil
	}
	if j := r.whole.Load(); j != nil {
		return j
	}
	ownBodyReads.Add(1)
	j := mocknoise.NewReadJSONBody([]byte(r.body))
	r.whole.Store(j)
	return j
}

// decoded is r's body decoded as JSON (ok false: it is not JSON): the
// whole read's (json) where a comparison read it, else decoded for the
// caller alone and let go.
func (r *ownRead) decoded() (any, bool) {
	if j := r.whole.Load(); j != nil {
		return j.Decoded()
	}
	ownBodyReads.Add(1)
	return mocknoise.NewJSONBody([]byte(r.body)).Decoded()
}

// ownPrint is a recording's body's print (matcher.JSONPrinter), and what it
// was printed under: keploy.yml's request-body noise (userKey, its keys as
// noiseKey writes them), the noise the record learned on the recording
// (learned, MockSpec.ReqBodyNoise's keys sorted; learnedKey, them as keysKey
// writes them) and the values the obfuscator redacted in it (noise,
// Mock.Noise), which decide decided. A body that does not decode as JSON
// has no print: its print is undecided.
type ownPrint struct {
	userKey    string
	learned    []string
	learnedKey string
	noise      []string
	sum        uint64
	decided    bool
}

// ownPrints counts the recorded bodies printed (ownRead.printOf),
// livePrints the calls' bodies printed (callPrints.another), and
// sameCallChecks the recordings sameCallBodies compared with the call, so a
// row can count that each recording is printed once, a call once for each
// noise, and that a recording whose print is not the call's is not
// compared.
var ownPrints, livePrints, sameCallChecks atomic.Int64

// under reports whether p was printed under userKey and m's noise as m
// holds it now: its learned noise's keys, its redacted values.
func (p *ownPrint) under(m *models.Mock, userKey string) bool {
	learned := httpNoiseAdapter{}.StoredNoise(m)
	if p.userKey != userKey || !slices.Equal(p.noise, m.Noise) || len(p.learned) != len(learned) {
		return false
	}
	for _, k := range p.learned {
		if _, ok := learned[k]; !ok {
			return false
		}
	}
	return true
}

// printOf is the print of r, m's recorded body, under user, keploy.yml's
// request-body noise (its keys userKey), and the noise the record learned
// on m (printedNoise): printed once, and again where m's noise or user's
// keys are not what it was printed under (ownPrint.under). A value the
// obfuscator redacted in it (RecordedValueIsNoise) leaves its print
// undecided.
func (r *ownRead) printOf(m *models.Mock, user map[string][]string, userKey string) *ownPrint {
	if p := r.print.Load(); p != nil && p.under(m, userKey) {
		return p
	}
	ownPrints.Add(1)
	learned := slices.Sorted(maps.Keys(httpNoiseAdapter{}.StoredNoise(m)))
	p := &ownPrint{userKey: userKey, learned: learned, learnedKey: keysKey(learned), noise: slices.Clone(m.Noise)}
	if rv, ok := r.decoded(); ok {
		p.sum, p.decided = matcher.NewJSONPrinter(printedNoise(user, learned)).Print(rv, httpNoiseAdapter{}.RecordedValueIsNoise(m))
	}
	r.print.Store(p)
	return p
}

// printedNoise is the noise a recording is printed under: user's entries
// and those the record learned on it (their keys, learned), each read as a
// path of the body as StripBodyPrefix reads it, whatever the proven switch.
// It holds every entry SameCallDrift compares the recording under, so its
// print leaves out at least what that comparison does not compare.
func printedNoise(user map[string][]string, learned []string) map[string][]string {
	out := make(map[string][]string, len(user)+len(learned))
	for k := range user {
		out[k] = nil
	}
	for _, k := range learned {
		out[strings.TrimPrefix(k, "body.")] = nil
	}
	return out
}

// noiseKey is noise's keys, sorted, as keysKey writes them.
func noiseKey(noise map[string][]string) string {
	return keysKey(slices.Sorted(maps.Keys(noise)))
}

// keysKey writes keys, each after its length, so two lists of other keys
// are never written alike.
func keysKey(keys []string) string {
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(strconv.Itoa(len(k)))
		b.WriteByte(':')
		b.WriteString(k)
	}
	return b.String()
}

// callPrints is a call's body's prints (matcher.JSONPrinter) under
// keploy.yml's request-body noise (user, its keys userKey) and, for each
// noise a recording it is compared with learned (ownPrint.learnedKey), that
// noise too: each printed once per call.
type callPrints struct {
	live    *mocknoise.JSONBody
	user    map[string][]string
	userKey string
	of      map[string]uint64
}

func newCallPrints(live *mocknoise.JSONBody, user map[string][]string) *callPrints {
	return &callPrints{live: live, user: user, userKey: noiseKey(user), of: map[string]uint64{}}
}

// another reports whether r, m's recorded body as ownBodies keeps it, is
// by its print another call's: both bodies are JSON, and their prints under
// the same noise differ, so the two differ at a field SameCallDrift
// compares. false says nothing: the prints are the same, a body is not
// JSON, or r's print is undecided.
func (c *callPrints) another(m *models.Mock, r *ownRead) bool {
	lv, ok := c.live.Decoded()
	if !ok {
		return false
	}
	p := r.printOf(m, c.user, c.userKey)
	if !p.decided {
		return false
	}
	live, ok := c.of[p.learnedKey]
	if !ok {
		livePrints.Add(1)
		live, _ = matcher.NewJSONPrinter(printedNoise(c.user, p.learned)).Print(lv, nil)
		c.of[p.learnedKey] = live
	}
	return live != p.sum
}

// callBodyRead is a call's request body (raw) read for one match: made
// (mocknoise.NewJSONBody, which reads it the first time a comparison
// does) the first time a check asks for it (get), so a match that compares
// it with no recording makes nothing, and every check of the match reads
// the one made.
type callBodyRead struct {
	raw  []byte
	body *mocknoise.JSONBody
}

// callBodiesMade counts the call bodies callBodyRead made, so a row can
// count one for a match whatever number of checks read it.
var callBodiesMade atomic.Int64

func (c *callBodyRead) get() *mocknoise.JSONBody {
	if c.body == nil {
		callBodiesMade.Add(1)
		c.body = mocknoise.NewJSONBody(c.raw)
	}
	return c.body
}
