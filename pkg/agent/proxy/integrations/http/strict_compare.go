package http

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"math/big"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/util"
	"go.keploy.io/server/v3/pkg/matcher"
	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// The strict comparison of S1 (design v14 §9, rows S1 and §7.2 item 8): under
// the proven switch a recorded HTTP request answers a live one only when the
// live one IS the recording, outside known noise. Every part is compared by
// value: the method, the destination (strict.go), the path, every query value,
// every header value, and the body, field by field and array element by
// array element. What may differ is only what is known:
//
//   - keploy.yml's noise: header names (test.globalNoise header), url
//     patterns (url) and request-body paths (requestbody: each names whole
//     fields, never part of a field's name, matcher.NoiseLookup);
//   - the static header list, minus the credentials and the payload hash
//     (models.ProvenFlakyHeaderNoise);
//   - what was learned onto the mock (MockSpec.ReqBodyNoise): by the learn
//     step, which names a field by its exact place (body.items[0].id,
//     header.<name>, url.query.<name>, url.path.<i>), or by the request-body
//     learner it replaces (body.items[].id). A learned field's value may
//     change; its type and its presence may not;
//   - a recorded value the obfuscator redacted (Mock.Noise).
//
// A place the learn step learned as a read of the application's clock
// (MockSpec.Clocks) is no noise: its value may change, and is checked apart,
// against the call's window on the application's clock (clockDiffs), and no
// learned noise excuses it; keploy.yml's noise still does.
//
// The User-Agent is compared by its first product token's name, so an SDK's
// version bump is not a different caller while another client is. The
// credentials (Authorization, the idempotency keys, the CSRF tokens) are
// compared: a token minted in the test is a variable (A2), one from the
// environment binds, one that drifts at record's F is learned as noise, and
// anything else must be the recorded one. An AWS SigV4 Authorization is
// compared by its parts, its signature not at all (strict_sigv4.go). No
// score, no closest match: the recording either is the call or does not
// answer it.

// strictNoise is the noise of the run's configuration the strict comparison
// reads; each mock adds its own learned noise and obfuscated values.
type strictNoise struct {
	// header holds the lowercased header names never compared: keploy.yml's
	// and the static list's.
	header map[string]bool
	// body is keploy.yml's request-body noise.
	body matcher.NoiseLookup
	// url is keploy.yml's url noise: what a pattern matches in the path or
	// a query value is not compared.
	url []*regexp.Regexp
	// notLearned are leaf paths where noise learned onto the mock
	// (MockSpec.ReqBodyNoise) is not applied: the leaves of the mock's
	// variables, for the variable pass's comparison (fitsRecording).
	notLearned map[string]bool
}

func (h *HTTP) newStrictNoise(headerNoise, bodyNoise map[string][]string, urlNoise []string) strictNoise {
	n := strictNoise{header: make(map[string]bool, len(headerNoise)), body: matcher.NewNoiseLookup(bodyNoise), url: compileURLNoise(h.Logger, urlNoise)}
	for k := range headerNoise {
		n.header[strings.ToLower(k)] = true
	}
	return n
}

// strictKnown is what a comparison with one mock excuses.
type strictKnown struct {
	n       strictNoise
	learned map[string][]string
	values  *util.NoiseChecker
	// clocks are the places the learn step learned as reads of the
	// application's clock (clocksOf): the walk lets their value change, and
	// clockDiffs checks it.
	clocks map[string]models.ClockOffset
}

// clocksOf is what the learn step learned of m as reads of the
// application's clock (MockSpec.Clocks), under the proven switch; none
// without it, which compares as before it.
func clocksOf(m *models.Mock) map[string]models.ClockOffset {
	if m == nil || !provenswitch.On() {
		return nil
	}
	return m.Spec.Clocks
}

// learnedAt reports whether the mock learned the field at exact (the learn
// step's dialect: that field or a container of it) or at legacy (the
// request-body learner's dialect, by its exact key), or learned a read of the
// application's clock at exact, whose value clockDiffs checks.
func (k strictKnown) learnedAt(exact, legacy string) bool {
	if _, clock := k.clocks[exact]; clock {
		return true
	}
	if len(k.learned) == 0 || k.n.notLearned[exact] {
		return false
	}
	if _, ok := k.learned[exact]; ok {
		return true
	}
	if legacy != "" && legacy != exact {
		if _, ok := k.learned[legacy]; ok {
			return true
		}
	}
	for p := range k.learned {
		if strings.HasPrefix(exact, p+".") || strings.HasPrefix(exact, p+"[") {
			return true
		}
	}
	return false
}

