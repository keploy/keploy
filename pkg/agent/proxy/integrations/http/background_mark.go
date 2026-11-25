package http

import (
	"context"
	"maps"
	"slices"
	"strconv"
	"strings"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/variables"
)

// A MISS THE APPLICATION'S BACKGROUND WORK MADE (models.MockMismatchReport.
// Background).
//
// A call no recording answered is still a miss, answered with keploy's 502
// miss response. When it is the same request as one of the set's background
// recordings (its mappings.yaml worker: and startup: mocks, of the set the
// call was marked in, as the store hands them:
// integrations.BackgroundRecordingsAt), it is one more run of the
// application's own background work, such as a health poll on a timer, made
// once more than a recording was left to answer. Its miss is marked
// Background: listed with the test's misses, it counts against no test
// (models.UnmatchedCall.CountsAgainstTest).
//
// The same request is read three ways, and no other:
//   - strictDiff: equal to the recording in method, destination, path,
//     query, headers, and body apart from learned noise, keploy.yml's noise,
//     static flaky headers, and learned/declared clock reads checked against
//     the call's clock (using learn.CallSpansOf when an intervening serve
//     narrowed ClockSpanOf);
//   - or equal to it so but at named text leaves where the recording holds
//     the recorded value of a variable the call's own test bound before the
//     call (variables.Table.Own), or the trailed ended test's own calls bound
//     (variables.Table.EndedOf at its own recordings), and the call holds
//     the value bound to it (bgBoundOnly);
//   - or equal to it so but at named text or number leaves where both hold a
//     bare epoch timestamp that tracks the set's background recordings' own
//     moments (learn.EpochTrack) and holds on one of the call's own
//     background spans (learn.EpochOffset.HoldsIn, learn.CallSpansOf).

// markBackground marks report Background when the call (input) is the same
// request as one of the HTTP background recordings of the set it was marked
// in (mark: integrations.BackgroundRecordingsAt), or matches an unrecorded
// read-only declared kind: background HTTP rule of that set
// (integrations.BackgroundHTTPRulesAt). heldDb is the call's held store (for
// round-scoped declared clock rules when !restaged), and mockDb is the
// underlying store that serves round-scoped background recordings, declared
// rules, and bound values.
func (h *HTTP) markBackground(ctx context.Context, report *models.MockMismatchReport, input *req, heldDb, mockDb integrations.MockMemDb, mark integrations.CallRound, restaged bool, headerNoise, bodyNoise map[string][]string, urlNoise []string) {
	if report == nil || input == nil {
		return
	}
	if ok, bound, ruleID := h.backgroundCall(ctx, input, heldDb, mockDb, mark, restaged, headerNoise, bodyNoise, urlNoise); ok {
		report.Background, report.BackgroundBound = true, bound
		if ruleID != "" {
			report.NextSteps = strings.TrimSpace(report.NextSteps + " It matched the declared kind: background rule " + ruleID + " with no covering recording in the set, so it is counted against no test.")
		}
	}
}

