package http

import (
	"context"
	"maps"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"unicode/utf8"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mismatch"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mocknoise"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// servedAs is how matchCascade served a call with the proven switch off
// (matchServing): answer is "" for the call's own recording, or the answer
// that says the call was served the recording of another call
// (models.AnsweredAsAnother); fields, for models.AnswerClosestBody, are the
// body fields the call differs from that recording at
// (bodyFieldsDiffering).
type servedAs struct {
	answer string
	fields []string
}

// noteServedAs notes, in how when it is not nil, how matchCascade served
// the call: with the recording of another path when idVaried (its second
// pass, which lets id segments differ, found the recording), else of
// another body when closest (the fuzzy fallback served the closest of
// several recordings), else the call's own.
func noteServedAs(how *servedAs, idVaried, closest bool) {
	if how == nil {
		return
	}
	switch {
	case idVaried:
		how.answer = models.AnswerOtherID
	case closest:
		how.answer = models.AnswerClosestBody
	default:
		how.answer = ""
	}
}

// sameCallBodies are the candidates whose request body is the call's (body)
// outside the noise keploy.yml declares (userBodyNoise) and the noise the
// record learned on each (MockSpec.ReqBodyNoise), compared both ways and
// whole (mocknoise.SameCallDrift, DiffBothWays), whichever noise mode the
// replay runs in: a JSON body is the same document, so key order and
// spacing are no difference while a field only one side holds, or an array
// of other elements, is; a form holds the same keys and values; any other
// body is the same bytes. Each is a recording of the very call. Each
// recording's body is read once (ownBodies), the call's once per call.
//
// A JSON recording whose print differs from the call's (callPrints) is
// another call's without the two being compared: the prints leave out at
// least what the comparison does not compare, so only a recording whose
// print is the call's, or says nothing, is compared (sameCallChecks counts
// them). Each recording is printed once (ownRead.printOf), the call (live,
// the match's read of it) once per noise the recordings were printed
// under, and only a recording compared is read whole (ownRead.json).
func (h *HTTP) sameCallBodies(candidates []*models.Mock, live *mocknoise.JSONBody, userBodyNoise map[string][]string) []*models.Mock {
	prints := newCallPrints(live, userBodyNoise)
	var own []*models.Mock
	for _, m := range candidates {
		recorded, ok := h.ownBodies.read(m)
		if !ok || prints.another(m, recorded) {
			continue
		}
		sameCallChecks.Add(1)
		if len(mocknoise.SameCallDrift(httpNoiseAdapter{}, m, recorded.json(), true, live, userBodyNoise)) == 0 {
			own = append(own, m)
		}
	}
	return own
}

// bodyFieldsDiffering names, sorted, the request-body fields ("body.ts") at
// which live, the call's body, differs from x's, as sameCallBodies compares
// them, a field only the call holds among them: marking each as
// request-body noise makes x the call's own recording. None where the
// bodies have no fields to compare (neither JSON nor a form), or are not
// the same kind of JSON document.
func (h *HTTP) bodyFieldsDiffering(x *models.Mock, live *mocknoise.JSONBody, userBodyNoise map[string][]string) []string {
	recorded, ok := h.ownBodies.read(x)
	return bodyFieldsOf(mocknoise.SameCallDrift(httpNoiseAdapter{}, x, recorded.json(), ok, live, userBodyNoise))
}

// bodyFieldsOf names, sorted, the request-body fields of drift
// (SameCallDrift's), the whole body ("body") not among them.
func bodyFieldsOf(drift map[string][]string) []string {
	var out []string
	for p := range drift {
		if strings.HasPrefix(p, "body.") {
			out = append(out, p)
		}
	}
	slices.Sort(out)
	return out
}

// noteFieldsOnOneSide notes, in how, a call served x, the one recording of
// its method and path left to serve it (the body-keys pass's one match, the
// fuzzy fallback's one candidate), that noteServedAs noted as served its own
// recording, as answered with the closest body (models.AnswerClosestBody)
// where a field only one of the two bodies holds, the call's or x's
// (fieldsOnOneSide), is a field they differ at outside the noise keploy.yml
// declares and the record learned (SameCallDrift, as sameCallBodies compares
// them): the call sent an input x's call did not, or lacks one it sent, so x
// is another call's recording. Its fields are then every field the two
// differ at, as for the closest of several. Where only values differ, x is
// still served as the call's own, with no report. Nothing is noted where how
// is nil (a match that only diagnoses the call) or noted another answer.
func (h *HTTP) noteFieldsOnOneSide(how *servedAs, x *models.Mock, live *mocknoise.JSONBody, userBodyNoise map[string][]string) {
	if how == nil || how.answer != "" {
		return
	}
	recorded, ok := h.ownBodies.read(x)
	if !ok {
		return
	}
	oneSide := fieldsOnOneSide(x, recorded.json(), live)
	if len(oneSide) == 0 {
		return
	}
	drift := mocknoise.SameCallDrift(httpNoiseAdapter{}, x, recorded.json(), true, live, userBodyNoise)
	for p := range drift {
		if oneSide[p] != 0 {
			how.answer, how.fields = models.AnswerClosestBody, bodyFieldsOf(drift)
			return
		}
	}
}

// urlNoiseEntry is the keploy.yml url-noise key (test.globalNoise.global.url,
// a regex MatchURLPath masks a path with) that makes recorded, a recorded
// path, match each path that differs from it only at the segments differs
// names (idPlacesDiffering's url.path.<i>): anchored to the whole path,
// every other segment as it is, an all-digit segment as [0-9]+ and any other
// as [^/]+. It is written Go-quoted, which YAML reads as the same string.
func urlNoiseEntry(recorded string, differs []string) string {
	varied := map[string]bool{}
	for _, d := range differs {
		varied[strings.TrimPrefix(d, "url.path.")] = true
	}
	segs := strings.Split(strings.TrimPrefix(recorded, "/"), "/")
	for i, s := range segs {
		switch {
		case !varied[strconv.Itoa(i)]:
			segs[i] = regexp.QuoteMeta(s)
		case reSegAllDigits.MatchString(s):
			segs[i] = "[0-9]+"
		default:
			segs[i] = "[^/]+"
		}
	}
	return strconv.Quote("^/" + strings.Join(segs, "/") + "$")
}

// requestBodyNoiseEntries are the keploy.yml request-body noise entries
// (test.globalNoise.global.requestbody, root-relative keys) that excuse the
// body fields fields names ("body.ts"), each Go-quoted, which YAML reads as
// the same string, with no patterns: `"ts": []`.
func requestBodyNoiseEntries(fields []string) string {
	entries := make([]string, 0, len(fields))
	for _, f := range fields {
		entries = append(entries, strconv.Quote(strings.TrimPrefix(f, "body."))+": []")
	}
	return strings.Join(entries, ", ")
}

// idPlacesDiffering names where the call differs from x, the recording it
// was served, at the places SchemaMatch's second pass lets an id differ:
// each segment at which recordedPath and livePath, two paths of as many
// segments, differ, as the learn step names a path's places (url.path.<i>,
// the first segment after the leading "/" at 0), in path order; then each
// query parameter whose values differ once keploy.yml's url noise (noise)
// is masked in both, as QueryParamsMatch compares them (a repeated
// parameter's in any order), named as the learn step and the strict
// comparison name one (url.query.<name>), by name. keploy's own parameters
// (keploy*), which no pass compares, are not named.
func idPlacesDiffering(recordedPath, livePath string, recordedQuery map[string]string, liveQuery url.Values, noise []*regexp.Regexp) []string {
	rs := strings.Split(strings.TrimPrefix(recordedPath, "/"), "/")
	ls := strings.Split(strings.TrimPrefix(livePath, "/"), "/")
	var out []string
	for i := range rs {
		if i < len(ls) && rs[i] != ls[i] {
			out = append(out, "url.path."+strconv.Itoa(i))
		}
	}
	for _, name := range slices.Sorted(maps.Keys(recordedQuery)) {
		if strings.HasPrefix(strings.ToLower(name), "keploy") {
			continue
		}
		if rv, lv := queryMembersDiffering(recordedQuery[name], liveQuery[name], noise); len(rv)+len(lv) > 0 {
			out = append(out, "url.query."+name)
		}
	}
	return out
}

// queryMembersDiffering are the members of recorded, a recorded query
// parameter's value (", "-joined, as pkg.URLParams keeps a repeated one),
// and of live, the call's values of it, each masked with noise
// (maskAndSort), that the other side does not hold as often: none where
// the two hold the same values in any order, as QueryParamsMatch compares
// them. As there, values the recording keeps as the call's, joined, are
// the same before any member is read: a value that holds ", " (?tag=a%2C%20b)
// is kept as two members would be.
func queryMembersDiffering(recorded string, live []string, noise []*regexp.Regexp) (rec, lv []string) {
	if recorded == strings.Join(live, ", ") {
		return nil, nil
	}
	rv, av := maskAndSort(strings.Split(recorded, ", "), noise), maskAndSort(live, noise)
	left := map[string]int{}
	for _, v := range av {
		left[v]++
	}
	for _, v := range rv {
		if left[v] > 0 {
			left[v]--
			continue
		}
		rec = append(rec, v)
	}
	for _, v := range av {
		if left[v] > 0 {
			left[v]--
			lv = append(lv, v)
		}
	}
	return rec, lv
}

// queryNoiseEntries are the keploy.yml url-noise keys (test.globalNoise
// .global.url) that mask the values of the query parameters differs names
// (url.query.<name>), each Go-quoted, which YAML reads as the same string,
// once each, in the order differs names them: QueryParamsMatch masks each
// query value with every url-noise regex, so a key anchored to a whole
// value of the shape of the members that differ (queryValueEntry) masks
// them, on both sides, in each run, and no path (each begins with "/").
// ok is false when any differing query id cannot be confined safely without
// matching "/" or non-id values, in which case the next step names the
// record step alone.
func queryNoiseEntries(recordedQuery map[string]string, liveQuery url.Values, differs []string, noise []*regexp.Regexp) ([]string, bool) {
	var out []string
	for _, d := range differs {
		name, ok := strings.CutPrefix(d, "url.query.")
		if !ok {
			continue
		}
		rv, lv := queryMembersDiffering(recordedQuery[name], liveQuery[name], noise)
		members := slices.Concat(rv, lv)
		for _, m := range members {
			c := queryValueTypeClass(m)
			if c == "" {
				continue
			}
			pat := queryValueEntry(c, members)
			if pat == "" {
				return nil, false
			}
			if e := strconv.Quote(pat); !slices.Contains(out, e) {
				out = append(out, e)
			}
		}
	}
	return out, true
}

const (
	minTokenDigitCore = 6
	minTokenIDCore    = 8
	minTokenTotalLen  = 16
)

// queryValueEntry is the url-noise regex that matches a whole query value
// of class, a dynamic type-class queryValueTypeClass gives: the class's own
// shape (a digit run of minDynamicQueryDigits or more, a uuid, long hex),
// and for a long token, the members' common literal prefix and suffix
// (trimmed to separator boundaries and regexp-quoted) around the varying
// part's own id class and length, with the core regex itself requiring a
// digit and the class's minimum length. Returns "" when no entry can be
// confined without matching "/" (paths) or values the matcher never calls an
// id (plain words, letter-only cores, snake_case enums, short digit runs).
func queryValueEntry(class string, members []string) string {
	switch class {
	case "digits":
		return "^[0-9]{" + strconv.Itoa(minDynamicQueryDigits) + ",}$"
	case "uuid":
		return reSegUUID.String()
	case "hex":
		return reSegLongHex.String()
	case "token":
		return tokenQueryValueEntry(members)
	default:
		return ""
	}
}

func isASCIIAlnum(r rune) bool {
	return (r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
}

func isASCIIAlpha(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
}

func hasASCIIAlpha(s string) bool {
	for _, r := range s {
		if isASCIIAlpha(r) {
			return true
		}
	}
	return false
}

// tokenQueryValueEntry confines a "token" query id to its common literal
// prefix and suffix around a narrow id-shaped varying core (uuid, digits,
// hex, or a prefix-labeled alphanumeric id). Every emitted hex or alnum core
// pattern syntactically requires at least one ASCII digit and the "token"
// class's total length floor, so letter-only values (class "") never match.
// Any member containing "/" or lacking a narrow id-shaped core returns "",
// so the next step names the record step alone.
func tokenQueryValueEntry(members []string) string {
	var toks []string
	for _, m := range members {
		if queryValueTypeClass(m) != "token" {
			continue
		}
		if strings.ContainsRune(m, '/') {
			return ""
		}
		toks = append(toks, m)
	}
	if len(toks) < 2 {
		return ""
	}
	prefix, suffix := commonTokenAffixes(toks)
	cores := make([]string, len(toks))
	for i, t := range toks {
		cores[i] = t[len(prefix) : len(t)-len(suffix)]
		if cores[i] == "" {
			return ""
		}
	}
	corePat := tokenCorePattern(cores, prefix, suffix)
	if corePat == "" {
		return ""
	}
	return "^" + regexp.QuoteMeta(prefix) + corePat + regexp.QuoteMeta(suffix) + "$"
}

// commonTokenAffixes extracts the longest common prefix and non-overlapping
// suffix of toks, trimmed to non-alphanumeric separator boundaries so a
// coincidental shared character inside the random id body is not baked into
// the literal affix.
func commonTokenAffixes(toks []string) (prefix, suffix string) {
	rawPre := toks[0]
	for _, t := range toks[1:] {
		i := 0
		for i < len(rawPre) && i < len(t) {
			r1, w1 := utf8.DecodeRuneInString(rawPre[i:])
			r2, _ := utf8.DecodeRuneInString(t[i:])
			if r1 != r2 {
				break
			}
			i += w1
		}
		rawPre = rawPre[:i]
	}
	preEnd := 0
	for i := 0; i < len(rawPre); {
		r, w := utf8.DecodeRuneInString(rawPre[i:])
		if !isASCIIAlnum(r) {
			preEnd = i + w
		}
		i += w
	}
	prefix = rawPre[:preEnd]

	rawSuf := toks[0][len(prefix):]
	for _, t := range toks[1:] {
		rem := t[len(prefix):]
		for len(rawSuf) > 0 {
			if len(rawSuf) <= len(rem) && strings.HasSuffix(rem, rawSuf) {
				break
			}
			_, w := utf8.DecodeRuneInString(rawSuf)
			rawSuf = rawSuf[w:]
		}
	}
	for i := 0; i < len(rawSuf); {
		r, w := utf8.DecodeRuneInString(rawSuf[i:])
		if !isASCIIAlnum(r) {
			suffix = rawSuf[i:]
			break
		}
		i += w
	}
	return prefix, suffix
}

// digitCorePattern builds an RE2 pattern over [charClass] that:
//  1. matches every string of length >= minLen over [charClass] containing at
//     least one ASCII digit [0-9], and
//  2. matches only strings of length >= minMatch containing at least one ASCII
//     digit [0-9] (with 1 <= minMatch <= minLen).
//
// Each branch has the form C{a,}[0-9]C{b,} with a+1+b == minMatch, so every
// match has length >= minMatch and includes a digit; stepping a by
// (minLen - minMatch + 1) covers every digit position 0..minLen-1. Returns ""
// when minMatch < 1 or minMatch > minLen.
func digitCorePattern(charClass string, minLen, minMatch int) string {
	if minMatch < 1 || minMatch > minLen {
		return ""
	}
	c := "[" + charClass + "]"
	quant := func(n int) string {
		switch n {
		case 0:
			return "*"
		case 1:
			return "+"
		default:
			return "{" + strconv.Itoa(n) + ",}"
		}
	}
	step := minLen - minMatch + 1
	var branches []string
	for a := 0; ; a += step {
		if a > minMatch-1 {
			a = minMatch - 1
		}
		b := minMatch - 1 - a
		branches = append(branches, c+quant(a)+"[0-9]"+c+quant(b))
		if a == minMatch-1 {
			break
		}
	}
	if len(branches) == 1 {
		return branches[0]
	}
	return "(?:" + strings.Join(branches, "|") + ")"
}

// tokenCorePattern returns the regex fragment for the varying cores of a
// token query id, or "" when the cores do not share a narrow id shape.
func tokenCorePattern(cores []string, prefix, suffix string) string {
	allUUID := true
	for _, c := range cores {
		if !reSegUUID.MatchString(c) {
			allUUID = false
			break
		}
	}
	if allUUID {
		return strings.TrimSuffix(strings.TrimPrefix(reSegUUID.String(), "^"), "$")
	}

	minLen, maxLen := len(cores[0]), len(cores[0])
	allDigits := true
	allHex := true
	allAlnum := true
	everyHasDigit := true
	everyHasAlpha := true
	hasLower, hasUpper := false, false
	for _, c := range cores {
		minLen = min(minLen, len(c))
		maxLen = max(maxLen, len(c))
		cDigit, cAlpha := false, false
		for i := 0; i < len(c); i++ {
			b := c[i]
			switch {
			case b >= '0' && b <= '9':
				cDigit = true
			case b >= 'a' && b <= 'f':
				allDigits = false
				cAlpha = true
				hasLower = true
			case b >= 'A' && b <= 'F':
				allDigits = false
				cAlpha = true
				hasUpper = true
			case b >= 'g' && b <= 'z':
				allDigits = false
				allHex = false
				cAlpha = true
				hasLower = true
			case b >= 'G' && b <= 'Z':
				allDigits = false
				allHex = false
				cAlpha = true
				hasUpper = true
			default:
				allDigits = false
				allHex = false
				allAlnum = false
			}
		}
		if !cDigit {
			everyHasDigit = false
		}
		if !cAlpha {
			everyHasAlpha = false
		}
	}

	hasPrefixLabel := hasASCIIAlpha(prefix)
	affixHasAlpha := hasPrefixLabel || hasASCIIAlpha(suffix)
	affixLen := len(prefix) + len(suffix)
	minMatch := max(minTokenIDCore, minTokenTotalLen-affixLen)

	if allDigits {
		minReq := minDynamicQueryDigits
		if hasPrefixLabel {
			minReq = minTokenDigitCore
		}
		if minLen < minReq {
			return ""
		}
		if minLen == maxLen {
			return "[0-9]{" + strconv.Itoa(minLen) + "}"
		}
		return "[0-9]{" + strconv.Itoa(minLen) + ",}"
	}
	if allHex && everyHasDigit && affixHasAlpha {
		minReq := 16
		if hasPrefixLabel {
			minReq = minTokenIDCore
		}
		if minLen < minReq {
			return ""
		}
		switch {
		case hasLower && !hasUpper:
			return digitCorePattern("0-9a-f", minLen, minMatch)
		case hasUpper && !hasLower:
			return digitCorePattern("0-9A-F", minLen, minMatch)
		default:
			return digitCorePattern("0-9a-fA-F", minLen, minMatch)
		}
	}
	if allAlnum && hasPrefixLabel && everyHasDigit && everyHasAlpha && minLen >= minTokenIDCore {
		switch {
		case hasLower && !hasUpper:
			return digitCorePattern("0-9a-z", minLen, minMatch)
		case hasUpper && !hasLower:
			return digitCorePattern("0-9A-Z", minLen, minMatch)
		default:
			return digitCorePattern("0-9A-Za-z", minLen, minMatch)
		}
	}
	return ""
}

// reportServedAnother reports, in-band (models.ReportAnswer), a call
// matchCascade served with the recording x of another call (how: its
// answer, a models.AnsweredAsAnother answer, and for a body the fields it
// differs at): ClosestMock names x, Diff says which call x recorded, Differs
// where the two differ, and NextSteps the two ways to give the call its own
// recording: record it, a new input; or, where what differs changes on every
// run, the keploy.yml noise entries that make x its own (urlNoiseEntry for
// the path, queryNoiseEntries for the query, both under urlNoise, keploy.yml's
// url noise the call was matched with; requestBodyNoiseEntries). The call is served either way; the report is
// what keeps keploy mock replay from counting it as the call's own answer.
// A how with no such answer (a call served its own recording, or one no
// pass served) reports nothing.
// The call and x's call are named as the miss's report names them: the
// segments x redacted stay redacted.
//
// The report carries the round its call was made in (call, where the
// request's first read of the round found it: integrations.StampMark), as a
// miss's does, so the proxy places it by that mark. With the switch off,
// the one mode a call is served so, the decode marks no call (the zero
// CallRound), its misses included: the report is placed as they are, by
// when it reaches the proxy.
func reportServedAnother(ctx context.Context, input *req, x *models.Mock, how servedAs, call integrations.CallRound, urlNoise []string) {
	answer := how.answer
	if !models.AnsweredAsAnother(answer) || x == nil || x.Spec.HTTPReq == nil {
		return
	}
	path, dest := "/", input.header.Get("Host")
	if input.url != nil {
		if escaped := input.url.EscapedPath(); escaped != "" {
			path = escaped
		}
		if dest == "" {
			dest = input.url.Host
		}
	}
	places := placesOf(x)
	liveCall := places.summary(input.method, path)
	r := mismatch.NewReport(mismatch.ProtocolHTTP, liveCall).WithDestination(dest).Build()
	r.Operation = models.HTTPOperationLabel(liveRequest(nil, input))
	r.Answer, r.ClosestMock = answer, x.Name
	r.PathTag = models.URLPathTag(ctx, input.method, input.url)
	r.ClosestMockReq = renderedOf(x)
	recordedPath, _ := recordedURLPath(x.Spec.HTTPReq.URL)
	if recordedPath == "" {
		recordedPath = "/"
	}
	recordedCall := string(x.Spec.HTTPReq.Method) + " " + recordedPath
	switch answer {
	case models.AnswerOtherID:
		livePath, liveQuery := "", url.Values(nil)
		if input.url != nil {
			livePath, liveQuery = input.url.Path, input.url.Query()
		}
		// The patterns were compiled, and an invalid one logged, as the
		// passes matched the call: the cache gives them back.
		recordedQuery, noise := x.Spec.HTTPReq.URLParams, compileURLNoise(zap.NewNop(), urlNoise)
		r.Differs = idPlacesDiffering(recordedPath, livePath, recordedQuery, liveQuery, noise)
		r.Diff = liveCall + " was answered with the recording of " + recordedCall + ": no recording has the call's URL, which differs from that recording's only at values that look like ids (" + strings.Join(r.Differs, ", ") + ")"
		var entries []string
		if slices.ContainsFunc(r.Differs, func(d string) bool { return strings.HasPrefix(d, "url.path.") }) {
			entries = append(entries, urlNoiseEntry(recordedPath, r.Differs))
		}
		query, queryOK := queryNoiseEntries(recordedQuery, liveQuery, r.Differs, noise)
		if !queryOK {
			entries, query = nil, nil
		} else {
			entries = append(entries, query...)
		}
		r.NextSteps = "If the call is a new input, record it (keploy mock record) so it is answered with its own recording."
		if len(entries) > 0 {
			r.NextSteps += " If the app makes a new id there on every run, mark it as url noise in keploy.yml, under test.globalNoise.global.url: {" + strings.Join(entries, ": [], ") + ": []}; each run's call is then answered with this recording as its own."
		}
		if len(query) > 0 {
			r.NextSteps += " An entry for a query value masks every query value that matches its pattern, in every call."
		}
	case models.AnswerClosestBody:
		r.Differs = how.fields
		if len(r.Differs) == 0 {
			r.Differs = []string{"body"}
		}
		r.Diff = liveCall + " was answered with the recording of " + recordedCall + " whose request body is closest to the call's: no recording of the call's method and path has the call's body outside its noise"
		r.NextSteps = "If the call is a new input, record it (keploy mock record) so it is answered with its own recording."
		if len(how.fields) > 0 {
			r.NextSteps += " If the fields it differs at (" + strings.Join(how.fields, ", ") + ") change on every run, mark them as noise in keploy.yml, under test.globalNoise.global.requestbody: {" + requestBodyNoiseEntries(how.fields) + "}; the call is then answered with this recording as its own."
		}
	}
	integrations.StampMark(r, call)
	models.ReportAnswer(ctx, r)
}

// renderedOf is x's recorded request (x records one) as renderMockRequest
// renders it, for the report of a call served x: rendered once for each
// recorded request and kept (servedRenders) by what renderMockRequest reads
// of it (renderKey), so the calls served x are reported without rendering
// x again, and a recording whose request or redacted values are not what
// was rendered is rendered again.
func renderedOf(x *models.Mock) string {
	key := renderKey{places: placesKey(x.Spec.HTTPReq, x.Noise), method: x.Spec.HTTPReq.Method}
	if s, ok := servedRenders.get(key); ok {
		return s
	}
	rendersMade.Add(1)
	s := renderMockRequest(x.Spec.HTTPReq, x.Noise)
	servedRenders.put(key, s)
	return s
}

// renderKey is what renderMockRequest reads of a recording: its request's
// URL, query parameters, headers and body and the patterns of the values
// the obfuscator redacted (placesKey's digest of them), and its method.
type renderKey struct {
	places placesDigest
	method models.Method
}

// servedRenders are the recorded requests renderedOf rendered, by their
// renderKey.
var servedRenders keptRenders

// rendersMade counts the requests renderedOf rendered, so a row can count
// that a recording is rendered once however many calls it serves.
var rendersMade atomic.Int64

// maxKeptRenders bounds the rendered text keptRenders holds: past it, it
// starts again from none.
const maxKeptRenders = 8 << 20

// keptRenders are rendered requests by their renderKey, holding at most
// maxKeptRenders bytes of them. Safe for concurrent use.
type keptRenders struct {
	mu    sync.Mutex
	bytes int
	of    map[renderKey]string
}

func (k *keptRenders) get(key renderKey) (string, bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	s, ok := k.of[key]
	return s, ok
}

func (k *keptRenders) put(key renderKey, s string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.of == nil || k.bytes+len(s) > maxKeptRenders {
		k.of, k.bytes = map[renderKey]string{}, 0
	}
	k.bytes += len(s)
	k.of[key] = s
}