// redacted reports whether a recorded value is one the obfuscator redacted.
func (k strictKnown) redacted(recorded string) bool {
	return recorded != "" && k.values != nil && k.values.IsNoisy(recorded)
}

// strictField is a compared field in which a call differs from a recording:
// its diff, as the miss report shows it, and its name with no part that may
// be a value (an object key the app sent: an ID, an email address), as the
// reason for a failed test names it.
type strictField struct {
	diff  models.MockFieldDiff
	shape string
	// why is, of a read of the application's clock a replay refused
	// (clockDiffs), the reason learn.CheckClockRead gives.
	why string
}

// field is a strictField whose name holds no value (a fixed part, a header).
func field(d models.MockFieldDiff) strictField { return strictField{diff: d, shape: d.Path} }

// shaped is a strictField named path in the report and shape in the reason.
func shaped(path, shape string, kind models.MockFieldDiffKind, expected, actual string) strictField {
	return strictField{diff: models.MockFieldDiff{Path: path, Kind: kind, Expected: expected, Actual: actual}, shape: shape}
}

// shapeKey is an object key, a form key or a query name as a reason names
// it: {} when it may be a value the app sent, by the rule a call's path
// segments follow (models.KeyShape).
func shapeKey(k string) string {
	return models.KeyShape(k)
}

// fieldDiffs are the fields' diffs, for the report.
func fieldDiffs(fields []strictField) []models.MockFieldDiff {
	out := make([]models.MockFieldDiff, len(fields))
	for i, f := range fields {
		out[i] = f.diff
	}
	return out
}

// strictDiff returns the compared fields in which the live request differs
// from m's recording outside known noise, nil when m's recording is this
// call. live is where the live request went; ctx is its connection's, whose
// learn tracker tells the call's window on the application's clock.
func strictDiff(ctx context.Context, input *req, live destination, m *models.Mock, n strictNoise) []strictField {
	r := m.Spec.HTTPReq
	if r == nil {
		return []strictField{field(models.MockFieldDiff{Path: "method", Kind: models.DiffKindMissingInMock, Actual: input.method})}
	}
	strictDiffs.Add(1)
	k := strictKnown{n: n, learned: m.Spec.ReqBodyNoise, values: util.NewNoiseChecker(m.Noise), clocks: clocksOf(m)}
	var out []strictField
	if string(r.Method) != input.method {
		out = append(out, field(valueDiff("method", string(r.Method), input.method)))
	}
	for _, d := range destinationDiff(live, m) {
		out = append(out, field(d))
	}
	recPath, recQuery := recordedStrictURL(m)
	livePath := ""
	var liveQuery url.Values
	if input.url != nil {
		livePath, liveQuery = input.url.Path, input.url.Query()
	}
	out = append(out, k.pathDiffs(recPath, livePath)...)
	out = append(out, k.queryDiffs(recQuery, liveQuery)...)
	out = append(out, k.headerDiffs(r.Header, input.header, input.sentBody())...)
	out = append(out, k.bodyDiffs(r, input)...)
	return append(out, k.clockDiffs(ctx, input)...)
}

// clockDiffs are the places the learn step learned as reads of the
// application's clock (k.clocks) at which the call carries no such read made
// in its own window on the application's clock (learn.ClockSpanFrom,
// learn.CheckClockRead), each with the reason it gives; none where
// keploy.yml's noise reaches the place, which the user declared not
// compared. Every place is checked, whatever the walk found: a value equal to
// its recording is a read of this run's clock only where the application's
// clock showed it in the call's window, and a place the call does not carry,
// or keploy cannot read, is refused. So is every place where keploy cannot
// tell the application's clock at the call.
func (k strictKnown) clockDiffs(ctx context.Context, input *req) []strictField {
	if len(k.clocks) == 0 {
		return nil
	}
	var leaves map[string]learn.Leaf
	if input.url != nil {
		leaves = learnNoise{}.shape(liveParts(ctx, input), "").Leaves
	}
	span, known := learn.ClockSpanFrom(ctx, input.arrived)
	places := make([]string, 0, len(k.clocks))
	for p := range k.clocks {
		places = append(places, p)
	}
	sort.Strings(places)
	var out []strictField
	for _, p := range places {
		live := leaves[p]
		if k.userNoiseAt(p, live.Value) {
			continue
		}
		// The reason names the field as a failed test's reason does (its
		// shape), never a value an object key of it may be.
		f := clockField(p)
		if ok, why := learn.CheckClockRead(f.shape, k.clocks[p], live, span, known); !ok {
			f.why = why
			out = append(out, f)
		}
	}
	return out
}