// backgroundCall reports whether input is the same request as one of the HTTP
// background recordings of the set it was marked in (mark), read any way the
// comment above says (or matches an unrecorded read-only declared kind:
// background HTTP rule, returning its rule ID), and, when it is so only by
// values its test bound, the places that differ by them; false where the store
// hands neither recordings nor declared rules.
func (h *HTTP) backgroundCall(ctx context.Context, input *req, heldDb, mockDb integrations.MockMemDb, mark integrations.CallRound, restaged bool, headerNoise, bodyNoise map[string][]string, urlNoise []string) (bool, []models.BackgroundBinding, string) {
	recs := integrations.BackgroundRecordingsAt(mockDb, mark, models.HTTP)
	bgRules := integrations.BackgroundHTTPRulesAt(mockDb, mark)
	if len(recs) == 0 && len(bgRules) == 0 {
		return false, nil, ""
	}
	live := liveDestination(ctx, input)
	if len(recs) > 0 {
		n := h.newStrictNoise(headerNoise, bodyNoise, urlNoise)
		// Declared clock rules belong to the call's round: do not apply a newly
		// restaged set's sources (whose mock names restart) to an earlier set's
		// recordings. Every recording returned by BackgroundRecordingsAt is a
		// background recording (worker:/startup:), whether the call landed inside
		// a test window or between tests.
		if !restaged {
			n = n.withSources(heldDb)
			n.background = func(*models.Mock) bool { return true }
		}
		spans, spansKnown := learn.CallSpansOf(ctx, input.arrived)
		var bgSpans []learn.Span
		if spansKnown {
			bgSpans = spans
		}

		firstDiffs := make([][]strictField, len(recs))
		anyCandidate := false
		for i, r := range recs {
			if !bgSameShape(input, live, r.Mock, n) {
				continue
			}
			diffs := strictDiff(ctx, input, live, r.Mock, n)
			if len(diffs) == 0 {
				return true, nil, ""
			}
			if bgSpans != nil && allClockDiffs(diffs) && len(strictDiffWith(ctx, input, live, r.Mock, n, nil, bgSpans)) == 0 {
				return true, nil, ""
			}
			firstDiffs[i] = diffs
			anyCandidate = true
		}

		if anyCandidate && input.url != nil {
			liveLeaves := bgLeavesOf(liveParts(ctx, input), n)
			cachedRecLeaves := make([]*bgLeaves, len(recs))
			leavesOf := func(i int) bgLeaves {
				if cachedRecLeaves[i] == nil {
					l := bgLeavesOf(recordedParts(recs[i].Mock), n)
					cachedRecLeaves[i] = &l
				}
				return *cachedRecLeaves[i]
			}

			if bv := integrations.BoundValuesAt(mockDb, mark); bv != nil {
				own := bgHeldBy(bv.Own(), "")
				for i, r := range recs {
					if len(firstDiffs[i]) == 0 {
						continue
					}
					held := own
					if tr := bv.TrailOf(r.Mock.Name); tr.Test != "" && tr.Test != mark.Test() {
						held = append(own[:len(own):len(own)], bgHeldBy(bgMadeAt(bv.EndedOf(tr.Test), tr.Mocks), tr.Test)...)
					}
					if len(held) == 0 {
						continue
					}
					if bound, ok := bgBoundOnly(ctx, r.Mock, input, live, n, firstDiffs[i], leavesOf(i), liveLeaves, held, bgSpans); ok {
						return true, bound, ""
					}
				}
			}

			if bgSpans != nil {
				tried := map[string]bool{}
				for i := range recs {
					if len(firstDiffs[i]) == 0 {
						continue
					}
					if bgEpochsTrack(ctx, i, recs, input, live, n, firstDiffs[i], leavesOf, liveLeaves, bgSpans, tried) {
						return true, nil, ""
					}
				}
			}
		}
	}
	if ruleID, ok := matchesDeclaredBackgroundHTTP(input, live, recs, bgRules); ok {
		return true, nil, ruleID
	}
	return false, nil, ""
}

// bgSameShape is a cheap O(1) pre-filter before strictDiff: m must be a valid
// HTTP recording with the same method, destination, and (when no url noise
// regex may cross "/") path segment count as input.
func bgSameShape(input *req, live destination, m *models.Mock, n strictNoise) bool {
	if m == nil || m.Spec.HTTPReq == nil {
		return false
	}
	if string(m.Spec.HTTPReq.Method) != input.method {
		return false
	}
	if !sameDestination(live, recordedDestination(m)) {
		return false
	}
	if len(n.url) == 0 && input.url != nil {
		recPath, _ := recordedStrictURL(m)
		if strings.Count(strings.TrimPrefix(recPath, "/"), "/") != strings.Count(strings.TrimPrefix(input.url.Path, "/"), "/") {
			return false
		}
	}
	return true
}

// allClockDiffs reports whether every difference in non-empty diffs is a
// refused learned/declared clock read.
func allClockDiffs(diffs []strictField) bool {
	for _, f := range diffs {
		if !f.clock {
			return false
		}
	}
	return true
}

// bgLeaves holds a request's extracted leaves together with every JSON path
// that traversed a key containing ".", "[" or "]" (unnamed). Tracking unnamed
// paths separately closes the map-order collision where {"a.b":...} and
// {"a":{"b":...}} share the exact path "body.a.b".
type bgLeaves struct {
	leaves  map[string]learn.Leaf
	unnamed map[string]bool
}

// bgLeavesOf extracts the request's leaves using the same path/query/header/
// body naming dialect that strictKnown checks, plus every unnamed JSON path.
func bgLeavesOf(p httpParts, n strictNoise) bgLeaves {
	s := learnNoise{url: n.url}.shape(p, "")
	var unnamed map[string]bool
	if s.Body != nil {
		unnamed = map[string]bool{}
		markUnnamedJSONPaths("body", false, s.Body, unnamed)
	}
	return bgLeaves{leaves: s.Leaves, unnamed: unnamed}
}