// userNoiseAt reports whether keploy.yml's noise reaches place, in the learn
// step's dialect, holding value: its header names, a url pattern that
// matches in a path segment or a query value, and its request-body paths.
func (k strictKnown) userNoiseAt(place, value string) bool {
	switch {
	case strings.HasPrefix(place, "header."):
		return k.n.header[strings.TrimPrefix(place, "header.")]
	case strings.HasPrefix(place, "url."):
		return maskURLNoise(value, k.n.url) != value
	case strings.HasPrefix(place, "body."):
		return k.n.body.Covers(riskOf(strings.TrimPrefix(place, "body.")), value, true)
	}
	return false
}

// arrayIndex is an array position in the learn step's dialect ([0]), which
// the request matcher's writes [].
var arrayIndex = regexp.MustCompile(`\[[0-9]+\]`)

// riskOf is a body path in the learn step's dialect (items[0].at) in the
// request matcher's, which keploy.yml's noise is written in (items[].at).
func riskOf(exact string) string {
	return arrayIndex.ReplaceAllString(exact, "[]")
}

// clockField is the field a refused read of the application's clock at
// place (the learn step's dialect) is, named as the walk names that field.
func clockField(place string) strictField {
	switch {
	case strings.HasPrefix(place, "url.path."):
		return field(models.MockFieldDiff{Path: "path", Kind: models.DiffKindValueChanged})
	case strings.HasPrefix(place, "url.query."):
		name := strings.TrimPrefix(place, "url.query.")
		return shaped("query."+name, "query."+shapeKey(name), models.DiffKindValueChanged, "", "")
	case strings.HasPrefix(place, "body."):
		risk := riskOf(strings.TrimPrefix(place, "body."))
		keys := strings.Split(risk, ".")
		for i, key := range keys {
			base := strings.TrimRight(key, "[]")
			keys[i] = shapeKey(base) + key[len(base):]
		}
		return shaped("body."+risk, "body."+strings.Join(keys, "."), models.DiffKindValueChanged, "", "")
	}
	return field(models.MockFieldDiff{Path: place, Kind: models.DiffKindValueChanged})
}

func valueDiff(path, expected, actual string) models.MockFieldDiff {
	return models.MockFieldDiff{Path: path, Kind: models.DiffKindValueChanged, Expected: expected, Actual: actual}
}

// recordedURL is a recorded request's path and query: the recorded URL's own
// query, or, for a recording that kept it only there, URLParams (which joins
// a repeated parameter's values with ", "). The path is decoded, as the
// request matcher, the learn step and keploy.yml's url noise read it: a
// percent-encoded slash ("a%2Fb") reads as two segments.
func recordedURL(r *models.HTTPReq) (string, url.Values) {
	parse := url.Parse
	if strings.HasPrefix(r.URL, "/") {
		// A path-only recording is read as a request line is: "//v1/charge"
		// is a path, not a host.
		parse = url.ParseRequestURI
	}
	u, err := parse(r.URL)
	if err != nil {
		return r.URL, nil
	}
	switch {
	case u.RawQuery != "":
		return u.Path, u.Query()
	case len(r.URLParams) > 0:
		q := make(url.Values, len(r.URLParams))
		for key, v := range r.URLParams {
			q[key] = strings.Split(v, ", ")
		}
		return u.Path, q
	}
	return u.Path, nil
}

// pathDiffs compares two paths segment by segment, after keploy.yml's url
// noise is masked in both; a segment the mock learned (url.path.<i>) may
// differ.
func (k strictKnown) pathDiffs(rec, live string) []strictField {
	if rec == live {
		return nil
	}
	rs := strings.Split(strings.TrimPrefix(maskURLNoise(rec, k.n.url), "/"), "/")
	ls := strings.Split(strings.TrimPrefix(maskURLNoise(live, k.n.url), "/"), "/")
	if len(rs) == len(ls) {
		same := true
		for i := range rs {
			if rs[i] != ls[i] && !k.learnedAt("url.path."+strconv.Itoa(i), "") {
				same = false
				break
			}
		}
		if same {
			return nil
		}
	}
	return []strictField{field(valueDiff("path", rec, live))}
}

// queryDiffs compares two queries: the same parameters, each with the same
// values (keploy.yml's url noise masked in both), a repeated parameter's in
// any order, as the request matcher and the learn step read them (an app may
// build them from a set); one value the mock learned (url.query.<name>) may
// differ. Parameters keploy itself adds (keploy*) are not compared. A SigV4
// presigned URL's signing parameters are read by their grammar
// (presignedDiffs).
func (k strictKnown) queryDiffs(rec, live url.Values) []strictField {
	byGrammar, signing := presignedDiffs(rec, live)
	names := map[string]bool{}
	for name := range rec {
		names[name] = true
	}
	for name := range live {
		names[name] = true
	}
	var out []strictField
	for _, name := range sortedNames(names) {
		if strings.HasPrefix(strings.ToLower(name), "keploy") {
			continue
		}
		if byGrammar[name] {
			if name == amzCredential {
				out = append(out, signing...)
			}
			continue
		}
		rv, inRec := rec[name]
		lv, inLive := live[name]
		path, shape := "query."+name, "query."+shapeKey(name)
		switch {
		case !inLive:
			out = append(out, shaped(path, shape, models.DiffKindMissingInLive, strings.Join(rv, ", "), ""))
			continue
		case !inRec:
			out = append(out, shaped(path, shape, models.DiffKindMissingInMock, "", strings.Join(lv, ", ")))
			continue
		}
		if sameMembers(maskValues(rv, k.n.url), maskValues(lv, k.n.url)) {
			continue
		}
		if len(rv) == 1 && len(lv) == 1 && k.learnedAt("url.query."+name, "") {
			continue
		}
		out = append(out, shaped(path, shape, models.DiffKindValueChanged, strings.Join(rv, ", "), strings.Join(lv, ", ")))
	}
	return out
}

// headerDiffs compares two header sets: the same headers, each with the same
// value. Not compared: Host (the destination is, strict.go), Content-Length
// (the body is), keploy's own headers, and the headers known noise names. A
// header the mock learned (header.<name>) or whose recorded value the
// obfuscator redacted may change its value, not go away. Content-Type is
// compared by its media types, so a boundary minted per request is no
// difference; User-Agent by its first product token's name; Authorization by
// its SigV4 parts, and x-amz-content-sha256 with sent, the body the client
// sent (strict_sigv4.go).
func (k strictKnown) headerDiffs(rec map[string]string, live http.Header, sent []byte) []strictField {
	recH := make(map[string]string, len(rec))
	for _, name := range sortedStringKeys(rec) {
		ln := strings.ToLower(name)
		if _, dup := recH[ln]; !dup {
			recH[ln] = rec[name]
		}
	}
	liveH := make(map[string][]string, len(live))
	for name, vs := range live {
		ln := strings.ToLower(name)
		liveH[ln] = append(liveH[ln], vs...)
	}
	names := map[string]bool{}
	for n := range recH {
		names[n] = true
	}
	for n := range liveH {
		names[n] = true
	}
	var out []strictField
	for _, name := range sortedNames(names) {
		if name == "host" || name == "content-length" || strings.HasPrefix(name, "keploy") || k.n.header[name] {
			continue
		}
		rv, inRec := recH[name]
		lvs, inLive := liveH[name]
		path := "header." + name
		switch {
		case !inLive:
			out = append(out, field(models.MockFieldDiff{Path: path, Kind: models.DiffKindMissingInLive, Expected: rv}))
			continue
		case !inRec:
			out = append(out, field(models.MockFieldDiff{Path: path, Kind: models.DiffKindMissingInMock, Actual: strings.Join(lvs, ",")}))
			continue
		}
		lv := strings.Join(lvs, ",")
		// A SigV4 Authorization is read by its parts before learned noise:
		// its signature differs on every run, so a learner that reads the
		// header whole (L, at record's full round) learns it, and that must
		// not excuse its credential scope or signed headers (founder
		// decision 25). A recorded value the obfuscator redacted is no
		// SigV4 to read, and is excused below as before.
		if name == "authorization" && !k.redacted(rv) {
			if _, recSigV4 := parseSigV4Authorization(rv); recSigV4 {
				parts, ok := sigV4AuthorizationDiffs(rv, lv)
				if !ok {
					// The live one is no SigV4 (another scheme, SigV4A, a
					// second line): noise learned on a SigV4 recording was
					// learned for its signature, and excuses nothing else.
					parts = []strictField{field(valueDiff(path, rv, lv))}
				}
				out = append(out, parts...)
				continue
			}
		}
		if k.learnedAt(path, "") || k.redacted(rv) {
			continue
		}
		switch name {
		case "x-amz-content-sha256":
			if samePayloadHash(rv, lvs, sent) {
				continue
			}
		default:
			if sameHeaderValue(name, rv, lvs) {
				continue
			}
		}
		out = append(out, field(valueDiff(path, rv, lv)))
	}
	return out
}