// markUnnamedJSONPaths records in out every JSON path that traverses an
// object key containing ".", "[" or "]". Because both {"a.b":...} and
// {"a":{"b":...}} map to "body.a.b", marking every such path deterministically
// prevents Go's random map iteration from letting a nested leaf hide a
// dotted-key collision.
func markUnnamedJSONPaths(path string, hasUnnamed bool, v interface{}, out map[string]bool) {
	switch t := v.(type) {
	case map[string]interface{}:
		if len(t) == 0 && hasUnnamed {
			out[path] = true
			return
		}
		for k, child := range t {
			markUnnamedJSONPaths(path+"."+k, hasUnnamed || strings.ContainsAny(k, ".[]"), child, out)
		}
	case []interface{}:
		if len(t) == 0 && hasUnnamed {
			out[path] = true
			return
		}
		for i, child := range t {
			markUnnamedJSONPaths(path+"["+strconv.Itoa(i)+"]", hasUnnamed, child, out)
		}
	default:
		if hasUnnamed {
			out[path] = true
		}
	}
}

type bgLeafKind uint8

const (
	// bgLeafTextOnly admits only named text (learn.String) leaves. Bound-value
	// matching uses this so numeric JSON fields (such as counters or batch
	// sizes) never match an unrelated numeric binding.
	bgLeafTextOnly bgLeafKind = iota
	// bgLeafTextOrNumber also admits bare JSON numbers (learn.Number) for
	// epoch tracking.
	bgLeafTextOrNumber
)

// bgLeafOK reports whether l is a single, named leaf permitted by kind.
func bgLeafOK(l learn.Leaf, kind bgLeafKind) bool {
	if l.Unnamed || strings.Contains(l.Value, "\x00") {
		return false
	}
	if l.Type == learn.String {
		return true
	}
	return kind == bgLeafTextOrNumber && l.Type == learn.Number
}

// leafAt returns the leaf at p from b when it exists, satisfies bgLeafOK,
// and does not collide with any unnamed JSON key path.
func (b bgLeaves) leafAt(p string, kind bgLeafKind) (learn.Leaf, bool) {
	if b.unnamed[p] {
		return learn.Leaf{}, false
	}
	l, ok := b.leaves[p]
	if !ok || !bgLeafOK(l, kind) {
		return learn.Leaf{}, false
	}
	return l, true
}

// bgRecordedHeaderValue returns the recorded value of lowerName from m's
// headers using sorted key order (matching strictKnown.headerDiffs), so
// duplicate-cased recorded headers resolve identically in both places.
func bgRecordedHeaderValue(m *models.Mock, lowerName string) (string, bool) {
	for _, name := range sortedStringKeys(m.Spec.HTTPReq.Header) {
		if strings.ToLower(name) == lowerName {
			return m.Spec.HTTPReq.Header[name], true
		}
	}
	return "", false
}

// bgRecordedLeafValue reads a leaf's text value from b at p; for a recorded
// header it resolves duplicate-cased header keys in sorted order matching
// headerDiffs while still requiring learnNoise.shape to have admitted p.
func bgRecordedLeafValue(m *models.Mock, b bgLeaves, p string, kind bgLeafKind) (string, bool) {
	if lowerName, isHdr := strings.CutPrefix(p, "header."); isHdr {
		l, ok := b.leaves[p]
		if !ok || l.Unnamed || l.Type != learn.String {
			return "", false
		}
		v, ok := bgRecordedHeaderValue(m, lowerName)
		if !ok || strings.Contains(v, "\x00") {
			return "", false
		}
		return v, true
	}
	l, ok := b.leafAt(p, kind)
	if !ok {
		return "", false
	}
	return l.Value, true
}