// sameHeaderValue reports whether a header's recorded value (its lines joined
// with ",", as the recorder writes them) and its live lines are the same, in
// the same order: the order of a repeated header's lines is significant (RFC
// 9110 §5.3).
func sameHeaderValue(name, rec string, live []string) bool {
	switch name {
	case "content-type":
		return mediaTypeKey(rec) == mediaTypeKey(strings.Join(live, ","))
	case "user-agent":
		return productName(rec) == productName(strings.Join(live, ","))
	}
	return rec == strings.Join(live, ",")
}

// mediaTypeKey is a Content-Type's media types, lowercased and sorted, without
// their parameters.
func mediaTypeKey(ct string) string {
	mts := parseMediaTypes(ct)
	for i := range mts {
		mts[i] = strings.ToLower(mts[i])
	}
	sort.Strings(mts)
	return strings.Join(mts, ",")
}

// productName is a User-Agent's first product token without its version
// (RFC 9110 §10.1.5: product = token ["/" product-version]): what names the
// client, whose version an upgrade changes.
func productName(ua string) string {
	first := strings.TrimSpace(ua)
	if i := strings.IndexAny(first, " \t"); i >= 0 {
		first = first[:i]
	}
	name, _, _ := strings.Cut(first, "/")
	return name
}

// sameMembers reports whether a and b hold the same values, in any order.
func sameMembers(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	as, bs := slices.Clone(a), slices.Clone(b)
	sort.Strings(as)
	sort.Strings(bs)
	return slices.Equal(as, bs)
}

// maskValues are vals with keploy.yml's url noise masked, in their order.
func maskValues(vals []string, noise []*regexp.Regexp) []string {
	out := make([]string, len(vals))
	for i, v := range vals {
		out[i] = maskURLNoise(v, noise)
	}
	return out
}

// bodyDiffs compares two bodies, by the recording's media type: a form or a
// multipart form field by field, JSON field by field and element by element,
// anything else byte for byte.
func (k strictKnown) bodyDiffs(r *models.HTTPReq, input *req) []strictField {
	rec, live := []byte(r.Body), input.body
	if bytes.Equal(rec, live) {
		return nil
	}
	if k.redacted(r.Body) {
		return nil // the whole recorded body is a redacted value
	}
	recCT := headerValue(r.Header, "Content-Type")
	switch mediaTypeKey(recCT) {
	case "application/x-www-form-urlencoded":
		return k.formDiffs(r.Body, string(live))
	case "multipart/form-data":
		return k.multipartDiffs(recCT, r.Body, input.header.Get("Content-Type"), live)
	}
	rv, rok := decodeJSONDoc(rec)
	lv, lok := decodeJSONDoc(live)
	if rok && lok {
		var out []strictField
		k.jsonDiffs(rv, lv, "body", "", "body", false, &out)
		return out
	}
	return []strictField{bodyChanged()}
}

// bodyChanged is the field of a body that differs as a whole.
func bodyChanged() strictField {
	return field(models.MockFieldDiff{Path: "body", Kind: models.DiffKindValueChanged})
}