// differingHTTPPlaces maps strictDiff's unexcused differences between m and
// input onto exact named leaf places in recLeaves/liveLeaves. Bound-value
// matching passes bgLeafTextOnly (learn.String only); epoch tracking passes
// bgLeafTextOrNumber (also admitting bare JSON numbers). It returns ok=false
// if any non-clock difference is structural (method, destination,
// missing/extra field, type change, pattern-guarded refusal, array length,
// multi-valued query, raw body, unnamed/colliding JSON key, or disallowed
// leaf type).
func differingHTTPPlaces(m *models.Mock, input *req, diffs []strictField, recLeaves, liveLeaves bgLeaves, n strictNoise, kind bgLeafKind) ([]string, bool) {
	k := newStrictKnown(m, n, nil)
	placeSet := map[string]bool{}
	for _, f := range diffs {
		if f.clock {
			continue
		}
		if f.diff.Kind != models.DiffKindValueChanged || f.pattern != nil {
			return nil, false
		}
		p := f.diff.Path
		switch {
		case p == "path":
			recPath, _ := recordedStrictURL(m)
			rPath, lPath := k.urlPathOf(recPath), k.urlPathOf(input.url.Path)
			if len(rPath.segs) != len(lPath.segs) {
				return nil, false
			}
			read := k.pathReadings(rPath)
			for i := range rPath.segs {
				if rPath.segs[i] != lPath.segs[i] && !k.segmentMayDiffer(i, rPath, lPath, read) {
					segPlace := "url.path." + strconv.Itoa(i)
					if _, rok := recLeaves.leafAt(segPlace, bgLeafTextOnly); !rok {
						return nil, false
					}
					if _, lok := liveLeaves.leafAt(segPlace, bgLeafTextOnly); !lok {
						return nil, false
					}
					placeSet[segPlace] = true
				}
			}
		case strings.HasPrefix(p, "query."):
			qPlace := "url." + p
			rl, rok := recLeaves.leafAt(qPlace, bgLeafTextOnly)
			ll, lok := liveLeaves.leafAt(qPlace, bgLeafTextOnly)
			if !rok || !lok || rl.Value == ll.Value {
				return nil, false
			}
			placeSet[qPlace] = true
		case strings.HasPrefix(p, "header."):
			rv, rok := bgRecordedLeafValue(m, recLeaves, p, bgLeafTextOnly)
			ll, lok := liveLeaves.leafAt(p, bgLeafTextOnly)
			if !rok || !lok || rv == ll.Value {
				return nil, false
			}
			placeSet[p] = true
		case strings.HasPrefix(p, "body."):
			added := false
			for exact, rl := range recLeaves.leaves {
				if rl.Noise != p {
					continue
				}
				ll, lok := liveLeaves.leaves[exact]
				if !lok || rl.Type != ll.Type || rl.Value == ll.Value {
					continue
				}
				if k.learnedAt(exact, rl.Noise) || k.redacted(rl.Value) {
					continue
				}
				if _, ok := recLeaves.leafAt(exact, kind); !ok {
					return nil, false
				}
				if _, ok := liveLeaves.leafAt(exact, kind); !ok {
					return nil, false
				}
				placeSet[exact] = true
				added = true
			}
			if !added {
				return nil, false
			}
		default:
			// method, container, host, whole body, or non-leaf diff.
			return nil, false
		}
	}
	if len(placeSet) == 0 {
		return nil, false
	}
	return slices.Sorted(maps.Keys(placeSet)), true
}

// bgHeldBinding is a binding bgBoundOnly reads, with the test that bound it
// when that is not the call's own (test, BackgroundBinding.BoundTest): the
// ended test the recording trails.
type bgHeldBinding struct {
	variables.Binding
	test string
}

// bgHeldBy wraps each binding in bs with test ("" for the call's own test).
func bgHeldBy(bs []variables.Binding, test string) []bgHeldBinding {
	out := make([]bgHeldBinding, 0, len(bs))
	for _, b := range bs {
		out = append(out, bgHeldBinding{Binding: b, test: test})
	}
	return out
}

// bgMadeAt filters ended (a trailed test's own ended-scope bindings) to those
// bound at one of mocks (the recordings the trailed test's own list names).
func bgMadeAt(ended []variables.Binding, mocks []string) []variables.Binding {
	if len(ended) == 0 || len(mocks) == 0 {
		return nil
	}
	of := make(map[string]bool, len(mocks))
	for _, m := range mocks {
		of[m] = true
	}
	var out []variables.Binding
	for _, b := range ended {
		if of[b.Mock] {
			out = append(out, b)
		}
	}
	return out
}