// formDiffs compares two form bodies key by key: the same keys, each with the
// same values, a repeated key's in any order, as the learn step reads them.
func (k strictKnown) formDiffs(rec, live string) []strictField {
	rf, rerr := url.ParseQuery(rec)
	lf, lerr := url.ParseQuery(live)
	if rerr != nil || lerr != nil {
		return []strictField{bodyChanged()}
	}
	redacted := k.redactedFormKeys(rec)
	names := map[string]bool{}
	for key := range rf {
		names[key] = true
	}
	for key := range lf {
		names[key] = true
	}
	var out []strictField
	for _, key := range sortedNames(names) {
		path, shape := "body."+key, "body."+shapeKey(key)
		if k.n.body.Covers(key, "", false) {
			continue
		}
		rv, inRec := rf[key]
		lv, inLive := lf[key]
		switch {
		case !inLive:
			out = append(out, shaped(path, shape, models.DiffKindMissingInLive, "", ""))
			continue
		case !inRec:
			out = append(out, shaped(path, shape, models.DiffKindMissingInMock, "", ""))
			continue
		}
		if sameMembers(rv, lv) || k.learnedAt(path, "") || redacted[key] {
			continue
		}
		out = append(out, shaped(path, shape, models.DiffKindValueChanged, "", ""))
	}
	return out
}

// redactedFormKeys are the keys of a recorded form one of whose "key=value"
// segments, as written, the obfuscator redacted (its patterns are anchored on
// the segment).
func (k strictKnown) redactedFormKeys(rec string) map[string]bool {
	out := map[string]bool{}
	if k.values == nil {
		return out
	}
	for _, seg := range strings.Split(rec, "&") {
		if seg == "" || !k.values.IsNoisy(seg) {
			continue
		}
		raw, _, _ := strings.Cut(seg, "=")
		if key, err := url.QueryUnescape(raw); err == nil {
			out[key] = true
		}
	}
	return out
}

// formPart is one part of a multipart form.
type formPart struct {
	name, file string
	data       []byte
}

// multipartDiffs compares two multipart forms part by part, each read by its
// own boundary: the same parts in the same order, each field with the same
// value and each file with the same name and bytes.
func (k strictKnown) multipartDiffs(recCT, rec, liveCT string, live []byte) []strictField {
	rp, rok := multipartParts(recCT, []byte(rec))
	lp, lok := multipartParts(liveCT, live)
	if !rok || !lok || len(rp) != len(lp) {
		return []strictField{bodyChanged()}
	}
	var out []strictField
	for i := range rp {
		path := "body." + rp[i].name
		switch {
		case rp[i].name != lp[i].name || rp[i].file != lp[i].file:
			return append(out, bodyChanged())
		case bytes.Equal(rp[i].data, lp[i].data):
			continue
		case rp[i].file == "" && (k.n.body.Covers(rp[i].name, "", false) || k.learnedAt(path, "") || k.redacted(string(rp[i].data))):
			continue
		}
		out = append(out, shaped(path, "body."+shapeKey(rp[i].name), models.DiffKindValueChanged, "", ""))
	}
	return out
}

func multipartParts(ct string, body []byte) ([]formPart, bool) {
	_, params, err := mime.ParseMediaType(ct)
	if err != nil || params["boundary"] == "" {
		return nil, false
	}
	mr := multipart.NewReader(bytes.NewReader(body), params["boundary"])
	var out []formPart
	for {
		p, err := mr.NextPart()
		if err == io.EOF {
			return out, true
		}
		if err != nil {
			return nil, false
		}
		data, err := io.ReadAll(p)
		if err != nil {
			return nil, false
		}
		out = append(out, formPart{name: p.FormName(), file: p.FileName(), data: data})
	}
}

// decodeJSONDoc decodes one JSON document, its numbers as json.Number so a
// large integer keeps every digit.
func decodeJSONDoc(b []byte) (interface{}, bool) {
	if len(bytes.TrimSpace(b)) == 0 {
		return nil, false
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var v interface{}
	if err := dec.Decode(&v); err != nil {
		return nil, false
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, false
	}
	return v, true
}

// jsonDiffs walks a recorded and a live JSON value at exact (the learn step's
// dialect: body.items[0].id) and risk (the request matcher's, relative to the
// body: items[].id), adding each difference to out under risk's name, which
// is what keploy.yml's noise is written in, and under shape, risk's name with
// each key that may be a value written {}. Every object member, every array
// element by its position (an array in another order differs), and every
// scalar by value. learned: a container the mock learned holds this value, so
// only its type is compared.
func (k strictKnown) jsonDiffs(r, l interface{}, exact, risk, shape string, learned bool, out *[]strictField) {
	name := "body"
	if risk != "" {
		name = "body." + risk
		if k.n.body.Covers(risk, scalarText(r), isScalar(r)) && k.n.body.Covers(risk, scalarText(l), isScalar(l)) {
			return
		}
	}
	if isScalar(r) && k.redacted(scalarText(r)) {
		return
	}
	learned = learned || k.learnedAt(exact, name)
	if jsonKind(r) != jsonKind(l) {
		*out = append(*out, shaped(name, shape, models.DiffKindTypeChanged, jsonKind(r), jsonKind(l)))
		return
	}
	switch rv := r.(type) {
	case map[string]interface{}:
		lv := l.(map[string]interface{})
		keys := map[string]bool{}
		for key := range rv {
			keys[key] = true
		}
		for key := range lv {
			keys[key] = true
		}
		for _, key := range sortedNames(keys) {
			childRisk := key
			if risk != "" {
				childRisk = risk + "." + key
			}
			childShape := shape + "." + shapeKey(key)
			rc, inRec := rv[key]
			lc, inLive := lv[key]
			switch {
			case inRec && inLive:
				k.jsonDiffs(rc, lc, exact+"."+key, childRisk, childShape, learned, out)
			case k.n.body.Covers(childRisk, scalarText(rc), false) || (inRec && isScalar(rc) && k.redacted(scalarText(rc))):
			case !inLive:
				*out = append(*out, shaped("body."+childRisk, childShape, models.DiffKindMissingInLive, "", ""))
			default:
				*out = append(*out, shaped("body."+childRisk, childShape, models.DiffKindMissingInMock, "", ""))
			}
		}
	case []interface{}:
		lv := l.([]interface{})
		if len(rv) != len(lv) {
			*out = append(*out, shaped(name, shape, models.DiffKindValueChanged, strconv.Itoa(len(rv))+" elements", strconv.Itoa(len(lv))+" elements"))
			return
		}
		for i := range rv {
			k.jsonDiffs(rv[i], lv[i], exact+"["+strconv.Itoa(i)+"]", risk+"[]", shape+"[]", learned, out)
		}
	default:
		if learned || sameScalar(r, l) {
			return
		}
		f := shaped(name, shape, models.DiffKindValueChanged, scalarText(r), scalarText(l))
		if strings.Contains(risk, "[]") {
			// Elements of an array share one name, so the values would not
			// say which element changed: the field alone.
			f.diff.Expected, f.diff.Actual = "", ""
		}
		*out = append(*out, f)
	}
}

func isScalar(v interface{}) bool {
	switch v.(type) {
	case map[string]interface{}, []interface{}:
		return false
	}
	return true
}

func jsonKind(v interface{}) string {
	switch v.(type) {
	case map[string]interface{}:
		return "object"
	case []interface{}:
		return "array"
	case string:
		return "string"
	case json.Number:
		return "number"
	case bool:
		return "boolean"
	case nil:
		return "null"
	}
	return "unknown"
}

func scalarText(v interface{}) string {
	switch t := v.(type) {
	case string:
		return t
	case json.Number:
		return t.String()
	case bool:
		return strconv.FormatBool(t)
	case nil:
		return "null"
	}
	return ""
}

// sameScalar compares two JSON scalars of one kind; numbers by their value
// (1.50 is 1.5), exactly.
func sameScalar(r, l interface{}) bool {
	rn, rok := r.(json.Number)
	ln, lok := l.(json.Number)
	if rok && lok {
		if rn == ln {
			return true
		}
		x, okX := new(big.Rat).SetString(rn.String())
		y, okY := new(big.Rat).SetString(ln.String())
		return okX && okY && x.Cmp(y) == 0
	}
	return r == l
}

func sortedNames(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedStringKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