// bgBoundOnly reports whether input is equal to m, as strictDiff reads it,
// but at named text leaves where m holds the recorded value of one of held
// and input holds that binding's live value; returning each such place and
// its binding.
func bgBoundOnly(ctx context.Context, m *models.Mock, input *req, live destination, n strictNoise, diffs []strictField, recLeaves, liveLeaves bgLeaves, held []bgHeldBinding, bgSpans []learn.Span) ([]models.BackgroundBinding, bool) {
	places, ok := differingHTTPPlaces(m, input, diffs, recLeaves, liveLeaves, n, bgLeafTextOnly)
	if !ok {
		return nil, false
	}
	but := make(map[string]bool, len(places))
	bound := make([]models.BackgroundBinding, 0, len(places))
	for _, p := range places {
		recorded, rok := bgRecordedLeafValue(m, recLeaves, p, bgLeafTextOnly)
		ll, lok := liveLeaves.leafAt(p, bgLeafTextOnly)
		if !rok || !lok {
			return nil, false
		}
		b, ok := bgBoundTo(held, recorded, ll.Value)
		if !ok {
			b, ok = bgWordsBoundTo(held, recorded, ll.Value)
		}
		if !ok {
			return nil, false
		}
		but[p] = true
		bound = append(bound, models.BackgroundBinding{
			Recording:  m.Name,
			Place:      p,
			Variable:   b.Name,
			BoundMock:  b.Mock,
			BoundPlace: b.Place,
			BoundTest:  b.test,
		})
	}
	if len(strictDiffWith(ctx, input, live, m, n, but, bgSpans)) != 0 {
		return nil, false
	}
	return bound, true
}

// bgBoundTo returns the first whole-value binding in held whose Recorded
// equals recorded and whose Value equals value. It returns false when none
// exists, or when any counted binding (whole or word) in held maps recorded
// to a different live value.
func bgBoundTo(held []bgHeldBinding, recorded, value string) (bgHeldBinding, bool) {
	var found bgHeldBinding
	ok := false
	for _, b := range held {
		if b.Recorded != recorded {
			continue
		}
		whole := bgWholeBinding(b)
		_, _, isWord := bgWordAt(b)
		if !whole && !isWord {
			continue
		}
		if b.Value != value {
			return bgHeldBinding{}, false
		}
		if whole && !ok {
			found, ok = b, true
		}
	}
	return found, ok
}

// bgWholeBinding reports whether b is a named, non-clock binding of a whole
// value (no word address and no word run).
func bgWholeBinding(b bgHeldBinding) bool {
	return b.Mock != "" && len(b.Clock) == 0 && b.WordRun == "" && !strings.Contains(b.Place, "#")
}

// bgWordAt parses a named, non-clock word-level binding address (<place>#<n>,
// with no decode step) into its whole place and word index.
func bgWordAt(b bgHeldBinding) (place string, n int, ok bool) {
	if b.Mock == "" || len(b.Clock) > 0 {
		return "", 0, false
	}
	a, err := variables.ParseAddress(b.Place)
	if err != nil || a.Word < 0 || len(a.Steps) > 0 {
		return "", 0, false
	}
	return a.Place.String(), a.Word, true
}

// bgWordOf returns the word index of a word-level binding parsed by bgWordAt.
func bgWordOf(b bgHeldBinding) int {
	_, n, _ := bgWordAt(b)
	return n
}

// bgWordsBoundTo matches recorded against value when one test bound a value
// word by word at one place of one recording (for example, a UUID's five
// words bound as <place>#0..#4) and the background call carries the value
// whole. Both texts must have identical word count and separators
// (bgWordsAlike), one place's bound words in order at their relative offsets
// must cover every differing word (bgHoldsPlace), and no counted binding in
// held may map any of those recorded words to another live value.
func bgWordsBoundTo(held []bgHeldBinding, recorded, value string) (bgHeldBinding, bool) {
	rec, live, ok := bgWordsAlike(recorded, value)
	if !ok {
		return bgHeldBinding{}, false
	}
	lives := map[string]map[string]bool{}
	type place struct {
		at    string
		first bgHeldBinding
		words map[int]bgHeldBinding
		torn  bool
	}
	var places []*place
	byKey := map[string]*place{}
	for _, b := range held {
		if bgWholeBinding(b) && b.Recorded == recorded {
			return bgHeldBinding{}, false
		}
		at, n, isWord := bgWordAt(b)
		if !bgWholeBinding(b) && !isWord {
			continue
		}
		if lives[b.Recorded] == nil {
			lives[b.Recorded] = map[string]bool{}
		}
		lives[b.Recorded][b.Value] = true
		if !isWord {
			continue
		}
		key := b.test + "\x00" + b.Mock + "\x00" + at
		p := byKey[key]
		if p == nil {
			p = &place{at: at, words: map[int]bgHeldBinding{}}
			byKey[key] = p
			places = append(places, p)
		}
		if w, seen := p.words[n]; seen {
			p.torn = p.torn || w.Recorded != b.Recorded
			continue
		}
		p.words[n] = b
		if len(p.words) == 1 || n < bgWordOf(p.first) {
			p.first = b
		}
	}
	for _, p := range places {
		if p.torn {
			continue
		}
		ns := slices.Sorted(maps.Keys(p.words))
		for s := 0; s+ns[len(ns)-1]-ns[0] < len(rec); s++ {
			if !bgHoldsPlace(rec, live, s, ns, p.words) {
				continue
			}
			for _, n := range ns {
				if len(lives[p.words[n].Recorded]) != 1 {
					return bgHeldBinding{}, false
				}
			}
			found := p.first
			found.Place = p.at
			return found, true
		}
	}
	return bgHeldBinding{}, false
}

// bgHoldsPlace reports whether placing words (at sorted indices ns) starting
// at offset s in rec/live matches every bound word and leaves no other word
// differing between rec and live.
func bgHoldsPlace(rec, live []string, s int, ns []int, words map[int]bgHeldBinding) bool {
	at := make(map[int]bool, len(ns))
	for _, n := range ns {
		i := s + n - ns[0]
		if rec[i] != words[n].Recorded || live[i] != words[n].Value {
			return false
		}
		at[i] = true
	}
	for i := range rec {
		if rec[i] != live[i] && !at[i] {
			return false
		}
	}
	return true
}

// bgWordsAlike splits recorded and value via variables.Words and reports
// whether they have the same number of words and identical text before,
// between, and after every word.
func bgWordsAlike(recorded, value string) (rec, live []string, ok bool) {
	rw, lw := variables.Words(recorded), variables.Words(value)
	if len(rw) != len(lw) {
		return nil, nil, false
	}
	rec, live = make([]string, len(rw)), make([]string, len(lw))
	r, l := 0, 0
	for i := range rw {
		if recorded[r:rw[i][0]] != value[l:lw[i][0]] {
			return nil, nil, false
		}
		rec[i], live[i] = recorded[rw[i][0]:rw[i][1]], value[lw[i][0]:lw[i][1]]
		r, l = rw[i][1], lw[i][1]
	}
	return rec, live, recorded[r:] == value[l:]
}

// bgEpochsTrack reports whether input is equal to the background recordings
// that are equal to it but at the leaves where it differs from recs[rIdx],
// and each such leaf is a bare epoch read of the application's clock:
// learn.EpochTrack learns it over those recordings and the call's value
// holds on one of spans.
func bgEpochsTrack(ctx context.Context, rIdx int, recs []models.BackgroundRecording, input *req, live destination, n strictNoise, diffs []strictField, leavesOf func(int) bgLeaves, liveLeaves bgLeaves, spans []learn.Span, tried map[string]bool) bool {
	places, ok := differingHTTPPlaces(recs[rIdx].Mock, input, diffs, leavesOf(rIdx), liveLeaves, n, bgLeafTextOrNumber)
	if !ok {
		return false
	}
	key := strings.Join(places, "\x00")
	if tried[key] {
		return false
	}
	but := make(map[string]bool, len(places))
	for _, p := range places {
		but[p] = true
	}
	if len(strictDiffWith(ctx, input, live, recs[rIdx].Mock, n, but, spans)) != 0 {
		return false
	}
	tried[key] = true
	groupIdx := make([]int, 0, len(recs))
	for j, q := range recs {
		if j == rIdx {
			groupIdx = append(groupIdx, j)
			continue
		}
		if !bgSameShape(input, live, q.Mock, n) {
			continue
		}
		if len(strictDiffWith(ctx, input, live, q.Mock, n, but, spans)) == 0 {
			groupIdx = append(groupIdx, j)
		}
	}
	for _, p := range places {
		lv := liveLeaves.leaves[p]
		reads := make([]learn.EpochRead, 0, len(groupIdx))
		for _, j := range groupIdx {
			qv, ok := bgRecordedLeafValue(recs[j].Mock, leavesOf(j), p, bgLeafTextOrNumber)
			if !ok {
				return false
			}
			reads = append(reads, learn.EpochRead{
				Value: qv,
				Span:  learn.Span{From: recs[j].From, To: recs[j].Mock.Spec.ReqTimestampMock},
			})
		}
		off, ok := learn.EpochTrack(reads)
		if !ok || !off.HoldsIn(lv.Value, spans) {
			return false
		}
	}
	return true
}

// isReadOnlyHTTPMethod reports whether method is a safe read-only HTTP method.
// Unrecorded HTTP writes (POST, PUT, PATCH, DELETE) are never excused by the
// declared-background fallback.
func isReadOnlyHTTPMethod(method string) bool {
	return method == "GET" || method == "HEAD" || method == "OPTIONS"
}

// httpPathHasLiteralSegment reports whether path is a '/'-rooted path
// containing at least one non-empty literal (non-placeholder, non-wildcard)
// segment. Root "/" and all-placeholder catch-alls (such as "/{}", "/{}/{}",
// "/{a}/{b}", or "/{*}") return false so catch-all rules cannot excuse
// arbitrary unrecorded HTTP calls.
func httpPathHasLiteralSegment(path string) bool {
	if !strings.HasPrefix(path, "/") || path == "/" {
		return false
	}
	for _, seg := range strings.Split(path[1:], "/") {
		if seg == "" || seg == "{}" || seg == "*" || (strings.HasPrefix(seg, "{") && strings.HasSuffix(seg, "}")) {
			continue
		}
		return true
	}
	return false
}

// matchesDeclaredBackgroundHTTP reports whether a missed HTTP call matches one
// of the staged set's read-only declared kind: background HTTP rules (carrying
// at least one literal path segment) AND zero HTTP recordings in recs already
// cover that rule. Unreadable recordings in recs fail closed (blocking the
// fallback). Serving remains strictly fail-closed either way.
func matchesDeclaredBackgroundHTTP(input *req, live destination, recs []models.BackgroundRecording, rules []models.BackgroundHTTPRule) (string, bool) {
	if len(rules) == 0 || input == nil || input.url == nil || live.host == "" {
		return "", false
	}
	for _, r := range rules {
		if !isReadOnlyHTTPMethod(r.Method) || r.Host == "" || !httpPathHasLiteralSegment(r.Path) {
			continue
		}
		if !httpRuleCovers(r, input.method, live.host, input.url.Path) {
			continue
		}
		if anyRecMatchesHTTPBgRule(recs, r) {
			continue
		}
		return r.ID, true
	}
	return "", false
}

// anyRecMatchesHTTPBgRule reports whether any HTTP background recording in
// recs already covers r. Any recording whose request, URL, or destination
// cannot be read is treated fail-closed as covering so strict per-recording
// checks are never bypassed.
func anyRecMatchesHTTPBgRule(recs []models.BackgroundRecording, r models.BackgroundHTTPRule) bool {
	for _, br := range recs {
		if br.Mock == nil || br.Mock.Spec.HTTPReq == nil {
			return true
		}
		u, err := models.ParseRequestURL(br.Mock.Spec.HTTPReq.URL)
		if err != nil {
			return true
		}
		dest, ok := br.Mock.RecordedDestination()
		if !ok {
			return true
		}
		if httpRuleCovers(r, string(br.Mock.Spec.HTTPReq.Method), dest, u.Path) {
			return true
		}
	}
	return false
}

// httpRuleCovers reports whether r matches an HTTP call of method to host at
// path (segment-by-segment, with "{}" matching exactly one non-empty segment).
// Host comparison uses strings.EqualFold because live Host headers are
// case-insensitive (unlike mockrules.HTTPTarget.Covers' exact string compare).
func httpRuleCovers(r models.BackgroundHTTPRule, method, host, path string) bool {
	if r.Method != method || !strings.EqualFold(r.Host, host) {
		return false
	}
	if r.Path == "/" || path == "/" {
		return r.Path == path
	}
	if !strings.HasPrefix(r.Path, "/") || !strings.HasPrefix(path, "/") {
		return false
	}
	want, got := strings.Split(r.Path[1:], "/"), strings.Split(path[1:], "/")
	if len(want) != len(got) {
		return false
	}
	for i, w := range want {
		if got[i] == "" || (w != "{}" && w != got[i]) {
			return false
		}
	}
	return true
}
