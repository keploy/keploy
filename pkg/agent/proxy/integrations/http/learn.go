package http

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/util"
	"go.keploy.io/server/v3/pkg/matcher"
	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/variables"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// The learn step for HTTP (design v14 §7.2; package matcher/learn).
//
// An HTTP request's operation key is its method, its destination and the
// number of its path segments. The destination is the Host the client sent
// (lowercased, with its port), as the replay matcher compares it under the
// switch (S1): an HTTP mock does not record its scheme, and a port other
// than the scheme's is in the Host. Container keys are not compared yet:
// their first-use numbering can swap two containers of one image used at
// once, and learning across a swapped key would pair the wrong recording. The learnable leaves are the path
// segments (url.path.<i>), the query values (url.query.<name>, a repeated
// parameter's values in any order), the header values (header.<name>) and
// the body's scalars (body.<path>, or body.<key> of a form). What must be
// equal and is never learned: the media type, which headers are sent, and a
// body that is neither JSON nor a form. Host, Content-Type and Content-Length
// are not leaves: the first is in the key, the second is compared by its
// media type, and the third follows the body.

// credentialHeaders leave the static noise list in the learn step (design
// §7.2 step 8): a token minted earlier in the test becomes a variable, one
// that drifts at F becomes noise, and any other is compared.
var credentialHeaders = map[string]bool{
	"authorization":     true,
	"idempotency-key":   true,
	"x-idempotency-key": true,
	"x-csrf-token":      true,
	"x-xsrf-token":      true,
}

// learnNoise is the noise keploy.yml declares, as the learn step reads it.
type learnNoise struct {
	// headers are the header names never compared: keploy.yml's, and the
	// static list's (models.FlakyHeaders) but the credentials.
	headers map[string]bool
	// body is keploy.yml's request-body noise, read by matcher.NoiseLookup:
	// a path names whole fields, as the strict matcher reads it.
	body matcher.NoiseLookup
	// url is keploy.yml's url noise: what a pattern matches in a path or a
	// query value is not compared.
	url []*regexp.Regexp
}

func (h *HTTP) newLearnNoise(opts models.OutgoingOptions, bodyNoise map[string][]string, urlNoise []string) learnNoise {
	n := learnNoise{headers: map[string]bool{}, body: matcher.NewNoiseLookup(bodyNoise), url: compileURLNoise(h.Logger, urlNoise)}
	if opts.NoiseConfig != nil {
		for k := range opts.NoiseConfig["header"] {
			n.headers[strings.ToLower(k)] = true
		}
	}
	if !opts.DisableAutoHeaderNoise {
		for _, k := range models.FlakyHeaders {
			if !credentialHeaders[k] {
				n.headers[k] = true
			}
		}
		// The proven switch's own additions, as the strict matcher reads
		// them (models.ProvenFlakyHeaderNoise): the learn step runs under the
		// switch only.
		for _, k := range models.ProvenOnlyFlakyHeaders {
			n.headers[k] = true
		}
	}
	return n
}

// httpParts are the parts of an HTTP request the learn step reads.
type httpParts struct {
	method    string
	host      string // the Host, lowercased
	path      string
	query     map[string][]string
	header    map[string]string // by lowercased name, a repeated header's values joined as recorded
	mediaType string
	body      []byte
	// dest is where the request went, as the strict matcher reads it (S1).
	dest destination
}

func liveParts(ctx context.Context, in *req) httpParts {
	p := httpParts{method: in.method, path: in.url.Path, query: in.url.Query(), header: make(map[string]string, len(in.header)), body: in.body,
		dest: liveDestination(ctx, in)}
	for k, v := range in.header {
		p.header[strings.ToLower(k)] = strings.Join(v, ",")
	}
	p.host = strings.ToLower(p.header["host"])
	p.mediaType = mediaTypeOf(p.header["content-type"])
	return p
}

func recordedParts(m *models.Mock) httpParts {
	r := m.Spec.HTTPReq
	p := httpParts{method: string(r.Method), path: r.URL, header: make(map[string]string, len(r.Header)), body: []byte(r.Body),
		dest: recordedDestination(m)}
	u, err := url.Parse(r.URL)
	if err == nil {
		p.path = u.Path
	}
	// The recorded URL's own query first: URLParams joins a repeated
	// parameter's values with ", ", which cannot be told apart from one value
	// holding ", ".
	switch {
	case err == nil && u.RawQuery != "":
		p.query = u.Query()
	case len(r.URLParams) > 0:
		p.query = make(map[string][]string, len(r.URLParams))
		for k, v := range r.URLParams {
			p.query[k] = strings.Split(v, ", ")
		}
	}
	for k, v := range r.Header {
		p.header[strings.ToLower(k)] = v
	}
	p.host = strings.ToLower(p.header["host"])
	p.mediaType = mediaTypeOf(p.header["content-type"])
	return p
}

// mediaTypeOf is a Content-Type's media types, lowercased and sorted, without
// their parameters (a multipart boundary or a charset).
func mediaTypeOf(ct string) string {
	mts := parseMediaTypes(ct)
	for i := range mts {
		mts[i] = strings.ToLower(mts[i])
	}
	sort.Strings(mts)
	return strings.Join(mts, ",")
}

// sameDest is the destination in the operation key of the live request, and
// of every recorded request to the same destination.
const sameDest = "="

// destOf is the destination in a recorded request's operation key, for the
// live request live: the same one as the strict matcher's (sameDestination,
// S1), else the recording's own (its container's key, or its authority).
func destOf(live, rec httpParts) string {
	if sameDestination(live.dest, rec.dest) {
		return sameDest
	}
	if rec.dest.container {
		return "container " + rec.dest.key
	}
	return rec.dest.host
}

// segments is a path's segments, with what url noise matches masked.
func (n learnNoise) segments(path string) []string {
	return strings.Split(strings.TrimPrefix(maskURLNoise(path, n.url), "/"), "/")
}

func operationKey(method, dest string, segments int) string {
	return method + " " + dest + " /" + strconv.Itoa(segments)
}

// shape is p as the learn step compares it, its destination named d.
func (n learnNoise) shape(p httpParts, d string) learn.Shape {
	segs := n.segments(p.path)
	s := learn.Shape{Key: operationKey(p.method, d, len(segs)), Fixed: map[string]string{"media type": p.mediaType}, Leaves: map[string]learn.Leaf{}}
	add := func(name, value string, unnamed bool) {
		s.Leaves[name] = learn.Leaf{Path: name, Noise: name, Type: learn.String, Value: value, Unnamed: unnamed}
	}
	for i, seg := range segs {
		add("url.path."+strconv.Itoa(i), seg, false)
	}
	for k, vs := range p.query {
		if strings.HasPrefix(strings.ToLower(k), "keploy") {
			continue
		}
		masked := make([]string, len(vs))
		for i, v := range vs {
			masked[i] = maskURLNoise(v, n.url)
		}
		if len(masked) > 1 {
			// A repeated parameter's values come in any order, so no one of
			// them has a place a learned entry could name: compared whole.
			sort.Strings(masked)
			s.Fixed["url.query."+k] = strings.Join(masked, "\x00")
			continue
		}
		add("url.query."+k, masked[0], strings.ContainsAny(k, ".[]"))
	}
	for k, v := range p.header {
		switch {
		case k == "host" || k == "content-length" || k == "content-type" || strings.HasPrefix(k, "keploy") || n.headers[k]:
			continue
		}
		add("header."+k, v, false)
	}
	switch {
	case len(p.body) == 0:
		s.Fixed["body"] = ""
	case p.mediaType == "application/x-www-form-urlencoded":
		form, err := url.ParseQuery(string(p.body))
		if err != nil {
			s.Fixed["body"] = "text " + string(p.body)
			break
		}
		s.Fixed["body"] = "form"
		for k, vs := range form {
			vs = append([]string(nil), vs...)
			sort.Strings(vs)
			add("body."+k, strings.Join(vs, ", "), strings.ContainsAny(k, ".[]"))
		}
	case p.mediaType == "multipart/form-data":
		if !multipartLeaves(p, s.Leaves, s.Fixed) {
			s.Fixed["body"] = "text " + string(p.body)
			break
		}
		s.Fixed["body"] = "multipart"
	default:
		v, ok := learn.DecodeJSON(p.body)
		if !ok {
			s.Fixed["body"] = "text " + string(p.body)
			break
		}
		s.Fixed["body"] = "json"
		s.Body = v
		learn.JSONLeaves("body", v, s.Leaves)
	}
	return s
}

// multipartLeaves reads a multipart/form-data body by its own boundary: each
// field part is a leaf (body.<name>, Form: a variable's place form.<name>),
// each text file part a leaf too (File: its content compared whole, never
// learned as noise; a value keploy follows may stand inside it) with its file
// name fixed, and a binary file part a fixed part (its file name and bytes),
// so a boundary minted per request is no difference. It reports false for a
// body it cannot read.
func multipartLeaves(p httpParts, leaves map[string]learn.Leaf, fixed map[string]string) bool {
	_, params, err := mime.ParseMediaType(p.header["content-type"])
	if err != nil || params["boundary"] == "" {
		return false
	}
	mr := multipart.NewReader(bytes.NewReader(p.body), params["boundary"])
	seen := map[string]bool{}
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			return true
		}
		if err != nil {
			return false
		}
		data, err := io.ReadAll(part)
		if err != nil {
			return false
		}
		name := part.FormName()
		if seen[name] {
			// A repeated part name has no place of its own.
			return false
		}
		seen[name] = true
		path := "body." + name
		if part.FileName() != "" {
			if !textFile(data) {
				fixed[path] = "file " + part.FileName() + "\x00" + string(data)
				continue
			}
			fixed[path+"{file}"] = part.FileName()
			leaves[path] = learn.Leaf{Path: path, Noise: path, Type: learn.String, Value: string(data), Unnamed: strings.ContainsAny(name, ".[]"), Form: true, File: true}
			continue
		}
		leaves[path] = learn.Leaf{Path: path, Noise: path, Type: learn.String, Value: string(data), Unnamed: strings.ContainsAny(name, ".[]"), Form: true}
	}
}

// textFile reports whether a file part's bytes are text keploy can read
// words in: valid UTF-8 with no NUL byte. A binary file stays fixed, compared
// as its bytes.
func textFile(data []byte) bool {
	return utf8.Valid(data) && bytes.IndexByte(data, 0) < 0
}

// known is what a comparison with the recorded mock m excuses: keploy.yml's
// noise, the noise learned onto m, and m's obfuscated values (Mock.Noise).
func (n learnNoise) known(m *models.Mock) learn.Known {
	return httpKnown{n: n, learned: m.Spec.ReqBodyNoise, values: util.NewNoiseChecker(m.Noise)}
}

// knownAsRecorded is known without the noise learned onto m: the recording
// as the learn step first compared it, for a spent mock (learn.Candidate).
func (n learnNoise) knownAsRecorded(m *models.Mock) learn.Known {
	return httpKnown{n: n, values: util.NewNoiseChecker(m.Noise)}
}

type httpKnown struct {
	n       learnNoise
	learned map[string][]string
	values  *util.NoiseChecker
}

// Excused implements learn.Known. Learned noise keeps its type; keploy.yml's
// noise and obfuscated values excuse type and presence.
// What the learn step learned names a leaf by its exact path, an array
// position included; noise learned before it (by the request-body learner
// it replaces) names it in the request matcher's dialect.
func (k httpKnown) Excused(l learn.Leaf) (bool, bool) {
	if learnedCovers(k.learned, l.Path) || l.Noise != l.Path && hasKey(k.learned, l.Noise) {
		return true, true
	}
	scalar := l.Type != 0 && l.Type != learn.EmptyObj && l.Type != learn.EmptyArray
	switch {
	case strings.HasPrefix(l.Noise, "body."):
		if k.n.body.Covers(strings.TrimPrefix(l.Noise, "body."), l.Value, scalar) {
			return true, false
		}
	case strings.HasPrefix(l.Noise, "header."):
		if k.n.headers[strings.TrimPrefix(l.Noise, "header.")] {
			return true, false
		}
	}
	if scalar && k.values != nil && k.values.IsNoisy(l.Value) {
		return true, false
	}
	return false, false
}

func hasKey(m map[string][]string, k string) bool {
	_, ok := m[k]
	return ok
}

// learnedCovers reports whether learned noise names the leaf at path: the
// leaf itself, or a container holding it. Unlike keploy.yml's noise, a
// learned entry names exactly the field that drifted, never a field whose
// name it happens to contain, nor another element of its array.
func learnedCovers(learned map[string][]string, path string) bool {
	if _, ok := learned[path]; ok {
		return true
	}
	for p := range learned {
		if strings.HasPrefix(path, p+".") || strings.HasPrefix(path, p+"[") {
			return true
		}
	}
	return false
}

// respLeaves are the leaves of a mock's recorded answer an echo can
// echo into: its header values (but Content-Length) and its body's scalars,
// or its whole body when that is not JSON.
func respLeaves(r *models.HTTPResp) []learn.Leaf {
	if r == nil {
		return nil
	}
	var out []learn.Leaf
	names := make([]string, 0, len(r.Header))
	for k, v := range r.Header {
		lk := strings.ToLower(k)
		names = append(names, lk)
		if lk == "content-length" {
			continue
		}
		out = append(out, learn.Leaf{Path: "header." + lk, Noise: "header." + lk, Type: learn.String, Value: v})
	}
	// Its header names: an id an answer names a header by
	// ("x-amz-meta-<id>") is a place too.
	if len(names) > 0 {
		sort.Strings(names)
		out = append(out, learn.Leaf{Path: "header{names}", Type: learn.Keys, Value: strings.Join(names, "\n"), Unnamed: true})
	}
	if v, ok := learn.DecodeJSON([]byte(r.Body)); ok {
		body := map[string]learn.Leaf{}
		learn.JSONLeaves("body", v, body)
		for _, l := range body {
			out = append(out, l)
		}
		// Its object keys: an id an answer is keyed by is a place too.
		if k, ok := learn.KeysLeaf("body", v); ok {
			out = append(out, k)
		}
	} else if r.Body != "" {
		out = append(out, learn.Leaf{Path: "body", Noise: "body", Type: learn.String, Value: r.Body})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// classifiedAnswer is m's recorded answer as the classifier reads it for
// the drift of m's own call (learn.ClassifyIn's resp): its leaves
// (respLeaves), without its Date header where that is the answer's own clock
// stamp (learn.AnswerStamp: within the span the exchange was recorded in).
// The server read its clock as it answered the call, so no word of the
// call's request came back there: a word of a drifted value that the Date
// holds by coincidence is no echo.
func classifiedAnswer(m *models.Mock) []learn.Leaf {
	leaves := respLeaves(m.Spec.HTTPResp)
	out := leaves[:0:0]
	for _, l := range leaves {
		if l.Path == "header.date" && learn.AnswerStamp(l.Value, m.Spec.ReqTimestampMock, m.Spec.ResTimestampMock) {
			continue
		}
		out = append(out, l)
	}
	return out
}

// learnMiss is a request the learn step left unanswered.
type learnMiss struct {
	res     learn.Result
	closest *models.Mock
	pool    []*models.Mock
	// moved: the replay moved the round under the call before the learn
	// step could decide it (learnRound.movedFrom), and res.Why says where
	// the call's first pass read it and what the replay did since. It has
	// no reason: nothing of the request was compared.
	moved bool
	// steps, where set, is the miss's next step, in place of the mismatch
	// report's default, which speaks of a request that changed since it was
	// recorded.
	steps string
	// answered, where set, is a stateful walk's miss (answeredSequence):
	// the report says so.
	answered *answeredSequence
	// extra: the learn step found the request exactly one of the test's own
	// recordings that answered already (learn.Result.SpentExact), the values
	// the test gave included: one call more than the test recorded.
	extra bool
}

// learnRound is the round a call's first pass of the learn step read: the
// learn tracker's test (learn.Placed.Test, the number every Begin,
// BeginAwaiting and NewSet moves on) and whether a test was open
// (learn.Placed.Open), and, from a store that reads its round in one
// snapshot (snap: integrations.RoundReader), the store's view read with it:
// the staging (epoch), the view in force (the generation it was installed
// at, viewGen, and its test, view: "" for a pool view), and whether the
// store and the tracker agreed on where the round was (agrees: in the
// view's test, or between tests). read is false before the first pass.
//
// Every later pass is held to it (movedFrom): the call was made in that
// round, and is decided in it or in none, as the Mongo and Postgres learn
// steps' calls are. A pass read again once the replay moved the round under
// the call (the store installed another view, staged again, or the tracker
// moved to another test) would pair the call with another round's
// recordings, and learn what differs onto them as noise: a call of a test
// answered from the pool, or from the next test's own, as the scheduler fell.
type learnRound struct {
	read, open bool
	test       uint64
	snap       bool
	agrees     bool
	epoch      uint64
	viewGen    uint64
	view       string
	// noOne says why the round read is no one test's, "" where it is one's
	// (integrations.RoundSnapshot.NoOneRound): the store ended the test
	// the tracker still has open. A call is never decided in one, on any
	// pass (learnMatchIn).
	noOne string
	// ahead: the tracker had begun a test whose view the store had not
	// installed (integrations.RoundSnapshot.TrackerAhead). On the store's
	// pool view (pools) the call is the pool's, decided at once in the
	// round between tests. On the view of the test before (waits) it
	// decides nothing until the store opens the tracker's test (then in
	// that test's round: movedFrom's catch-up) or the test's window closes
	// (the moved miss), and waits for either (learnMatchIn).
	ahead bool
	// alone says why the round read is no one round to learn in, "" where
	// it is one (integrations.RoundSnapshot.ViewAlone): the store shows a
	// test's view and the tracker has no test open. The call is answered
	// only by a recording it matches exactly, and learns nothing, on any
	// pass (learnMatchIn).
	alone string
}

// pools reports whether c read the pool's call: the tracker ahead of the
// store's pool view (integrations.RoundSnapshot.PoolsCall), keploy test's
// begin of a test before its view's install, which waits for the call.
func (c learnRound) pools() bool { return c.ahead && c.view == "" }

// waits reports whether c read a call that waits for the store: the tracker
// ahead of a store still showing the view of the test before.
func (c learnRound) waits() bool { return c.ahead && c.view != "" }

// movedFrom says, of a later pass (now), what the replay moved since the
// call's first pass (c): "" when the call is still in the round it was made
// in. The store moves the round by staging again or by installing another
// view (a test's end installs the pool's, a test's begin its own, a pool
// view the next); the tracker by another test (Begin, BeginAwaiting, NewSet)
// or by ending the one the first pass found open (End moves no test
// number). A call that waited for its test case's request
// (learn.Tracker.AwaitRequest) whose test's window closed without it is
// such a call: keploy never read that request, so every changed value of the
// call may have come from it (learn.Tracker.OnUnreadClose).
//
// Two moves are not the round's: where the first pass found the store and
// the tracker apart (a test's begin that reached one of them first), the one
// that lagged catching up with the other, which did not move, so that the
// two agree on the one test the first pass read. The call was made after the
// side that led moved, in that test. A first pass that found the store's
// end of the tracker's open test read a round of no one test
// (learnRound.noOne): the call is decided in none, on that pass, so no
// catch-up follows it, and its verdict never rests on whether a boundary
// lands under a take. A first pass that found the tracker ahead of the
// store's pool view (it began a test whose view the store had not installed:
// learnRound.pools) read the pool's call, decided in the round between
// tests: any move of that round is a move, the store opening the tracker's
// test among them (keploy test's flow installs it only once the call is
// answered). One that found the tracker ahead of the view of the test before
// (learnRound.waits) waits for the store, and every pass still so on the
// tracker's same test is no move. The two catch-ups:
//   - the tracker catching up with the store's same test view, on a retake
//     (a pass read again because a take of the pass before was refused): it
//     began the test whose view the store shows, where it had no test open
//     at the first pass. The recordings the view offers, and whose own each
//     is, are the ones the call was made among
//     (TestALearnStepReadsAgainATakeALearnBoundaryRefused), and it is
//     answered only by one it matches exactly, learning nothing, as on the
//     first pass (learnRound.alone): only an exact take's refusal makes
//     the retake there. A first pass
//     with a test of the tracker's open is checked all the same: one of the
//     store's pool view is a move (it lands between tests, not in the test
//     the call was read in), as is any other (TestMovedFromCatchesUpOnlyWithTheFirstPassesTest).
//     A pass after a wait is no retake: a window that closed unread is a
//     round that moved, whatever the store shows
//     (TestMovedFromAPassAfterAWaitIsNoCatchUp). That the first pass found
//     the two apart is implied: on one view, a store that binds each test's
//     view to the tracker's test it opened under agrees with one test of
//     the tracker only;
//   - the store installing the view of the test the tracker has open, its
//     number and openness unchanged since the first pass, which found the
//     tracker ahead of the view of the test before (learnRound.waits), and
//     that waited for it. The call was made in that test, as the tracker
//     began it, while the store still served the view before
//     (TestALearnStepReadsTheRoundOnceMoreAfterAnInstall). The tracker's
//     number is its set's too (a new set moves it), so the test is of the
//     same set; a view of any other test is a move
//     (TestMovedFromCatchesUpOnlyWithTheFirstPassesTest), as is a test view
//     the tracker does not have open: a store that opened a test before the
//     tracker began it, then the next over it, moved on
//     (TestALearnStepDecidesNothingOfACallOnceTheStoreMovedItsViewAlone).
//     A store that installed its pool view while the tracker had no test
//     open moved on:
//     the tracker may never have begun the test the store showed
//     (TestALearnStepDecidesNothingOfACallOnceTheStoreEndedATestTheTrackerNeverBegan).
//     Read from a store that binds views, the two never agreed on the view
//     before (a test's view installed under the test the tracker has open is
//     bound to none); the first pass's disagreement is checked all the same,
//     for a store that does not bind them.
//
// An accepted catch-up is the call's round from then on (the caller holds
// the later passes to now). Any other move, of either side, is the replay
// moving the round under the call: a call of an agreed round (a test both
// had open, between tests on both) is decided in it or in none.
func (c learnRound) movedFrom(now learnRound, retake bool) (moved string, caughtUp bool) {
	if !c.read {
		return "", false
	}
	if now.snap != c.snap || c.snap && now.epoch != c.epoch {
		return movedRestaged, false
	}
	sameView := now.viewGen == c.viewGen && now.view == c.view
	sameTest := now.test == c.test && now.open == c.open
	switch {
	case sameView && sameTest:
		return "", false
	case c.waits() && now.ahead && sameTest:
		// The store is still behind the tracker's test, the call's own:
		// whatever view it installed meanwhile (its pool view after the
		// test before, another), the call still waits for that test's
		// (learnMatchIn), held to the first pass.
		return "", false
	case retake && sameView && now.agrees && now.view != "" && !c.open:
		// learnMatchIn waits only for an open test's request, so a
		// first pass with no test open is never followed by a pass
		// after a wait, and !c.open implies the retake there. It is
		// checked all the same: movedFrom's rule does not rest on its
		// caller (TestMovedFromAPassAfterAWaitIsNoCatchUp).
		return "", true
	case c.waits() && now.agrees && sameTest && now.view != "":
		return "", true
	case c.pools() && now.view != "":
		// The pool's call (integrations.RoundSnapshot.PoolsCall): the
		// store opening the tracker's test is no catch-up of it, but the
		// round between tests moving on.
		return "began a test", false
	case c.pools() && !sameView:
		return "served another view of its recordings between tests", false
	case c.pools():
		return "moved on to another test or test set", false
	case c.open || c.view != "" && !sameView:
		return "ended that test", false
	case now.open || now.view != "":
		return "began a test", false
	case !sameView:
		return "served another view of its recordings between tests", false
	}
	return "moved on to another test or test set", false
}

// movedRestaged is what movedFrom says of a pass read after the replay
// staged a test set again.
const movedRestaged = "staged a test set again"

// where is the round c was read in, as a miss says it: in the test open (by
// its view's name where the store and the tracker agreed on it, or as the
// test then running), while the store showed a test's view the tracker had
// no test open beside (before the tracker began it, or after it ended it),
// or in the round between tests (the start-up among them).
func (c learnRound) where() string {
	switch {
	case c.open && c.agrees && c.view != "":
		return "in " + c.view
	case c.pools():
		return "in the round between tests, as keploy's learn tracker began the next test"
	case c.open:
		return "in the test then running"
	case c.view != "":
		return "while keploy's store showed " + c.view + "'s view, with no test open on its learn tracker"
	}
	return "in the round between tests"
}

// scope is what a request's scope offers: the HTTP candidates learn.Pair
// takes (the test's own first, in recorded order, then the pool's; a pool
// mock is pairable only with pool), and every mock of any kind the scope
// shows, for the earlier places of the test's recording.
type scope struct {
	cands   []learn.Candidate
	mocks   []*models.Mock // the candidates' mocks
	own     []bool         // whether each candidate is one of the test's own
	http    []*models.Mock // every HTTP mock offered
	visible []*models.Mock // every mock offered, of any kind
}

// owner says whether a mock of the scope is one of the test's own, for the
// stateful cursor (cursorPick): a test's own sequence starts over with each
// test, the pool's does not.
func (sc scope) owner() func(*models.Mock) bool {
	own := make(map[*models.Mock]bool, len(sc.mocks))
	for i, m := range sc.mocks {
		if sc.own[i] {
			own[m] = true
		}
	}
	return func(m *models.Mock) bool { return own[m] }
}

// learnScope reads the scope of a request. A mock is the test's own only
// while a test is open (tracker): outside every test a request belongs to
// the pool, and in a view that scopes the pool to a test
// (integrations.TestOwnership), only the mocks it says are the test's.
func (h *HTTP) learnScope(mockDb integrations.MockMemDb, tracker *learn.Tracker, live httpParts, liveShape learn.Shape, n learnNoise, pool bool) (scope, error) {
	perTest, err := mockDb.GetPerTestMocksInWindow()
	if err != nil {
		utils.LogError(h.Logger, err, "failed to get per-test mocks")
		return scope{}, errors.New("error while matching the request with the mocks")
	}
	session, err := mockDb.GetSessionMocks()
	if err != nil {
		utils.LogError(h.Logger, err, "failed to get session mocks")
		return scope{}, errors.New("error while matching the request with the mocks")
	}
	r := scopeReads{perTest: perTest, session: session, open: tracker.Open(), answered: tracker.Answered, answeredMocks: tracker.AnsweredMocks}
	r.owner, _ = mockDb.(integrations.TestOwnership)
	if integrations.ExactServing(mockDb) {
		// Under exact serving the store alone says what the open test owns:
		// what its per-test tier shows, as on the bare manager. A worker's
		// view made before exact serving began passes straight through, and
		// its allowlist is the scope it was made for, not the open test's.
		r.owner = nil
	}
	r.windows, _ = mockDb.(integrations.TestWindows)
	r.inFlight, _ = mockDb.(integrations.InFlightOwner)
	r.pools, _ = mockDb.(integrations.PoolMembership)
	// Under the background rules a test learns from its own recordings
	// only (own exact, own learn, then background exact): with
	// a test open the background (the worker scope and the pool the view
	// shows in it) answers a call exactly or not at all, and no drift of the
	// test's is learned onto a background recording. The pool is learned
	// from between tests only.
	r.exactBackground = r.open && integrations.S12Background(mockDb)
	return r.scope(live, liveShape, n, pool), nil
}

// learnScopeIn is learnScope read from one snapshot of the store's round
// (integrations.RoundSnapshot): the view, whose each recording is, whether a
// test is open, and what the test's own answered, all of one instant. A test
// is open only where the store shows its view and the learn tracker has it
// open (RoundInTest): at a boundary between the two no recording is a test's
// own, and where the store shows a test's view the background answers
// exactly or not at all (RoundSnapshot.ExactBackground). What the open
// test's own answered is every account of it: what the tracker took for a
// call (Answer), what the view says answered (a pool view's ahead answers
// carried into the test among them), and the recordings the view holds (its
// test's own, in flight, or shared) that the round answered as a call in
// flight of a test that did not open next (voided): each answered a call of
// the round, and answers none again.
func (h *HTTP) learnScopeIn(snap integrations.RoundSnapshot, live httpParts, liveShape learn.Shape, n learnNoise, pool bool) scope {
	answered := map[string]*models.Mock{}
	var order []*models.Mock
	add := func(m *models.Mock) {
		if m != nil && answered[m.Name] == nil {
			answered[m.Name] = m
			order = append(order, m)
		}
	}
	for _, m := range snap.Learn.Answered {
		add(m)
	}
	for _, m := range snap.TakenInView() {
		add(m)
	}
	for _, m := range snap.Voided() {
		// Whoever's the view holds it as: the test's own, in flight, or
		// shared with the tests recorded beside it.
		if snap.Open && snap.Owner(m) != integrations.OwnerNone {
			add(m)
		}
	}
	r := scopeReads{
		perTest: snap.PerTest, session: snap.Session, open: snap.Open,
		windows: snap, inFlight: snap, pools: snap,
		exactBackground: snap.ExactBackground(),
		answered:        func(name string) bool { return answered[name] != nil },
		answeredMocks:   func() []*models.Mock { return order },
	}
	return r.scope(live, liveShape, n, pool)
}

// scopeReads is what a request's scope is built from (scopeReads.scope):
// the store's per-test and session reads, whether a test is open, how the
// store tells whose a recording is, and which of the test's own answered.
type scopeReads struct {
	perTest, session []*models.Mock
	open             bool
	owner            integrations.TestOwnership
	windows          integrations.TestWindows
	inFlight         integrations.InFlightOwner
	pools            integrations.PoolMembership
	exactBackground  bool
	// answered reports whether one of the test's own answered a request of
	// it; answeredMocks are those, as recorded, read after the scope's
	// mocks.
	answered      func(name string) bool
	answeredMocks func() []*models.Mock
}

// scope is the scope r reads.
func (r scopeReads) scope(live httpParts, liveShape learn.Shape, n learnNoise, pool bool) scope {
	var sc scope
	perTest, session, open := r.perTest, r.session, r.open
	sc.visible = append(append(make([]*models.Mock, 0, len(perTest)+len(session)), perTest...), session...)
	httpPerTest, httpSession := inRecordedOrder(FilterHTTPMocks(perTest)), inRecordedOrder(FilterHTTPMocks(session))
	sc.http = append(append(make([]*models.Mock, 0, len(httpPerTest)+len(httpSession)), httpPerTest...), httpSession...)

	inScope := map[string]bool{}
	var poolCands []learn.Candidate
	var poolMocks []*models.Mock
	for i, m := range sc.http {
		// A recording that declares variables is the variable pass's
		// (variables.go): it is served only where its variables hold, and
		// the learn step neither serves it nor learns onto it.
		if m == nil || m.Spec.HTTPReq == nil || hasVariables(m) {
			continue
		}
		rp := recordedParts(m)
		d := destOf(live, rp)
		// The key first: a body is decoded only for a candidate.
		if operationKey(rp.method, d, len(n.segments(rp.path))) != liveShape.Key {
			continue
		}
		own, poolsOwn := ownership(m, i < len(httpPerTest), r.owner, r.windows, r.inFlight, r.pools)
		// A protocol-phase mock (a handshake, a token fetch) is never a
		// test's own: the pool's, answering any number of the test's
		// requests (design §5.2), and never paired.
		own = own && open && !m.IsProtocolPhase()
		c := learn.Candidate{
			Shape:    n.shape(rp, d),
			Known:    n.known(m),
			Pairable: !m.IsProtocolPhase() && (own || pool && poolsOwn && !r.exactBackground),
		}
		inScope[m.Name] = true
		if own && r.answered(m.Name) {
			// Each of the test's own mocks answers one request of it
			// (design §5.2), whatever lifetime the store gives it: one
			// that answered is spent. It still competes, as the recording
			// it is: compared without the noise this round learned onto it
			// (the pool's copy carries it), so the next recording of the
			// same call is paired, not hidden behind it, and a call it
			// still matches exactly is one more than recorded.
			c.Known, c.Pairable, c.Spent = n.knownAsRecorded(m), false, true
		}
		if own {
			sc.cands, sc.mocks, sc.own = append(sc.cands, c), append(sc.mocks, m), append(sc.own, true)
		} else {
			poolCands, poolMocks = append(poolCands, c), append(poolMocks, m)
		}
	}
	// An own mock that answered and that the store let go since (a per-test
	// mock is deleted when consumed) is still spent, and still competes.
	if open {
		var gone []*models.Mock
		for _, m := range r.answeredMocks() {
			if !inScope[m.Name] && m.Spec.HTTPReq != nil {
				gone = append(gone, m)
			}
		}
		for _, m := range inRecordedOrder(gone) {
			rp := recordedParts(m)
			d := destOf(live, rp)
			if operationKey(rp.method, d, len(n.segments(rp.path))) != liveShape.Key {
				continue
			}
			sc.cands = append(sc.cands, learn.Candidate{Shape: n.shape(rp, d), Known: n.knownAsRecorded(m), Spent: true})
			sc.mocks, sc.own = append(sc.mocks, m), append(sc.own, true)
		}
	}
	sc.cands, sc.mocks = append(sc.cands, poolCands...), append(sc.mocks, poolMocks...)
	for range poolCands {
		sc.own = append(sc.own, false)
	}
	return sc
}

// ownership says whether a recorded mock is the open test's own and whether
// it is the pool's, from what the store can tell. A test's own mock is
// pairable while its test is open; the pool's only in a full record; another
// test's never (it still counts as the closest, which makes the request a
// miss). The store's tiers alone cannot tell them apart: a reusable HTTP mock
// of any test is in the session tier.
//
//   - A view that scopes the pool to a test (owner) says: the mocks its
//     mapping names are the test's, every other mock it shows is the pool's;
//     with no test open in it, none is paired.
//   - Otherwise a mock recorded outside every recorded test's window (the
//     start-up, between tests) is the pool's, but for one in flight when the
//     open test began (inFlight: sent in the gap before its window, answered
//     inside it), which the store serves as the test's own and is. The open
//     test's own are, of
//     the rest, its per-test tier, the session mocks its mapping names
//     (IsFiltered, set from the test's mapping when the replay stages it),
//     and the session mocks recorded inside its window. With no recorded
//     windows any other session mock is no one's to pair.
//   - A replay that sends no recorded windows (keploy mock replay) sends its
//     scope table instead (pools): a mock one of its pool slices names was
//     recorded while no test was open, and is the pool's as one recorded
//     outside every window is.
func ownership(m *models.Mock, perTestTier bool, owner integrations.TestOwnership, windows integrations.TestWindows, inFlight integrations.InFlightOwner, pools integrations.PoolMembership) (own, pool bool) {
	if owner != nil {
		// A view with no test open in it cannot say whose a mock is: none
		// is paired.
		owned, known := owner.OwnedByTest(m)
		return known && owned, known && !owned
	}
	at := m.Spec.ReqTimestampMock
	placed := false
	if windows != nil {
		// Recorded outside every recorded test's window (the start-up,
		// between tests): the pool's, whatever tier or mapping holds it (a
		// replay arms the start-up's names with every test's mapping).
		inside, known := windows.InRecordedTestWindow(at)
		placed = known
		if known && !inside {
			if inFlight != nil && inFlight.InFlightIntoOpenTest(m) {
				return true, false
			}
			return false, true
		}
	}
	if !placed && pools != nil {
		// No recorded windows: the scope table's pool slices name the mocks
		// recorded while no test was open.
		if inPool, known := pools.InPool(m); known && inPool {
			if inFlight != nil && inFlight.InFlightIntoOpenTest(m) {
				return true, false
			}
			return false, true
		}
	}
	if perTestTier || m.TestModeInfo.IsFiltered || windows != nil && windows.InTestWindow(at) {
		return true, false
	}
	return false, false
}

// exactGroup are the candidates that match live exactly, modulo what is
// known, and that may answer it (none spent): what a request recorded
// several times answers from, in turn (cursorPick), and nothing else. With
// poolOnly, only the pool's.
func exactGroup(live learn.Shape, sc scope, poolOnly bool) []*models.Mock {
	var out []*models.Mock
	for i, c := range sc.cands {
		if c.Shape.Key != live.Key || c.Spent || poolOnly && sc.own[i] {
			continue
		}
		if d := learn.Compare(live, c.Shape, c.Known); d.Structural == "" && len(d.Drift) == 0 && len(d.Reordered) == 0 {
			out = append(out, sc.mocks[i])
		}
	}
	return out
}

// earlierThan are the exchanges F served earlier in the test but those of
// the paired mock itself (a reusable mock served before is not an earlier
// place of its own values).
func earlierThan(earlier []learn.Earlier, paired *models.Mock) []learn.Earlier {
	out := make([]learn.Earlier, 0, len(earlier))
	for _, e := range earlier {
		if e.Mock != paired.Name {
			out = append(out, e)
		}
	}
	return out
}

// ofPairedTest are those of mocks (served before the running test began, or
// shown by the scope) that may be earlier places of the paired mock's
// recorded test: the pool's (recorded outside every recorded test's window:
// the start-up, between tests) and those recorded in paired's own recorded
// test. Another recorded test's mock is not: a late call of the previous
// test finds its earlier places in its own test, and a call of this test is
// not refused for a coincidence with another test's rows (in the same
// second, a clock read of each). That is only so of the open test's own
// mock (own) that the store places inside a recorded test's window. Any
// other paired mock (the pool's, recorded at the start-up, between tests or
// wherever a view says it is the pool's; a test's own in flight when it
// began, outside every window) may carry a value from any mock recorded
// before it, a test's too: every one is kept, as with no recorded windows
// (it errs to refusing; earlierRaw reads those recorded before it).
func ofPairedTest(mocks []*models.Mock, paired *models.Mock, windows integrations.TestWindows, own bool) []*models.Mock {
	if windows == nil || !own {
		return mocks
	}
	if inside, known := windows.InRecordedTestWindow(paired.Spec.ReqTimestampMock); !known || !inside {
		return mocks
	}
	out := mocks[:0:0]
	for _, b := range mocks {
		if b == nil {
			continue
		}
		at := b.Spec.ReqTimestampMock
		if inside, known := windows.InRecordedTestWindow(at); !known || !inside {
			out = append(out, b)
			continue
		}
		if same, known := windows.SameRecordedTestWindow(at, paired.Spec.ReqTimestampMock); !known || same {
			out = append(out, b)
		}
	}
	return out
}

// recordedBeforeOf visits, of a paired mock whose earlier places are every
// recording made before it (ofPairedTest: the pool's, a test's own outside
// every window, or one the store cannot place), every recording of the set
// made before it, wherever the store keeps it
// (integrations.RecordedBeforeReader), one at a time: such a value may have
// flowed from any of them, a test's own too, which the store keeps on disk
// until that test opens. nil for the open test's own mock (own) inside a
// recorded test's window, whose earlier places are its own test's, and for a
// store with no such reader.
func recordedBeforeOf(mockDb integrations.MockMemDb, paired *models.Mock, windows integrations.TestWindows, own bool) func(visit func(*models.Mock) bool) error {
	at := paired.Spec.ReqTimestampMock
	if own && windows != nil {
		if inside, known := windows.InRecordedTestWindow(at); known && inside {
			return nil
		}
	}
	rb, ok := mockDb.(integrations.RecordedBeforeReader)
	if !ok {
		return nil
	}
	return func(visit func(*models.Mock) bool) error { return rb.RangeRecordedBefore(at, visit) }
}

// recordedUnread is the store's failure to read the recordings before a pool
// mock (recordedBeforeOf): what they held cannot be told.
type recordedUnread struct{ err error }

func (e recordedUnread) Error() string {
	return "read the recordings before the call: " + e.err.Error()
}
func (e recordedUnread) Unwrap() error { return e.err }

// stagedOf is every mock the store stages, served or not and whatever its
// view shows (integrations.StagedReader); none for a store that keeps none.
func stagedOf(mockDb integrations.MockMemDb) []*models.Mock {
	if sr, ok := mockDb.(integrations.StagedReader); ok {
		return sr.StagedMocks()
	}
	return nil
}

// earlierRaw are the mocks of the test's recording that came before a
// request recorded at at, as Classify reads them when no leaf names their
// values: every mock any parser served earlier in the test that was
// recorded before it (served), every one F served before the test began
// that is an earlier place of the call's own test and was recorded before
// it (given: in the previous test, at the start-up, between tests;
// ofPairedTest), and every mock recorded before it
// that the scope shows or that the store stages, served or not (visible: a
// start-up call a kept-alive app never makes again in F is one). Recorded
// before it here counts one recorded at the call's own instant too, whether
// F served it or not (recordedByCall): read as one recorded a moment before
// the call, the fail-closed side. Each is read once. The paired mock is not one, and neither is an exchange the
// learn step served (earlier): its leaves and fixed parts, with what F saw
// at each, are read instead (earlierOf); where F served the paired mock
// before the call, givenEarlier reads it for the offset rule alone. A mock
// F served, in the test or before it, is read as F served it
// (learn.Raw.ServedIn): at the first and the last of its serves in the one
// order F served the set's mocks in,
// across its start-up, its tests and the gaps between them (order, as the
// read of the places saw it; 0, an order keploy cannot tell, which the
// offset rule fails closed on). Every other mock F served in the set
// (another test's, or one recorded after the call, served in the test or
// before it) is read by the offset rule and, for the words and the
// instants of its answer, by the word rule and the echo (givenEarlier): a
// value built on it, or copied word for word from what it answered, is
// refused naming it, never learned as noise. The paired mock has a
// recorded time here: the learn step learns nothing from one with none, as
// without it every mock F served would read as recorded after the call. dc keeps each mock's text for the test, read with its
// answer (DriftCache.ReadRaw). err names one whose answer the agent parked
// on disk and that cannot be read back, and why; no mock is then returned,
// since what it held cannot be told.
func earlierRaw(dc *learn.DriftCache, paired *models.Mock, served, given, visible []*models.Mock, order learn.Order, recorded func(visit func(*models.Mock) bool) error, earlier []learn.Earlier, prev *learn.PreviousCall) (out []learn.Raw, err error) {
	at := paired.Spec.ReqTimestampMock
	// A mock recorded at the call's own instant (a fixture's whole second:
	// record may have made it before the call) is an earlier place, as one
	// recorded a moment before the call is, whether F served it before the
	// call or not: the fail-closed side, which every learn rule reads the
	// equal instant by (learn.Raw). Read as what F gave alone
	// (givenEarlier), a value it holds whole would pass as noise.
	recordedByCall := func(m *models.Mock) bool {
		return !m.Spec.ReqTimestampMock.After(at)
	}
	leafed := make(map[string]bool, len(earlier)+1)
	leafed[paired.Name] = true
	for _, e := range earlier {
		leafed[e.Mock] = true
	}
	read := map[string]bool{}
	once := func(m *models.Mock) bool {
		id := rawID(m)
		if read[id] {
			return false
		}
		read[id] = true
		return true
	}
	add := func(m *models.Mock, servedByF bool, span [2]int) error {
		r, err := dc.ReadRaw(m, servedByF)
		if err != nil {
			return fmt.Errorf("read back the recorded answer of %q: %w", m.Name, err)
		}
		if servedByF {
			r = r.ServedIn(span[0], span[1])
		}
		out = append(out, r)
		return nil
	}
	for _, m := range served {
		prev.See(m)
	}
	for _, list := range [][]*models.Mock{served, given} {
		spans := order.Spans(list)
		for k, m := range list {
			// What F served, in the test or before it, is an earlier
			// place where it was recorded before the call or at its own
			// instant (recordedByCall). One recorded after it gave the
			// recorded application nothing before the call, wherever F
			// served it this run (at the start-up, in an earlier test, or
			// in this test before the call): the offset rule, the word
			// rule and the echo read it as what F gave this run alone
			// (givenEarlier), so a value copied from what F gave is
			// refused naming it, and a word the value was recorded with
			// that only it holds decides nothing.
			if m == nil || leafed[m.Name] || !recordedByCall(m) || !once(m) {
				continue
			}
			if err := add(m, true, spans[k]); err != nil {
				return nil, err
			}
		}
	}
	for _, m := range visible {
		if m != nil && !leafed[m.Name] && recordedByCall(m) && once(m) {
			if err := add(m, false, [2]int{}); err != nil {
				return nil, err
			}
		}
	}
	if recorded == nil {
		return out, nil
	}
	// Streamed, one at a time, and read without the cache (which keys by
	// the mock and would keep every one): only its text stays.
	var readErr error
	if err := recorded(func(m *models.Mock) bool {
		prev.See(m)
		if m == nil || leafed[m.Name] || !recordedByCall(m) || !once(m) {
			return true
		}
		r, err := learn.ReadRaw(m, false)
		if err != nil {
			readErr = fmt.Errorf("read back the recorded answer of %q: %w", m.Name, err)
			return false
		}
		out = append(out, r)
		return true
	}); err != nil {
		return nil, recordedUnread{err}
	}
	if readErr != nil {
		return nil, readErr
	}
	return out, nil
}

// rawID is a recording's identity as earlierRaw reads each once.
func rawID(m *models.Mock) string {
	return m.Name + "\x00" + string(m.Kind) + "\x00" + m.Spec.ReqTimestampMock.String()
}

// givenEarlier is the learn step's learn.GivenEarlier for the call paired
// with paired: every mock of set (those F served in the running set before
// the call, as the read of the places saw it) that earlierRaw reads no place
// of (read: those it read as F served them, the test's served and its given
// recorded before the call or at its own instant), each as F served it, by
// order: a mock F served
// in the test before the call but recorded after it is read here, as one F
// served before the test is, never as an earlier place. The paired mock
// is one where F served it before the call (in the set: a pool's reusable
// recording F gave the application at the start-up, or earlier in the
// test), which earlierRaw never reads: recorded at the call's own instant,
// it is a place F gave the application alone, never one the recording did.
// Each is read once for the set, its places alone kept, its text not held
// (learn.Order.GivenPlaces), a parked answer read back from disk only the
// first time. The exchanges the learn step served leaf by leaf (earlier:
// their payloads are read there) are not read again. An answer it cannot
// read back is an error naming it.
func givenEarlier(paired *models.Mock, set, served, given []*models.Mock, earlier []learn.Earlier, order learn.Order) learn.GivenEarlier {
	return func() ([]learn.GivenPlaces, error) {
		skip := make(map[string]bool, len(earlier))
		for _, e := range earlier {
			skip[e.Mock] = true
		}
		read := map[string]bool{}
		for _, list := range [][]*models.Mock{served, given} {
			for _, m := range list {
				if m != nil && m.Name != paired.Name && !m.Spec.ReqTimestampMock.After(paired.Spec.ReqTimestampMock) {
					read[rawID(m)] = true
				}
			}
		}
		out := make([]learn.GivenPlaces, 0, len(set))
		for _, m := range set {
			if m == nil || skip[m.Name] || read[rawID(m)] {
				continue
			}
			read[rawID(m)] = true
			g, err := order.GivenPlaces(m)
			if err != nil {
				return nil, fmt.Errorf("read back the recorded answer of %q: %w", m.Name, err)
			}
			out = append(out, g)
		}
		return out, nil
	}
}

// orderMiss is the order rule's reading, under the proven switch outside
// record's full round, of a request no mock answered: when the recorded call
// it most resembles holds an array it sent with the recorded elements in
// another order, the miss is unstable_order in any round (design §7.3), and
// names that call. nil otherwise: the matcher's own report stands. It learns
// nothing and serves nothing.
func (h *HTTP) orderMiss(ctx context.Context, input *req, mockDb integrations.MockMemDb, n learnNoise) *learnMiss {
	live := liveParts(ctx, input)
	liveShape := n.shape(live, sameDest)
	sc, err := h.learnScope(mockDb, nil, live, liveShape, n, true)
	if err != nil {
		return nil
	}
	res := learn.Pair(liveShape, sc.cands)
	if res.Outcome != learn.Miss || res.Reason != models.MissUnstableOrder || res.Index < 0 {
		return nil
	}
	return &learnMiss{res: res, closest: sc.mocks[res.Index], pool: sc.http}
}

// learnMatch decides a request of record's full round (opts.Learn, under the
// proven switch) with the learn step (design §7.2): the first mock that
// matches it exactly, modulo what is known, is served; otherwise the unique
// closest unconsumed mock it drifted from is served, and what drifted is
// learned onto that mock (noise or a variable, an echo included: learnPlan);
// otherwise it is a miss with its reason. It returns the mock to serve, or
// the miss.
func (h *HTTP) learnMatch(ctx context.Context, input *req, mockDb integrations.MockMemDb, n learnNoise, opts models.OutgoingOptions) (*models.Mock, *learnMiss, error) {
	return h.learnMatchIn(ctx, input, mockDb, n, opts, nil)
}

// learnMatchIn is learnMatch, telling call, when set, where the call's first
// read of the round found it made (integrations.CallRound): its first
// pass's. A miss's lookup of a recording made elsewhere is asked of that
// round (notOwnedRecording).
func (h *HTTP) learnMatchIn(ctx context.Context, input *req, mockDb integrations.MockMemDb, n learnNoise, opts models.OutgoingOptions, call *integrations.CallRound) (*models.Mock, *learnMiss, error) {
	tracker := learn.TrackerFrom(ctx)
	live := liveParts(ctx, input)
	liveShape := n.shape(live, sameDest)
	// unclaimed are the mocks this call's claim failed on, each with the
	// store's revision read before the pass that failed read its scope. A
	// claim fails when another call took the mock first, or when the store
	// moved on between the scope and the claim (a test's begin closed the
	// view the scope was read from): either changes the store, and the call
	// looks again. One the store still shows, with no change at all since
	// that pass read its scope, would fail the same way, and looking again
	// would find it, and fail, forever (a session-lifetime mock in the
	// per-test tier, whose claim updates the session tier): the call
	// misses instead. Read from one snapshot of the round, the pass is also
	// the round's staging, generation and take log's length: a claim the
	// round moved under (another view, the tracker's next test), refused
	// only once the round moved, is never stuck: the call reads the round
	// again, and misses unless it is still the round the first pass read,
	// or the one the side that lagged caught up with (learnRound.movedFrom).
	unclaimed := map[*models.Mock]learnPass{}
	stuck := func(m *models.Mock, now learnPass) bool {
		at, ok := unclaimed[m]
		return ok && at == now
	}
	refused := func(m *models.Mock, res learn.Result, sc scope) (*models.Mock, *learnMiss, error) {
		h.Logger.Warn("keploy's mock store would not hand out the recording the learn step chose for this call; learning nothing from it",
			zap.String("mock", m.Name))
		res.Outcome, res.Reason, res.Why = learn.Miss, "", "keploy's mock store would not hand out "+m.Name+", which it still shows, so keploy cannot answer from it"
		return nil, &learnMiss{res: res, closest: m, pool: sc.http}, nil
	}
	marked := false
	// made is the round the call's first pass read (learnRound), and
	// madePool the HTTP mocks that pass offered: every later pass is held
	// to that round. retake: the pass reads again because the take of the
	// pass before was refused.
	var made learnRound
	var madePool []*models.Mock
	// alone is why the call learns nothing on any pass, "" where it may
	// learn: its first pass read the store's test view with the tracker
	// not open (integrations.RoundSnapshot.ViewAlone). It holds past a
	// catch-up of the tracker to the store's test on a retake, which only
	// an exact take's refusal makes there: the call is answered as it would
	// have been had the tracker's begin not landed under its take, by a
	// recording it matches exactly, or not at all.
	alone := ""
	retake := false
	for {
		if ctx.Err() != nil {
			return nil, nil, ctx.Err()
		}
		first := call != nil && !marked
		read, err := h.readLearnPass(mockDb, tracker, live, liveShape, n, opts.LearnPool)
		if err != nil {
			return nil, nil, err
		}
		if first {
			*call, marked = read.call, true
		}
		moved, caughtUp := made.movedFrom(read.round, retake)
		if caughtUp && call != nil {
			// The call's round is the one caught up with from now on, and
			// so is its mark: the recordings that round's view holds as its
			// own are the call's (a test the store opened after the call's
			// first pass found the tracker ahead holds its own), so a miss
			// names none of them made elsewhere.
			*call = read.call
		}
		if moved != "" {
			// The replay moved the round the call was made in before the
			// learn step decided it (another view installed, a staging,
			// the tracker's next test or its end of the call's test,
			// while the call waited for its test case's request or while
			// a take of it was refused): what it would learn now is of a
			// round it was not made in, against another round's places
			// and recordings, and the next test's own, or the pool's, are
			// not its own. A wait for a request that never came ends so:
			// the window closed unread.
			res := learn.Result{Outcome: learn.Miss, Index: -1, Why: "keploy record's full round read this request " + made.where() + ", and the replay " + moved + " before keploy could answer it"}
			return nil, &learnMiss{res: res, pool: madePool, moved: true}, nil
		}
		if why := read.round.noOne; why != "" {
			// The store and the tracker are apart on which test is open
			// (integrations.RoundSnapshot.NoOneRound): the call is one
			// test's late call or the other's early one, and nothing tells
			// which. Read as either, a call that is exactly one test's own
			// recording was paired with another round's, and what it
			// differs in learned onto it as noise: a false green. It is of
			// no one round, and decided in none, on its first pass or any
			// later one (a pass after its wait for its test case's
			// request, the store still not showing that test), whatever
			// the replay does next: the verdict never rests on whether a
			// boundary lands before a take.
			res := learn.Result{Outcome: learn.Miss, Index: -1, Why: "keploy record's full round read this request " + why}
			return nil, &learnMiss{res: res, pool: read.sc.http, moved: true}, nil
		}
		if !made.read {
			alone = read.round.alone
		}
		if !made.read || caughtUp {
			// The first pass, or the round the side that lagged caught up
			// with (learnRound.movedFrom): every later pass is held to it.
			made, madePool = read.round, read.sc.http
		}
		if made.waits() && read.round.ahead {
			// The tracker began a test whose view the store has not
			// installed while the store still shows the view of the test
			// before (integrations.RoundSnapshot.TrackerAhead), which
			// keploy test's flow never leaves in force. The call is of the
			// tracker's test, and decides nothing until the store opens
			// it, waiting for the round to move through no clock: the
			// store opening the test is the catch-up
			// (learnRound.movedFrom), and the call is decided in that
			// test's round, its mark read again there (its own recordings
			// held); the window closing (End, the next Begin, NewSet) or a
			// staging is the moved miss. The call and the install give
			// one verdict whichever way the scheduler orders them, but for
			// a waiter the install woke that runs only once the store has
			// ended that test too: it reads the round moved on and gets
			// the moved miss, accepted as fail-closed
			// (MockManager.AwaitRoundMove's doc). A store that
			// cannot tell the move fails closed: the call is decided in no
			// round. The tracker ahead of the store's pool view is no
			// wait: that call is the pool's
			// (integrations.RoundSnapshot.PoolsCall), decided in the round
			// between tests, since keploy test installs the test's view
			// only once every call made before it is answered, this one
			// among them: a wait for the install would wait for itself.
			moved, err := integrations.AwaitRoundMove(ctx, mockDb, read.at.round)
			if err != nil {
				return nil, nil, err
			}
			if !moved {
				res := learn.Result{Outcome: learn.Miss, Index: -1, Why: "keploy record's full round read this request after keploy's learn tracker began a test its store had not opened, and the store cannot tell when it opens it, so the call is of no round keploy can decide it in"}
				return nil, &learnMiss{res: res, pool: madePool, moved: true}, nil
			}
			continue
		}
		retake = false
		sc, windows, claimDb := read.sc, read.windows, read.claimDb
		test, earlier, served, before, set, order := read.test, read.earlier, read.served, read.before, read.set, read.order
		if read.awaiting {
			// The call came after the test's window opened and before the
			// filter that opens it brought its test case's request: it
			// learns from nothing until the request came
			// (learn.Tracker.AwaitRequest), then reads the round again,
			// learned from as a call made after it, whichever way the two
			// fell. The wait ends too as the test's window closes without
			// the request (End, the next Begin, NewSet), though the next
			// test awaits its own: the next pass reads a round that moved
			// (learnRound.movedFrom), and decides nothing. The pool's call
			// read as a test opens (integrations.RoundSnapshot.PoolsCall)
			// waits so too: the agent gives the request before it settles
			// the calls made before (OpenTestCase), so the wait never holds
			// the test's install, and the call reads the request either way.
			if err := tracker.AwaitRequest(ctx); err != nil {
				return nil, nil, err
			}
			continue
		}
		// Under a claim, in a full record (which re-makes the pool), what
		// the claim key's calls made outside every test sent first (a
		// Playwright beforeAll seeding a name) is the key's: an earlier
		// place of the key's tests, followed as a variable of the key. A
		// call outside every test reads only the key's earlier calls. Whether a
		// test is open is the pass's own read (read.open); the key and its
		// places are read from the tracker here (Tracker.Keyed, Outside).
		keyScope := opts.LearnPool && tracker.Keyed()
		outsideCall := keyScope && !read.open
		var outsideMinted []learn.Minted
		if keyScope {
			var outsideEarlier []learn.Earlier
			outsideEarlier, outsideMinted = tracker.Outside()
			if outsideCall {
				earlier = outsideEarlier
			} else {
				earlier = append(append([]learn.Earlier(nil), earlier...), outsideEarlier...)
			}
		}

		// The values the test or its harness gave (givenBindings) tell the
		// recordings of one call apart by what the request carries: a page
		// that read the same call under two sessions the test switched
		// between is paired with the recording of its own session, and its
		// read one more than recorded under a session is one call more than
		// recorded (learn.PairGiven), never paired with another session's.
		res := learn.PairGiven(liveShape, sc.cands, givenLeaf(givenBindings(mockDb)))
		switch res.Outcome {
		case learn.Exact:
			m := sc.mocks[res.Index]
			rec := sc.cands[res.Index].Shape
			own := sc.own[res.Index]
			// The test's own recordings answer in recorded order, each one
			// request (design §5.2): Pair's is the first one not spent. A
			// pool request recorded several times answers with each
			// recorded answer in turn (cursorPick), among the pool's
			// recordings it matches exactly, as outside the learn step.
			picked, commit := m, func() {}
			if !own {
				var c func()
				var answered *answeredSequence
				picked, c, answered = h.cursorPick(m, exactGroup(liveShape, sc, true), sc.owner(), mockDb, !opts.DisableStatefulMocks)
				if answered != nil {
					res.Outcome, res.Reason, res.Why = learn.Miss, "", answered.why()
					return nil, &learnMiss{res: res, closest: m, pool: sc.http, answered: answered}, nil
				}
				if c != nil {
					commit = c
				}
			}
			if picked != m {
				rp := recordedParts(picked)
				rec = n.shape(rp, destOf(live, rp))
			}
			if stuck(picked, read.at) {
				return refused(picked, res, sc)
			}
			if own && !tracker.Answer(test, picked) {
				// Read again, held to the call's round: a test's own is
				// offered only where the store and the tracker agreed on
				// the test, and from an agreed round no move is a
				// catch-up (learnRound.movedFrom), so the next pass is
				// this round again or the moved miss.
				retake = true
				continue
			}
			// A recorded answer of the test's own that holds a value the
			// test made earlier is answered with this run's value, and what
			// that is learned (mintedAnswers). A value the test case gave
			// that the call carries unchanged is followed all the same
			// (givenUses): the next run gives another.
			var minted pairing
			bindings := givenBindings(mockDb)
			switch {
			case own:
				minted = h.mintedAnswers(tracker, test, picked, givenUses(picked, rec, bindings, false), reservedNames(bindings), append(tracker.Minted(test), outsideMinted...))
			case outsideCall:
				minted = h.mintedAnswers(tracker, test, picked, givenUses(picked, rec, bindings, true), nil, outsideMinted)
			}
			var (
				out     *models.Mock
				claimed bool
				why     string
			)
			if len(minted.vars) > 0 {
				out, claimed, why, err = h.claimLearned(ctx, picked, claimDb, minted, sc.owner())
			}
			learned := len(minted.vars) > 0 && why == ""
			if !learned {
				// The claim reads the scope's owners (ownedKey), as the
				// matcher's does: a stated recording no open test owns is a
				// reusable one, kept and served in turn (cursorPick), never
				// used up.
				out, claimed, err = h.claim(context.WithValue(ctx, ownedKey{}, sc.owner()), picked, claimDb, nil, nil)
			} else if claimed {
				h.logMintedAnswers(picked, minted.vars)
				h.logGivenUses(picked, minted.vars)
			}
			if err != nil || !claimed {
				if own {
					tracker.Unanswer(test, picked.Name)
				}
				if err != nil {
					return nil, nil, err
				}
				unclaimed[picked] = read.at
				retake = true
				continue
			}
			commit()
			if learned {
				// What the call carried of the values the test case gave, a
				// later call's answer may hold (mintedAnswers).
				for _, v := range minted.minted {
					if outsideCall {
						tracker.MintOutside(v)
						continue
					}
					tracker.Mint(test, v)
				}
			}
			if e, ok := n.servedExchange(picked, nil, rec, recordedParts(picked), liveShape, live, out, own); ok {
				tracker.Served(test, e)
				if outsideCall {
					tracker.ServedOutside(e)
				}
			}
			return out, nil, nil

		case learn.Paired:
			m, c, own := sc.mocks[res.Index], sc.cands[res.Index], sc.own[res.Index]
			if alone != "" {
				// The store showed a test's view the tracker had no test
				// open beside (integrations.RoundSnapshot.ViewAlone): the
				// call is the view's test's or the round between tests',
				// and nothing tells which. Paired with the pool's, a call
				// that is exactly one of the view's own recordings would
				// learn what it differs in onto them as noise. Decided at
				// once, never waiting for the tracker: what keploy test or
				// keploy mock would wait on may itself wait for the call.
				res := learn.Result{Outcome: learn.Miss, Index: -1, Why: "keploy record's full round read this request " + alone}
				return nil, &learnMiss{res: res, pool: sc.http, moved: true}, nil
			}
			// A call that is exactly a recording the scope does not offer
			// (the test before's own, read between tests after that test
			// ended; another test's) is that recording's call, made where
			// it was not recorded, never a drift of m: paired with m, what
			// it differs from m in was learned onto m as noise, a false
			// green. It learns nothing, and its miss goes on to the steps
			// that answer or name a recording made elsewhere (the
			// pairing, the miss's lookup), or names it here.
			var place *unreadPlace
			if x, err := exactlyElsewhere(input, mockDb, sc, live, liveShape, n, elsewhereCounts(own, read.windows)); errors.As(err, &place) {
				if _, warned := h.placeWarned.LoadOrStore(placeWarning{epoch: read.round.epoch, mock: place.mock}, true); !warned {
					h.Logger.Warn("a recording of the test set declares a variable at a place keploy cannot read, so the learn step cannot tell whether a call of its operation is exactly that recording: it learns nothing from such a call, which misses. Correct or remove the variable in the test set's mocks.yaml, or record again, then run again",
						zap.String("recording", place.mock), zap.String("variable", place.variable), zap.String("place", place.place))
				}
				res = learn.Result{Outcome: learn.Miss, Index: -1, Why: "keploy cannot read where the recording " + place.mock + " declares its variable " + place.variable + " (" + strconv.Quote(place.place) + "), so it cannot tell whether this request is exactly " + place.mock + " rather than a changed call of " + m.Name + ", and learns nothing from it"}
				return nil, &learnMiss{res: res, closest: m, pool: sc.http, steps: placeSteps}, nil
			} else if err != nil {
				h.Logger.Warn("keploy cannot read every recording of the test set, so its learn step cannot tell whether this call is exactly one of them: it learns nothing from the call, which misses. Fix what the error names, then run again",
					zap.String("mock", m.Name), zap.Error(err))
				res = learn.Result{Outcome: learn.Miss, Index: -1, Why: "keploy cannot read every recording of the test set (" + err.Error() + "), so it cannot tell whether this request is exactly one of them rather than a changed call of " + m.Name + ", and learns nothing from it"}
				return nil, &learnMiss{res: res, closest: m, pool: sc.http, steps: unreadSteps}, nil
			} else if x != nil {
				res = learn.Result{Outcome: learn.Miss, Index: -1, Why: elsewhereWhy(x, m, read.round.where(), read.windows)}
				return nil, &learnMiss{res: res, closest: x, pool: sc.http, steps: elsewhereSteps}, nil
			}
			if stuck(m, read.at) {
				return refused(m, res, sc)
			}
			if m.Spec.ReqTimestampMock.IsZero() {
				// A mock's earlier places are the recordings made before
				// it (a pool mock's, every one; the test's own, those of
				// its test, and what F served in it recorded before it):
				// of one with no recorded time, none can be told, so
				// where its value came from cannot be either.
				res.Outcome, res.Reason, res.Why = learn.Miss, models.MissRequestChanged, "keploy cannot tell which recordings of the test set came before "+m.Name+", which has no recorded time, so it cannot tell whether what changed came from one"
				return nil, &learnMiss{res: res, closest: m, pool: sc.http}, nil
			}
			earlier := earlierThan(earlier, m)
			// The paired mock with its answer: one the agent parked on disk
			// (diskmocks.go) holds none until it is read back, and an echo
			// in an answer read as empty would be learned as noise, never
			// compared again. One that cannot be read back is learned
			// nothing from.
			answered, err := withResponse(m)
			if err != nil {
				// keploy's own store failed, not the request: no reason
				// of the learn step's.
				h.Logger.Warn("the learn step cannot read back a recorded answer the agent parked on disk; learning nothing from this call",
					zap.String("mock", m.Name), zap.Error(err))
				res.Outcome, res.Reason, res.Why = learn.Miss, "", "keploy cannot read back the recorded answer of "+m.Name+", so it cannot tell whether the answer echoes what changed"
				return nil, &learnMiss{res: res, closest: m, pool: sc.http}, nil
			}
			staged := stagedOf(mockDb)
			prev := learn.NewPreviousCall(m)
			for _, s := range staged {
				prev.See(s)
			}
			given := ofPairedTest(before, m, windows, own)
			raw, err := earlierRaw(tracker.Drifts(), m, served, given, append(ofPairedTest(sc.visible, m, windows, own), ofPairedTest(staged, m, windows, own)...), order, recordedBeforeOf(mockDb, m, windows, own), earlier, prev)
			var unread recordedUnread
			if errors.As(err, &unread) {
				h.Logger.Warn("the learn step cannot read the recordings made before a pool call; learning nothing from this call",
					zap.String("mock", m.Name), zap.Error(err))
				res.Outcome, res.Reason, res.Why = learn.Miss, "", "keploy cannot read the recordings of the test set made before "+m.Name+", so it cannot tell whether what changed came from there"
				return nil, &learnMiss{res: res, closest: m, pool: sc.http}, nil
			}
			if err != nil {
				h.Logger.Warn("the learn step cannot read back an earlier recorded answer the agent parked on disk; learning nothing from this call",
					zap.String("mock", m.Name), zap.Error(err))
				res.Outcome, res.Reason, res.Why = learn.Miss, "", "keploy cannot read back the recorded answer of a recording made or served before this call, so it cannot tell whether what changed came from there"
				return nil, &learnMiss{res: res, closest: m, pool: sc.http}, nil
			}
			// The exchanges served leaf by leaf, each as the n-th mock F
			// served, as the payloads among raw are.
			earlier = learn.InServeOrder(earlier, append(append([]*models.Mock(nil), served...), before...), order)
			// A pool mock's call need not run on the clock F set for a
			// test (at the start-up and between tests none is set, and a
			// call made in a test was recorded in none): its clock reads
			// are read in its own window (learn.Tracker.CallWindowFrom),
			// as well as by the lead. Its previous recorded call is found
			// among every recording of the set, those on disk too, as the
			// earlier places streamed them. In keploy mock's client tests
			// every call's are, a test's own too: there the lead shows no
			// read of the application's clock (learn.DriftCache.ClientTests).
			var w *learn.Window
			if !own || tracker.ClientTests() {
				w = tracker.CallWindowFrom(prev.At(), m, input.arrived)
			}
			learned := learn.ClassifyGiven(tracker.Drifts(), res.Diff.Drift, liveShape, c.Shape, m.Spec.ReqTimestampMock, earlier, raw, classifiedAnswer(answered), w, givenEarlier(m, set, served, given, earlier, order))
			// The values the test case or its harness gave (template keys,
			// installed tokens: givenBindings), apart from given above, the
			// recordings F gave the application before the test.
			bindings := givenBindings(mockDb)
			plan, why := learnPlan(tracker, test, answered, c.Shape, own, outsideCall, learned, earlier, bindings)
			if why != "" {
				res.Outcome, res.Reason, res.Why = learn.Miss, models.MissRequestChanged, why
				return nil, &learnMiss{res: res, closest: m, pool: sc.http}, nil
			}
			switch {
			case own:
				plan = h.mintedAnswers(tracker, test, m, plan, reservedNames(bindings), append(tracker.Minted(test), outsideMinted...))
			case outsideCall:
				plan = h.mintedAnswers(tracker, test, m, plan, nil, outsideMinted)
			}
			if own && !tracker.Answer(test, m) {
				retake = true
				continue
			}
			out, claimed, why, err := h.claimLearned(ctx, m, claimDb, plan, sc.owner())
			if own && (err != nil || why != "" || !claimed) {
				tracker.Unanswer(test, m.Name)
			}
			if err != nil {
				return nil, nil, err
			}
			if why != "" {
				res.Outcome, res.Reason, res.Why = learn.Miss, models.MissRequestChanged, why
				return nil, &learnMiss{res: res, closest: m, pool: sc.http}, nil
			}
			if !claimed {
				// Read again as an exact claim is. movedFrom's retake
				// catch-up follows only a first pass where the store
				// showed a test's view the tracker had no test open
				// beside, where nothing is paired (alone): a paired
				// claim's retake reads its own round again, or the moved
				// miss.
				unclaimed[m] = read.at
				retake = true
				continue
			}
			for _, l := range learned {
				h.Logger.Info("learned a field that changed between record and replay",
					zap.String("mock", m.Name), zap.String("field", l.Recorded.Path), zap.String("as", string(l.Kind)))
			}
			h.logMintedAnswers(m, plan.vars)
			// What this call's request carried first in the test, a later
			// call's answer may hold (mintedAnswers).
			for _, v := range plan.minted {
				if outsideCall {
					tracker.MintOutside(v)
					continue
				}
				tracker.Mint(test, v)
			}
			if e, ok := n.servedExchange(m, answered, c.Shape, recordedParts(m), liveShape, live, out, own); ok {
				e = withinPlaces(e, plan.vars)
				tracker.Served(test, e)
				if outsideCall {
					tracker.ServedOutside(e)
				}
			}
			return out, nil, nil
		}
		miss := &learnMiss{res: res, pool: sc.http, extra: res.SpentExact}
		if res.Index >= 0 {
			miss.closest = sc.mocks[res.Index]
		}
		return nil, miss, nil
	}
}

// exactlyElsewhere is a recording of the staged test set that live is
// exactly, modulo what is known (as Pair's Exact), that the scope sc does not
// offer, and that counts reads: the test before's own, read between tests
// once that test ended, or another test's, which the view in force does not
// show. Such a call is read as that recording's, made outside where it was
// recorded, and is never paired with another recording. A recording that
// declares variables is compared with its variables' request places excused
// (variablePlaces): a call of it differs there on every run.
//
// The store visits its recordings of the call's request keys
// (integrations.StagedRecordingReader), those it keeps on disk too; a store
// with no such reader, its staged mocks (integrations.StagedReader). err is
// why the store could not read them all, or an *unreadPlace: a variable place
// keploy cannot read, of a recording of the call's operation. A limit: an agent that could not create its disk store keeps its
// mocks resident (agent.go), and a test's own recordings are then staged
// only as that test opens, so a call exactly a later test's is not seen.
func exactlyElsewhere(input *req, mockDb integrations.MockMemDb, sc scope, live httpParts, liveShape learn.Shape, n learnNoise, counts func(*models.Mock) bool) (*models.Mock, error) {
	offered := make(map[string]bool, len(sc.http)+len(sc.mocks))
	for _, list := range [][]*models.Mock{sc.http, sc.mocks} {
		for _, m := range list {
			if m != nil {
				offered[m.Name] = true
			}
		}
	}
	var found *models.Mock
	var placeErr error
	try := func(m *models.Mock) bool {
		if m == nil || m.Kind != models.HTTP || m.Spec.HTTPReq == nil || offered[m.Name] || !counts(m) {
			return true
		}
		rp := recordedParts(m)
		d := destOf(live, rp)
		if operationKey(rp.method, d, len(n.segments(rp.path))) != liveShape.Key {
			return true
		}
		known := n.known(m)
		if hasVariables(m) {
			at, err := variablePlaces(m)
			if err != nil {
				placeErr = err
				return false
			}
			known = excusedAt{Known: known, at: at}
		}
		if diff := learn.Compare(liveShape, n.shape(rp, d), known); diff.Structural == "" && len(diff.Drift) == 0 && len(diff.Reordered) == 0 {
			found = m
			return false
		}
		return true
	}
	var err error
	if r, ok := mockDb.(integrations.StagedRecordingReader); ok {
		path := ""
		if input.url != nil {
			path = input.url.Path
		}
		target := strings.Join(input.header.Values(amzTargetHeader), ",")
		loose := len(n.url) > 0 || n.headers[strings.ToLower(amzTargetHeader)]
		err = r.RangeStagedRecordings(models.HTTP, RequestKeysOf(input.method, path, target, loose), try)
	} else {
		for _, m := range stagedOf(mockDb) {
			if !try(m) {
				break
			}
		}
	}
	if placeErr != nil {
		return nil, placeErr
	}
	return found, err
}

// elsewhereCounts is which recordings exactlyElsewhere reads for a call the
// learn step would pair with a recording, the open test's own or not. Paired
// with the pool's (between tests, or in a test), every one. Paired with the
// open test's own, those recorded before the test's recorded window began
// (an earlier test's, the test before's late call): keploy test sends a
// test's request only once the test before's view ended, so a call in a test
// that is exactly a later test's recording is as likely a value that drifted
// onto it (an off-by-one counter) as an early call, and keeps its pairing
// with the test's own. Where the window is not known (no recorded windows, a
// store that does not tell), every one: it fails closed. This holds in
// recorded order only: keploy replay's mock-consistency retry cycles
// (RetryPassing) run test-1 again after test-N, where a late call of test-N
// read in test-1 would keep its pairing with test-1's own, so the learn step
// ends for those cycles (integrations.LearnEnded) and decides none of their
// requests. So it does for the deferred streaming pass of a set whose
// streaming test case was recorded before a test case that ran ahead of it.
func elsewhereCounts(own bool, windows integrations.TestWindows) func(*models.Mock) bool {
	every := func(*models.Mock) bool { return true }
	o, ok := windows.(integrations.RecordedTests)
	if !own || !ok {
		return every
	}
	return func(x *models.Mock) bool {
		before, known := o.BeforeTestWindow(x.Spec.ReqTimestampMock)
		return before || !known
	}
}

// variablePlaces is the leaf paths (learn.LeafPath) of m's variables'
// request places, each bind and use (models.MockVariable): where a call of m
// holds a value the application makes anew each run. err names a place keploy
// cannot read.
func variablePlaces(m *models.Mock) (map[string]bool, error) {
	names := make([]string, 0, len(m.Spec.Variables))
	for name := range m.Spec.Variables {
		names = append(names, name)
	}
	sort.Strings(names)
	at := map[string]bool{}
	for _, name := range names {
		v := m.Spec.Variables[name]
		places := append(make([]string, 0, 1+len(v.Use)), v.Use...)
		if v.Bind != "" {
			places = append(places, v.Bind)
		}
		for _, p := range places {
			path, ok := learn.LeafPath(p)
			if !ok {
				return nil, &unreadPlace{mock: m.Name, variable: name, place: p}
			}
			at[path] = true
		}
	}
	return at, nil
}

// unreadPlace is a variable's request place keploy cannot read
// (variablePlaces): the recording, the variable, the place as written.
type unreadPlace struct{ mock, variable, place string }

func (e *unreadPlace) Error() string {
	return fmt.Sprintf("the recording %s declares its variable %s at %q, a place keploy cannot read", e.mock, e.variable, e.place)
}

// placeWarning is a recording, of the staging epoch, whose unreadable
// variable place (unreadPlace) the learn step warned of: once per recording
// and staging, however many calls of its operation miss on it. Mock names
// repeat across test sets, hence the staging.
type placeWarning struct {
	epoch uint64
	mock  string
}

// excusedAt excuses what Known does and, at the leaf paths at, any value,
// type or presence.
type excusedAt struct {
	learn.Known
	at map[string]bool
}

func (k excusedAt) Excused(l learn.Leaf) (bool, bool) {
	if k.at[l.Path] {
		return true, false
	}
	return k.Known.Excused(l)
}

// elsewhereWhy is what the learn step says of a call that is exactly x, a
// recording its scope did not offer (exactlyElsewhere), where it would have
// paired the call with m: read where (learnRound.where), and the recorded
// test whose window x lies in, where the store tells it
// (integrations.RecordedTests).
func elsewhereWhy(x, m *models.Mock, where string, windows integrations.TestWindows) string {
	made := "a recording of the test set that is not offered where this call was made"
	if w, ok := windows.(integrations.RecordedTests); ok {
		if test := w.RecordedTestAt(x.Spec.ReqTimestampMock); test != "" {
			made = "which keploy recorded in " + test + "'s window"
		}
	}
	exactly := x.Name
	if hasVariables(x) {
		exactly += " (but for its variables: values the application makes anew each run)"
	}
	return "keploy record's full round read this request " + where + ", and it is exactly " + exactly + ", " + made +
		": keploy reads it as that recording's call made outside where it was recorded (a test's late or early call), not as a changed call of " + m.Name +
		", so it learns nothing onto " + m.Name + " from it"
}

// elsewhereSteps, unreadSteps and placeSteps are the next steps of a call
// exactly a recording made elsewhere, of one keploy could not tell for a
// recording it cannot read back, and for one whose variable place it cannot
// read (in place of the mismatch report's default, which speaks of a request
// that changed).
const (
	elsewhereSteps = "If the application makes this call after its test's response or before the next test's request, make the test, or the application's readiness, wait for it, then record again; if a value only happens to equal that recording's, record again so keploy sees it change."
	unreadSteps    = "This is keploy failing to read its own recordings, not a change of the request: fix what the agent's log names, then run again."
	placeSteps     = "This is a recording keploy cannot read, not a change of the request: correct or remove the variable named above in the test set's mocks.yaml, or record again, then run again."
)

// learnPass is the instant a pass of the learn step read the store at: the
// store's revision, and, read from one snapshot of the round, the round's
// staging, generation and take log's length.
type learnPass struct {
	rev   uint64
	round integrations.RoundAt
}

// learnRead is what a pass of the learn step reads (readLearnPass): the
// request's scope, the learn tracker's test (its number, its earlier places,
// what was served in it and before it), where the store places a recording
// among the recorded tests (windows), and the store a claim of this pass
// takes from (claimDb): one checked against the snapshot the pass read.
type learnRead struct {
	sc             scope
	test           uint64
	earlier        []learn.Earlier
	served, before []*models.Mock
	// set and order are F's serving of the running set as the read saw it
	// (learn.Placed): the places of the offset rule alone (givenEarlier).
	set   []*models.Mock
	order learn.Order
	// open: a test was open (learn.Placed.Open), read with test.
	open bool
	// round is the round the pass read (learnRound): what a call's later
	// passes are held to.
	round learnRound
	// awaiting: the running test still awaits its test case's request
	// (learn.Tracker.Places), and the pass learns nothing from its places.
	awaiting bool
	windows  integrations.TestWindows
	claimDb  integrations.MockMemDb
	at       learnPass
	// call is where the call was made, as this pass read it: the
	// snapshot's round (RoundSnapshot.CallRound), the call's mark from its
	// first pass on, and from a pass it caught up with (learnMatchIn). A
	// store that reads no round in one snapshot gives none (the zero
	// CallRound), and its misses name no recording made elsewhere.
	call integrations.CallRound
}

// readLearnPass reads a pass of the learn step. From a store that reads its
// round in one snapshot (integrations.RoundReader, under exact serving), it
// is that one read: the scope, whose each recording is, the tracker's test
// and what the test's own answered, of one instant, and a claim is taken
// only while the round is still at it (integrations.WithinRound). From any
// other store, the reads one after another: the scope first, then the
// tracker, so a mock a concurrent call consumed before the scope was read is
// gone from the scope, and in the tracker's account (the store tells it
// first: MockManager.observeServed). From a snapshot it reads the pass's
// mark too (learnRead.call): the call's, of its first pass and of a pass
// whose round the call caught up with (learnMatchIn).
func (h *HTTP) readLearnPass(mockDb integrations.MockMemDb, tracker *learn.Tracker, live httpParts, liveShape learn.Shape, n learnNoise, pool bool) (learnRead, error) {
	rev := storeRevision(mockDb)
	if snap, ok := integrations.SnapshotRound(mockDb, integrations.RoundQuery{Tracker: tracker}); ok {
		r := learnRead{
			sc:       h.learnScopeIn(snap, live, liveShape, n, pool),
			test:     snap.Learn.Number,
			earlier:  snap.Learn.Earlier,
			served:   snap.Learn.Served,
			before:   snap.Learn.Before,
			set:      snap.Learn.Set,
			order:    snap.Learn.Order,
			awaiting: snap.Learn.Awaiting,
			open:     snap.Learn.Open,
			windows:  snap,
			claimDb:  integrations.WithinRound(mockDb, snap.At),
			at:       learnPass{rev: rev, round: snap.At},
		}
		r.round = learnRound{read: true, open: r.open, test: r.test, snap: true,
			agrees:  snap.Phase == integrations.RoundInTest || snap.Phase == integrations.RoundBetween,
			epoch:   snap.At.Epoch,
			viewGen: snap.ViewGen(),
			view:    snap.Test,
			noOne:   snap.NoOneRound(),
			ahead:   snap.TrackerAhead(),
			alone:   snap.ViewAlone(),
		}
		r.call = snap.CallRound()
		return r, nil
	}
	sc, err := h.learnScope(mockDb, tracker, live, liveShape, n, pool)
	if err != nil {
		return learnRead{}, err
	}
	r := learnRead{sc: sc, claimDb: mockDb, at: learnPass{rev: rev}}
	p := tracker.Placed()
	r.test, r.earlier, r.served, r.before, r.awaiting, r.set, r.order = p.Test, p.Earlier, p.Served, p.Before, p.Awaiting, p.Set, p.Order
	r.open = p.Open
	r.round = learnRound{read: true, open: r.open, test: r.test}
	r.windows, _ = mockDb.(integrations.TestWindows)
	return r, nil
}

// storeRevision is the mock store's revision (MockManager.Revision: it moves
// with every change a parser can see, a consume, a staging, a view's open),
// zero for a store that keeps none.
func storeRevision(mockDb integrations.MockMemDb) uint64 {
	if r, ok := mockDb.(interface{ Revision() uint64 }); ok {
		return r.Revision()
	}
	return 0
}

// pairing is what the learn step learned on a paired mock: its noise, its
// variables (an echo included), the variables of the test bound at an earlier
// mock that it uses (binds, by that mock), and each variable's value in this
// call, for its answer. minted are the values this call's request carried
// first in the test (learned here as noise or as an echo), which the tracker
// keeps once the call is answered (learn.Tracker.Mint).
type pairing struct {
	noise  map[string][]string
	vars   models.MockVariables
	binds  map[string]models.MockVariables
	values map[string]variables.Binding
	minted []learn.Minted
}

// learnPlan turns what Classify learned about mock m (paired as rec; own: one
// of the test's own mocks) into what is written onto the recording, or says
// why the request cannot be answered (request_changed). A value never
// becomes noise when it flowed from an earlier place of the test (a
// variable) or comes back in the answer (an echo): when keploy cannot follow
// it as a variable, the request is refused instead.
//
//   - A variable is bound at its earlier place, a request place of one of
//     the test's own HTTP mocks F served, which held the value whole, and is
//     used here: it holds when the live value here is the one F saw there.
//     A value held here inside a longer one ("<name>-renamed") is used here
//     too, bound where it was sent whole (withinSource): it holds when the
//     live text here is the recorded one with that value in its place.
//   - An echo is a variable of this call alone (echo: true), bound here and
//     answered where the recorded answer holds the value whole. Leaves of
//     this call that drifted from the same recorded value are one variable.
//   - A value the test case's request gave (given: its template keys, which
//     keploy test binds for the test, #4789) is that key's variable, used
//     here under the key's name with no bind, whatever else Classify found:
//     the test case is earlier than any call it makes. Its name is the key's
//     alone in the test: no other variable takes it.
//   - A value learned as noise or as an echo at a variable place of one of
//     the test's own calls first appeared there: the test made it (minted).
//     It stays what it was learned as until a later call of the test answers
//     it whole (mintedAnswers): then it is the test's variable, bound here.
//   - outside: m is a pool call of the claim key, made while no test is open,
//     in a full record (learnMatch). What it sends first is the key's
//     (minted, Outside): a later call of the key, outside every test or in
//     one of its tests, that sends it again uses the key's variable, bound
//     here, and one that answers it answers it. Its names are the key's
//     (learn.Tracker.OutsideName).
func learnPlan(tracker *learn.Tracker, test uint64, m *models.Mock, rec learn.Shape, own, outside bool, learned []learn.Learned, earlier []learn.Earlier, given []variables.Binding) (pairing, string) {
	p := pairing{noise: map[string][]string{}, vars: models.MockVariables{}, binds: map[string]models.MockVariables{}, values: map[string]variables.Binding{}}
	jsonReq := rec.Fixed["body"] == "json"
	reserved := reservedNames(given)
	answers := func(l learn.Learned, from []learn.Leaf) []string {
		var out []string
		for _, a := range from {
			if !learn.WholeEcho(l.Recorded, a) {
				continue
			}
			if place, ok := learn.VarPlace(a, true); ok {
				out = append(out, place)
			}
		}
		return out
	}
	echoed := map[string]string{} // recorded value -> its echo variable
	// A text file's content that holds a value this very call sends whole at
	// another place (an upload's test-set id, in its form field and inside the
	// file it uploads): the value first appears here, so the call binds it at
	// that place and uses it inside the file, exactly (sameCallWithin).
	fileUses := sameCallWithin(learned, jsonReq, respLeavesOf(m))
	for _, l := range learned {
		r := l.Recorded
		if src, ok := fileUses[r.Path]; ok && own {
			name, ok := tracker.Name(test, m.Name, src.place, reserved)
			if !ok {
				return p, "the test ended before " + r.Path + " was learned"
			}
			v := p.vars[name]
			v.Recorded, v.Bind = src.recorded, src.place
			if use, ok := learn.VarPlace(r, jsonReq); ok {
				v.Use = appendNew(v.Use, use)
			}
			v.Answer = appendNew(v.Answer, src.answers...)
			p.vars[name] = v
			p.values[name] = variables.Binding{Name: name, Value: src.live, Recorded: src.recorded, Mock: m.Name, Place: src.place}
			continue
		}
		if src, ok := fileBinds(fileUses, r.Path); ok && own {
			// The place that binds what the file holds: a variable, not noise.
			name, ok := tracker.Name(test, m.Name, src.place, reserved)
			if !ok {
				return p, "the test ended before " + r.Path + " was learned"
			}
			v := p.vars[name]
			v.Recorded, v.Bind = src.recorded, src.place
			v.Answer = appendNew(v.Answer, src.answers...)
			p.vars[name] = v
			p.values[name] = variables.Binding{Name: name, Value: src.live, Recorded: src.recorded, Mock: m.Name, Place: src.place}
			p.minted = append(p.minted, learn.Minted{Mock: m.Name, Place: src.place, Recorded: src.recorded, Live: src.live, Type: r.Type})
			continue
		}
		if r.File && (l.Kind == learn.KindNoise || l.Kind == learn.KindEcho) {
			// A file's content is compared whole: what changed in it must be a
			// value keploy follows, never noise.
			return p, r.Path + " is a file's content, which keploy compares whole, and it changed by more than a value keploy follows"
		}
		if g, ok := givenFor(r, l.Live, given); ok {
			// The test case gave this value: a use of its key, no bind.
			place, placeable := learn.VarPlace(r, jsonReq)
			switch {
			case !placeable:
				return p, r.Path + " carries the test case's " + g.Name + " at a place keploy cannot follow a variable to (a form or multipart field)"
			case !own && !(outside && g.Outside):
				// A claim key's given value (its harness gave it outside every
				// test: a group's beforeAll) is carried by the key's calls
				// outside its tests too.
				return p, r.Path + " carries the test case's " + g.Name + ", and only a test's own calls carry its variables"
			}
			v := p.vars[g.Name]
			v.Recorded, v.Use, v.Outside = g.Recorded, appendNew(v.Use, place), g.Outside
			v.Answer = appendNew(v.Answer, answers(l, respLeavesOf(m))...)
			p.vars[g.Name] = v
			p.values[g.Name] = g
			// The application now holds it: a later call's answer that holds
			// it is answered with this run's value (mintedAnswers), by the
			// given name.
			typ := r.Type
			if !variables.SameValue(g.Recorded, r.Value, r.Type == learn.Number) {
				typ = learn.String // a token of a longer text (givenFor)
			}
			p.minted = append(p.minted, learn.Minted{Mock: m.Name, Place: place, Recorded: g.Recorded, Live: g.Value, Type: typ, Outside: g.Outside, Given: g.Name})
			continue
		}
		switch l.Kind {
		case learn.KindNoise:
			p.noise[r.Path] = []string{}
			if place, ok := learn.VarPlace(r, jsonReq); ok && (own || outside) {
				p.minted = append(p.minted, learn.Minted{Mock: m.Name, Place: place, Recorded: r.Value, Live: l.Live.Value, Type: r.Type, Outside: outside})
			}

		case learn.KindVariable:
			from := l.From
			// word is the variable's recorded value: r's own, or the value an
			// earlier call sent whole that r holds inside a longer one, or a
			// word both hold inside longer values (inside: bound there inside
			// the earlier call's value, sharedSource).
			word, inside := r.Value, false
			switch {
			case from.Given:
				// The test case's own request (learn.Earlier.Given): what
				// the application was given, which no variable binds at.
				why := r.Path + " carries a value the test case's own request holds (" + from.Mock + " at " + from.Path + "), which keploy cannot follow into a call yet"
				if l.Why != "" {
					why += "; " + l.Why
				}
				return p, why
			case from.Side == "":
				why := r.Path + " carries a value the test's recording holds earlier (in " + from.Mock + "), where keploy cannot bind it yet: a call of another protocol, or inside its payload"
				if l.Why != "" {
					why += "; " + l.Why
				}
				return p, why
			case from.Side != learn.Request:
				why := r.Path + " carries a value " + from.Mock + " answered earlier in the test at " + from.Path + ", which keploy cannot follow from an answer"
				if l.Why != "" {
					why += "; " + l.Why
				}
				return p, why
			case !from.Whole || from.Var == "":
				src, w, ok := withinSource(r, l.Live, earlier)
				if !ok {
					src, w, ok = sharedSource(r, l.Live, earlier)
					inside = ok
				}
				if !ok {
					why := r.Path + " carries a value " + from.Mock + " sent earlier in the test at " + from.Path + ", inside a longer value or at a place keploy cannot bind it"
					if l.Why != "" {
						why += "; " + l.Why
					}
					return p, why
				}
				from, word = src, w
			}
			if !(own || outside) || !from.Own && !from.Outside {
				return p, r.Path + " carries a value " + from.Mock + " sent earlier, and only a test's own calls carry its variables"
			}
			use, ok := learn.VarPlace(r, jsonReq)
			if !ok {
				return p, r.Path + " carries a value " + from.Mock + " sent earlier in the test, at a place keploy cannot follow a variable to (a form or multipart field)"
			}
			// withinSource checked a value inside a longer one already: the
			// live text is the recorded one with the live value in its place.
			if word == r.Value && !variables.SameValue(from.Live, l.Live.Value, r.Type == learn.Number) {
				return p, r.Path + " does not hold the value the test sent at " + from.Var + " of " + from.Mock + ", which it held when recorded"
			}
			name, ok := tracker.Name(test, from.Mock, from.Var, reserved)
			if inside {
				// A word of the earlier place's value: a variable of its own
				// beside the one the place may hold whole.
				name, ok = tracker.NameWithin(test, from.Mock, from.Var, word, reserved)
			}
			if from.Outside {
				// The claim key's variable, bound at a call made outside
				// every test.
				name, ok = tracker.OutsideName(from.Mock, from.Var)
			}
			if !ok {
				return p, "the test ended before " + r.Path + " was learned"
			}
			v := p.vars[name]
			v.Recorded, v.Use, v.Outside = word, appendNew(v.Use, use), from.Outside
			if word == r.Value {
				v.Answer = appendNew(v.Answer, answers(l, respLeavesOf(m))...)
			} else {
				v.Answer = appendNew(v.Answer, withinAnswers(word, respLeavesOf(m))...)
			}
			p.vars[name] = v
			b := p.binds[from.Mock]
			if b == nil {
				b = models.MockVariables{}
				p.binds[from.Mock] = b
			}
			bv := b[name]
			bv.Recorded, bv.Bind, bv.Outside = word, from.Var, from.Outside
			// F's clock evidence, read where the variable is bound: a clock
			// read that moved by the lead there may share its live value with
			// another one read in the same second in a replay
			// (models.MockVariable.Clock), read of the word bound (r's value,
			// or the value it holds inside a longer one). None in keploy
			// mock's client tests, where the lead shows no read of the app's
			// clock (learn.DriftCache.ClockReads).
			bv.Clock = tracker.Drifts().ClockReads(word, from.Live)
			bv.Answer = appendNew(bv.Answer, earlierAnswers(earlier, from.Mock, learn.Leaf{Path: r.Path, Type: r.Type, Value: word})...)
			b[name] = bv
			p.values[name] = variables.Binding{Name: name, Value: from.Live, Recorded: word, Mock: from.Mock, Place: from.Var, Outside: from.Outside}

		case learn.KindUnaccounted:
			return p, r.Path + " changed, and " + l.Why

		case learn.KindEcho:
			place, ok := learn.VarPlace(r, jsonReq)
			if !ok {
				return p, r.Path + " is echoed in the answer, at a place keploy cannot bind it (a form or multipart field)"
			}
			at := answers(l, l.Echo)
			if len(at) == 0 {
				// Echoes names every place the answer holds it; none is
				// one keploy can answer with this run's value.
				if e := l.Echo[0]; e.Type != learn.Keys && (learn.WholeEcho(r, e) || e.Unnamed && e.Value == r.Value) {
					return p, r.Path + " is echoed in the answer whole only at " + l.Echo[0].Path + ", a place keploy cannot answer with this run's value"
				}
				return p, r.Path + " is echoed in the answer only inside a longer value, in another form or at a key (" + l.Echo[0].Path + "), which keploy cannot answer with this run's value"
			}
			if name, ok := echoed[r.Value]; ok {
				// One recorded value is one variable only while the call
				// sends one value for it.
				if !variables.SameValue(p.values[name].Value, l.Live.Value, r.Type == learn.Number) {
					return p, r.Path + " and " + p.values[name].Place + " held one value when recorded and two now, so they are not one value the answer echoes"
				}
				v := p.vars[name]
				v.Use = appendNew(v.Use, place)
				p.vars[name] = v
				continue
			}
			name, ok := tracker.Name(test, m.Name, place, reserved)
			if outside {
				// Named as the key's: a later call of the key that answers
				// it makes it the key's variable (mintedAnswers).
				name, ok = tracker.OutsideName(m.Name, place)
			}
			if !ok {
				return p, "the test ended before " + r.Path + " was learned"
			}
			echoed[r.Value] = name
			p.vars[name] = models.MockVariable{Recorded: r.Value, Bind: place, Answer: at, Echo: true}
			p.values[name] = variables.Binding{Name: name, Value: l.Live.Value, Recorded: r.Value, Mock: m.Name, Place: place}
			if own || outside {
				p.minted = append(p.minted, learn.Minted{Mock: m.Name, Place: place, Recorded: r.Value, Live: l.Live.Value, Type: r.Type, Outside: outside})
			}
		}
	}
	return p, ""
}

// minWithin is the length below which a value is not followed inside a
// longer one: a short word equal by chance to one the test sent ("1", "on")
// is not that value. Such a call is refused, as before.
const minWithin = 6

// withinSource is the earlier place whose value the drifted leaf r (live:
// what F saw there) carries inside a longer value: a request place that sent
// a value w whole (at least minWithin long, a string or a number) that
// stands in r's recorded value as a token (learn.ContainsToken), where F saw
// it drift, and whose live value put in w's place in r's recorded value
// (learn.ReplaceToken) is r's live value, exactly: "<name>-renamed" carries
// the name a create sent whole, "doc:<id>" the id. Of the test's own calls
// F served before, else the claim key's made outside every test, the
// earliest. ok is false when no place is: the call is refused, as before.
// The place holds w whole, so the variable is bound there.
func withinSource(r, live learn.Leaf, earlier []learn.Earlier) (from learn.Place, w string, ok bool) {
	if r.Type != learn.String || r.Value == "" {
		return learn.Place{}, "", false
	}
	served := append([]learn.Earlier(nil), earlier...)
	sort.SliceStable(served, func(i, j int) bool { return served[i].At.Before(served[j].At) })
	for _, outside := range []bool{false, true} {
		for _, e := range served {
			if !outside && !e.Own || outside && (e.Own || !e.Outside) {
				continue
			}
			for _, s := range e.Seen {
				v := s.Leaf.Value
				if s.Side != learn.Request || s.Place == "" || s.Leaf.Unnamed || s.Leaf.Type != learn.String && s.Leaf.Type != learn.Number ||
					s.Live == "" || s.Live == v || len(v) < minWithin || v == r.Value || !learn.ContainsToken(r.Value, v) {
					continue
				}
				if with, _ := learn.ReplaceToken(r.Value, v, s.Live); with != live.Value {
					continue
				}
				return learn.Place{Mock: e.Mock, Side: learn.Request, Path: s.Leaf.Path, Noise: s.Leaf.Noise, Var: s.Place, Live: s.Live,
					Whole: true, Own: e.Own, Run: e.Run, Outside: e.Outside, Name: s.Name}, v, true
			}
		}
	}
	return learn.Place{}, "", false
}

// sharedSource is, for a drifted leaf r (live: what F saw there) that withinSource
// finds no whole value inside, the test's own earlier request place whose value
// holds, inside a longer value too, a word r's value holds: a token w (at
// least minWithin long, as learn.ReplaceToken reads tokens) standing in both
// recorded values, where F's live value of each is its recorded one with one
// value w' in w's places, exactly (learn.TokenValue, learn.ReplaceToken): the
// time two names a test made of one Date.now() ("e2e-role-<ts>", then
// "e2e-case-<ts>"). w is the variable: bound inside the earlier place's value,
// where it first stands, and used inside r's. Of the test's own calls F served,
// the earliest; ok is false when none is, and the call is refused, as before.
// The place's live value (Place.Live) is w', the variable's value.
func sharedSource(r, live learn.Leaf, earlier []learn.Earlier) (from learn.Place, w string, ok bool) {
	if r.Type != learn.String || r.Value == "" || live.Value == "" {
		return learn.Place{}, "", false
	}
	served := append([]learn.Earlier(nil), earlier...)
	sort.SliceStable(served, func(i, j int) bool { return served[i].At.Before(served[j].At) })
	words := sortedWords(r.Value)
	for _, e := range served {
		if !e.Own {
			continue
		}
		for _, s := range e.Seen {
			v := s.Leaf.Value
			if s.Side != learn.Request || s.Place == "" || s.Leaf.Unnamed || s.Leaf.Type != learn.String ||
				s.Live == "" || s.Live == v || v == r.Value {
				continue
			}
			for _, cand := range words {
				if len(cand) < minWithin || cand == v || !learn.ContainsToken(v, cand) {
					continue
				}
				wl, ok := learn.TokenValue(v, cand, s.Live)
				if !ok {
					continue
				}
				if with, _ := learn.ReplaceToken(r.Value, cand, wl); with != live.Value {
					continue
				}
				return learn.Place{Mock: e.Mock, Side: learn.Request, Path: s.Leaf.Path, Noise: s.Leaf.Noise, Var: s.Place, Live: wl,
					Own: e.Own, Run: e.Run, Outside: e.Outside}, cand, true
			}
		}
	}
	return learn.Place{}, "", false
}

// sortedWords are the ASCII letter-and-digit words of s, longest first, each
// once: the words a value may share with another, the most specific first.
func sortedWords(s string) []string {
	seen := map[string]bool{}
	var out []string
	for i := 0; i < len(s); {
		if !isASCIIWord(s[i]) {
			i++
			continue
		}
		j := i
		for j < len(s) && isASCIIWord(s[j]) {
			j++
		}
		if w := s[i:j]; !seen[w] {
			seen[w] = true
			out = append(out, w)
		}
		i = j
	}
	sort.SliceStable(out, func(i, j int) bool { return len(out[i]) > len(out[j]) })
	return out
}

func isASCIIWord(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

// fileSource is the place of a call that sends, whole, a value a text file
// of the same call holds inside it: the variable's bind place, and the value
// recorded and in this run.
type fileSource struct {
	path, place    string
	recorded, live string
	// answers are the call's answer places that hold the value, whole or
	// inside a longer text (withinAnswers): answered with this run's value.
	answers []string
}

// sameCallWithin maps each text file leaf of a call (learned: what F saw drift
// in it) whose change is exactly one value the same call sends whole at
// another place, first appearing there (no earlier call sent it), to that
// place: the file's recorded text with that place's live value in the
// recorded value's places (learn.ReplaceToken) is the file's live text. The
// value is at least minWithin long, a string, at a variable's place. Each
// was learned as noise, as an echo, or refused only because a word of it
// comes back in the call's answer (Learned.Echoed): when the answer holds it,
// every answer place holding a word of what changed must hold the value
// whole or as a token, so that the variable answers it with this run's value
// (resp: the call's recorded answer).
func sameCallWithin(learned []learn.Learned, jsonReq bool, resp []learn.Leaf) map[string]fileSource {
	fresh := func(l learn.Learned) bool {
		return l.Kind == learn.KindNoise || l.Kind == learn.KindEcho || l.Kind == learn.KindUnaccounted && l.Echoed
	}
	echoed := func(l learn.Learned) bool { return l.Kind == learn.KindEcho || l.Echoed }
	out := map[string]fileSource{}
	for _, f := range learned {
		r := f.Recorded
		if !r.File || r.Value == "" || !fresh(f) {
			continue
		}
		for _, s := range learned {
			v := s.Recorded
			if v.Path == r.Path || v.File || !fresh(s) || v.Type != learn.String || len(v.Value) < minWithin ||
				s.Live.Value == "" || s.Live.Value == v.Value || !learn.ContainsToken(r.Value, v.Value) {
				continue
			}
			place, ok := learn.VarPlace(v, jsonReq)
			if !ok {
				continue
			}
			if with, _ := learn.ReplaceToken(r.Value, v.Value, s.Live.Value); with != f.Live.Value {
				continue
			}
			if (echoed(f) || echoed(s)) && !answeredWhole(v.Value, s.Live.Value, resp) {
				continue
			}
			out[r.Path] = fileSource{path: v.Path, place: place, recorded: v.Value, live: s.Live.Value, answers: withinAnswers(v.Value, resp)}
			break
		}
	}
	return out
}

// answeredWhole reports whether every answer leaf that holds a word the value
// lost (recorded, not live) holds the value itself, whole or as a token, at a
// place a variable can answer: so answering the value accounts for every word
// of it the answer echoes.
func answeredWhole(recorded, live string, resp []learn.Leaf) bool {
	liveWords := map[string]bool{}
	for _, w := range sortedWords(live) {
		liveWords[w] = true
	}
	var lost []string
	for _, w := range sortedWords(recorded) {
		if !liveWords[w] {
			lost = append(lost, w)
		}
	}
	for _, a := range resp {
		holds := false
		for _, w := range lost {
			if learn.ContainsToken(a.Value, w) {
				holds = true
				break
			}
		}
		if !holds {
			continue
		}
		if _, ok := learn.VarPlace(a, true); !ok || a.Unnamed || !learn.ContainsToken(a.Value, recorded) && !learn.WholeEcho(learn.Leaf{Value: recorded}, a) {
			return false
		}
	}
	return true
}

// fileBinds reports whether the leaf at path is the place that binds a value
// a file of its call holds (sameCallWithin), and that place.
func fileBinds(uses map[string]fileSource, path string) (fileSource, bool) {
	for _, src := range uses {
		if src.path == path {
			return src, true
		}
	}
	return fileSource{}, false
}

// withinAnswers are the places of an answer (leaves) that hold w, a value
// the call carries inside a longer one: whole (learn.WholeEcho), or as a
// token of a text (the recorded "<name>-renamed" the call sent, answered
// back). Each is answered with the text holding this run's value in w's
// place (renderAnswers).
func withinAnswers(w string, leaves []learn.Leaf) []string {
	var out []string
	for _, a := range leaves {
		if !learn.WholeEcho(learn.Leaf{Value: w}, a) && (a.Unnamed || a.Type != learn.String || !learn.ContainsToken(a.Value, w)) {
			continue
		}
		if place, ok := learn.VarPlace(a, true); ok {
			out = append(out, place)
		}
	}
	return out
}

// withinPlaces is e, an exchange F served, with no variable place at a
// request place where it holds a variable of vars inside a longer value (a
// use withinSource followed): what it sent there is that variable's text, no
// value of its own to bind. A later call that sends the text again follows
// it to the variable's own place (withinSource), never binds another
// variable at this one.
func withinPlaces(e learn.Earlier, vars models.MockVariables) learn.Earlier {
	recorded := map[string]string{} // a use place -> its variable's recorded value
	for _, v := range vars {
		for _, u := range v.Use {
			recorded[u] = v.Recorded
		}
	}
	if len(recorded) == 0 {
		return e
	}
	e.Seen = append([]learn.Seen(nil), e.Seen...)
	for i, s := range e.Seen {
		if w, ok := recorded[s.Place]; ok && s.Side == learn.Request && !variables.SameValue(w, s.Leaf.Value, s.Leaf.Type == learn.Number) {
			e.Seen[i].Place = ""
		}
	}
	return e
}

// reservedNames are the names of the values the test case gave (its template
// keys): no other variable of the test takes one.
func reservedNames(given []variables.Binding) map[string]bool {
	reserved := make(map[string]bool, len(given))
	for _, g := range given {
		reserved[g.Name] = true
	}
	return reserved
}

// mintedAnswers is p, what the learn step learned on the test's own mock m
// in test number n (or on a pool call of the claim key made outside every
// test), with the answer places of m's recorded answer that hold a value of
// minted, made at an earlier call (learn.Minted: the test's, or the claim
// key's): a scalar of the answer's JSON body that holds the value whole, of
// the type the request carried it as (a string or a number), or a string of
// it that holds a string value as a token of a longer text (mintedWithin:
// "Test Suites/<name>"), answered with this run's value in its place; never
// an object key or a header. Each makes the value a
// variable (the test's, or the key's: Outside): answered here with this
// run's value, and bound where it first appeared (the bind rides on m's
// state onto that call, which turns what was learned there, noise or an
// echo, into the variable's bind). A value the test case gave (Minted.Given:
// a name the harness typed into the page, once a call carried it) is its
// given name's variable, which the table binds: it is only answered here.
// An answer place a variable of p answers
// already is left to it. When m's answer cannot hold them all, p is returned
// as it was: the call is answered as it was before.
func (h *HTTP) mintedAnswers(tracker *learn.Tracker, test uint64, m *models.Mock, p pairing, reserved map[string]bool, minted []learn.Minted) pairing {
	if len(minted) == 0 {
		return p
	}
	// A big answer the disk store keeps apart from its mock (strict mock
	// windows: proxy.EligibleForResponseSpill) is read loaded, as it is
	// served: m's own Spec holds no answer then.
	loaded, err := withResponse(m)
	if err != nil || loaded.Spec.HTTPResp == nil {
		return p
	}
	taken := map[string]bool{}
	for _, v := range p.vars {
		for _, a := range v.Answer {
			taken[a] = true
		}
	}
	leaves := respLeaves(loaded.Spec.HTTPResp)
	vars, binds := models.MockVariables{}, map[string]models.MockVariables{}
	values := make(map[string]variables.Binding, len(p.values))
	for k, v := range p.values {
		values[k] = v
	}
	within := map[string]bool{} // answer places that hold minted values inside a longer text
	for _, v := range minted {
		if v.Mock == m.Name {
			// Its own answer: an echo, learned with its request.
			continue
		}
		var at []string
		for _, a := range leaves {
			if !strings.HasPrefix(a.Path, "body") || a.Unnamed {
				continue
			}
			whole := a.Type == v.Type && variables.SameValue(v.Recorded, a.Value, v.Type == learn.Number)
			if !whole && !mintedWithin(v, a) {
				continue
			}
			place, ok := learn.VarPlace(a, true)
			if !ok || taken[place] || whole && within[place] {
				continue
			}
			if whole {
				taken[place] = true
			} else {
				// One text may hold several values the test made, each
				// answered in its place (renderAnswers).
				within[place] = true
			}
			at = append(at, place)
		}
		if len(at) == 0 {
			continue
		}
		var name string
		var ok bool
		switch {
		case v.Given != "":
			// A value the test case gave: the table binds its name (the
			// harness gives it in every run), so no call binds it.
			name, ok = v.Given, true
		case v.Outside:
			name, ok = tracker.OutsideName(v.Mock, v.Place)
		default:
			name, ok = tracker.Name(test, v.Mock, v.Place, reserved)
		}
		if !ok {
			// The test ended: nothing more is learned in it.
			return p
		}
		if have, ok := p.vars[name]; ok && (have.Recorded != v.Recorded || have.Outside != v.Outside) {
			continue
		}
		vars[name] = models.MockVariable{Recorded: v.Recorded, Answer: at, Outside: v.Outside}
		if v.Given != "" {
			if _, ok := values[name]; !ok {
				values[name] = variables.Binding{Name: name, Value: v.Live, Recorded: v.Recorded, Outside: v.Outside, Given: true}
			}
			continue
		}
		if binds[v.Mock] == nil {
			binds[v.Mock] = models.MockVariables{}
		}
		binds[v.Mock][name] = models.MockVariable{Recorded: v.Recorded, Bind: v.Place, Outside: v.Outside}
		if _, ok := values[name]; !ok {
			values[name] = variables.Binding{Name: name, Value: v.Live, Recorded: v.Recorded, Mock: v.Mock, Place: v.Place, Outside: v.Outside}
		}
	}
	if len(vars) == 0 {
		return p
	}
	out := pairing{noise: p.noise, minted: p.minted, vars: mergeVariables(p.vars, vars), binds: mergeBinds(p.binds, binds), values: values}
	// The answer must hold every variable with this run's value, as the
	// variable pass answers it in the replays after F.
	decls, problem := readDecls(&models.Mock{Name: m.Name, Spec: models.MockSpec{Variables: out.vars}})
	if problem == "" {
		if _, vp := h.renderAnswers(loaded, &varHit{mock: m, decls: decls, local: out.values}, &variables.Table{}); vp != nil {
			problem = vp.why
		}
	}
	if problem != "" {
		h.Logger.Debug("the answer of a later call holds a value the test made, and cannot be answered with this run's value; it is served as recorded",
			zap.String("mock", m.Name), zap.String("why", problem))
		return p
	}
	return out
}

// mintedWithin reports whether answer leaf a holds the value the test made,
// v, inside a longer text: as a token of a string (learn.ContainsToken), a
// string v at least minWithin long ("Test Suites/<name>", "<name>-renamed").
func mintedWithin(v learn.Minted, a learn.Leaf) bool {
	return v.Type == learn.String && a.Type == learn.String && len(v.Recorded) >= minWithin && learn.ContainsToken(a.Value, v.Recorded)
}

// logMintedAnswers says, for a served mock, each answer place of a value the
// test made that it learned (a variable m answers and whose request places
// are another call's: mintedAnswers).
func (h *HTTP) logMintedAnswers(m *models.Mock, vars models.MockVariables) {
	for name, v := range vars {
		if v.Bind != "" || len(v.Use) > 0 {
			continue
		}
		for _, a := range v.Answer {
			h.Logger.Info("learned an answer that holds a value the test made earlier; it is answered with this run's value",
				zap.String("mock", m.Name), zap.String("field", a), zap.String("variable", name))
		}
	}
}

// logGivenUses says, for a served mock its call matched exactly, each request
// place that uses a value the test case gave, unchanged in this run
// (givenUses).
func (h *HTTP) logGivenUses(m *models.Mock, vars models.MockVariables) {
	for name, v := range vars {
		for _, u := range v.Use {
			h.Logger.Info("followed a value the test case gave, which the call carried unchanged; the next run's is answered too",
				zap.String("mock", m.Name), zap.String("field", u), zap.String("variable", name))
		}
	}
}

// givenBindings are the values the running test case's request gave
// (variables.Binding.Given), as keploy test bound them for the test: none
// for a store that keeps no variables.
func givenBindings(mockDb integrations.MockMemDb) []variables.Binding {
	table := variableTable(mockDb)
	if table == nil {
		return nil
	}
	var out []variables.Binding
	// The test's, then the claim key's (given by the harness while no test was
	// open), which every test of the key and its calls outside them carry.
	for _, b := range append(table.Snapshot(), table.OutsideSnapshot()...) {
		if b.Given {
			out = append(out, b)
		}
	}
	return out
}

// givenLeaf is learn.PairGiven's reading of given: whether a drifted leaf
// carries one of its values, as the learn step binds it (givenFor). nil when
// nothing was given.
func givenLeaf(given []variables.Binding) func(rec, live learn.Leaf) bool {
	if len(given) == 0 {
		return nil
	}
	return func(rec, live learn.Leaf) bool {
		_, ok := givenFor(rec, live, given)
		return ok
	}
}

// givenFor is the given value the drifted leaf carries: the key's value
// when recorded (r) and in this run (live). An equal recorded value alone is
// not the key's (two counters both at 1 when recorded are two values).
func givenFor(r, live learn.Leaf, given []variables.Binding) (variables.Binding, bool) {
	number := r.Type == learn.Number
	var whole, within []variables.Binding
	for _, g := range given {
		if r.Value != "" && variables.SameValue(g.Recorded, r.Value, number) && variables.SameValue(g.Value, live.Value, number) {
			whole = append(whole, g)
		}
	}
	if len(whole) > 0 {
		return latestGiven(whole), true
	}
	// A given value inside a longer one (a session token the test installs,
	// in the Cookie header with the run's other cookies): the recorded text
	// holds its recorded value as a token, and the live text is that text
	// with this run's value in its place, exactly (the variable pass then
	// asserts it so in every replay: usedWithin).
	if r.Type != learn.String {
		return variables.Binding{}, false
	}
	for _, g := range given {
		if len(g.Recorded) < minWithin || g.Recorded == r.Value || !learn.ContainsToken(r.Value, g.Recorded) {
			continue
		}
		if with, _ := learn.ReplaceToken(r.Value, g.Recorded, g.Value); with == live.Value {
			within = append(within, g)
		}
	}
	if len(within) > 0 {
		return latestGiven(within), true
	}
	return variables.Binding{}, false
}

// givenUses is what a call that is exactly its recording m (rec: its shape)
// learns of the values the test case gave that it carries unchanged: F sent
// the value it recorded (a session the test signs for each call, signed in the
// second it was recorded in), so nothing drifted for the learn step to learn,
// yet the next run signs another, and its call would be refused. Each request
// place that holds a given value whole, or as a token of a longer text (the
// Cookie header), uses that value's name (givenFor), as a value F saw change
// does in learnPlan, and an answer place of m that echoes it whole answers it;
// the application then holds it (Minted.Given: mintedAnswers). Only a value at
// least minWithin long: a short one ("1", "main") the test gave may stand in
// a call by chance. outside: the call is the claim key's, made outside every
// test, which carries only the key's values (Binding.Outside).
func givenUses(m *models.Mock, rec learn.Shape, given []variables.Binding, outside bool) pairing {
	var held []variables.Binding
	for _, g := range given {
		if g.Recorded == g.Value && len(g.Recorded) >= minWithin && (!outside || g.Outside) {
			held = append(held, g)
		}
	}
	p := pairing{vars: models.MockVariables{}, values: map[string]variables.Binding{}}
	if len(held) == 0 {
		return p
	}
	jsonReq := rec.Fixed["body"] == "json"
	var answers []learn.Leaf
	if answered, err := withResponse(m); err == nil {
		answers = respLeavesOf(answered)
	}
	paths := make([]string, 0, len(rec.Leaves))
	for path := range rec.Leaves {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		r := rec.Leaves[path]
		if r.Unnamed {
			continue
		}
		g, ok := givenFor(r, r, held)
		if !ok {
			continue
		}
		place, placeable := learn.VarPlace(r, jsonReq)
		if !placeable {
			continue
		}
		v := p.vars[g.Name]
		v.Recorded, v.Use, v.Outside = g.Recorded, appendNew(v.Use, place), g.Outside
		for _, a := range answers {
			if learn.WholeEcho(r, a) {
				if at, ok := learn.VarPlace(a, true); ok {
					v.Answer = appendNew(v.Answer, at)
				}
			}
		}
		p.vars[g.Name] = v
		p.values[g.Name] = g
		typ := r.Type
		if !variables.SameValue(g.Recorded, r.Value, r.Type == learn.Number) {
			typ = learn.String // a token of a longer text (givenFor)
		}
		p.minted = append(p.minted, learn.Minted{Mock: m.Name, Place: place, Recorded: g.Recorded, Live: g.Value, Type: typ, Outside: g.Outside, Given: g.Name})
	}
	return p
}

// latestGiven is, of the given values a call carries, the one it carries: of
// one name, the latest install. A harness names each value it gives by what
// it is and the order the test installed it ("cookie-jwt-3"), and gives it as
// it is installed, before any call carries it: a value a helper signs for
// each call (a per-request header) is given right before that call, and a
// cookie stays the latest one installed. Two installs a run made in one
// second hold one value, recorded and live alike, and carry it to different
// calls: the call's own is the latest installed so far, which a replay that
// made them in two seconds tells apart. Of two names (a session's jwt and
// refresh cookies, set to one token), the first by name: the call holds
// both. given is in name order (variables.Table.Snapshot).
func latestGiven(gs []variables.Binding) variables.Binding {
	best := gs[0]
	bestBase, bestN := installOrder(best.Name)
	for _, g := range gs[1:] {
		base, n := installOrder(g.Name)
		if base == bestBase && n > bestN {
			best, bestN = g, n
		}
	}
	return best
}

// installOrder splits a given value's name into what it is and the order of
// its install ("cookie-jwt", 3 of "cookie-jwt-3"); a name with no order is
// its own, with order 0.
func installOrder(name string) (string, int) {
	i := strings.LastIndexByte(name, '-')
	if i < 0 || i == len(name)-1 {
		return name, 0
	}
	n, err := strconv.Atoi(name[i+1:])
	if err != nil || n < 0 {
		return name, 0
	}
	return name[:i], n
}

// respLeavesOf are the leaves of m's recorded answer.
func respLeavesOf(m *models.Mock) []learn.Leaf { return respLeaves(m.Spec.HTTPResp) }

// earlierAnswers are the answer places of the earlier mock that hold r's
// recorded value whole: where a variable bound in its request is answered.
func earlierAnswers(earlier []learn.Earlier, mock string, r learn.Leaf) []string {
	var out []string
	for _, e := range earlier {
		if e.Mock != mock {
			continue
		}
		for _, s := range e.Seen {
			if s.Side == learn.Response && s.Place != "" && learn.WholeEcho(r, s.Leaf) {
				out = appendNew(out, s.Place)
			}
		}
	}
	return out
}

// mergeVariables and mergeBinds merge two learned blocks (models).
var (
	mergeVariables = models.MergeVariables
	mergeBinds     = models.MergeBinds
)

// appendNew appends to list each of vs it does not hold yet.
func appendNew(list []string, vs ...string) []string {
	for _, v := range vs {
		dup := false
		for _, have := range list {
			if have == v {
				dup = true
				break
			}
		}
		if !dup {
			list = append(list, v)
		}
	}
	return list
}

// claimLearned serves mock m the learn step paired: its answer holds each of
// the variables it learned with this run's value (renderAnswers), and the
// mock is consumed with what was learned on its state (the noise, its
// variables, and the variables it uses that are bound at earlier mocks), as
// detected noise rides. why says why the answer cannot hold a variable's
// value (nothing is consumed then); claimed is false when another connection
// took m first. owner says which of the scope's mocks are the open test's
// own: one no open test owns (a stated stateful recording of the pool) is
// kept reusable, as the matcher keeps it (ownedKey).
func (h *HTTP) claimLearned(ctx context.Context, m *models.Mock, mockDb integrations.MockMemDb, p pairing, owner func(*models.Mock) bool) (served *models.Mock, claimed bool, why string, err error) {
	served, err = withResponse(m)
	if err != nil {
		return nil, false, "", err
	}
	if len(p.vars) > 0 {
		decls, problem := readDecls(&models.Mock{Name: m.Name, Spec: models.MockSpec{Variables: p.vars}})
		if problem != "" {
			return nil, false, problem, nil
		}
		// local holds every variable this call answers; the empty table
		// guards renderAnswers' table lookup all the same.
		rendered, vp := h.renderAnswers(served, &varHit{mock: m, decls: decls, local: p.values}, &variables.Table{})
		if vp != nil {
			return nil, false, vp.why, nil
		}
		served = rendered
	}
	claimed = h.consumeWith(context.WithValue(ctx, ownedKey{}, owner), m, mockDb, func(spec *models.MockSpec) {
		if len(p.noise) > 0 {
			spec.ReqBodyNoise = mergeReqBodyNoise(spec.ReqBodyNoise, p.noise)
		}
		// What was learned rides out on the consumed mock's state, never on
		// what the pool serves next: a reused mock stays the recording the
		// learn step pairs again (not one the variable pass alone serves).
		// A reused mock paired again in the round keeps what it learned
		// before too.
		if len(p.vars) > 0 || len(p.binds) > 0 {
			spec.SetLearned(mergeVariables(spec.Learned(), p.vars), mergeBinds(spec.LearnedBinds(), p.binds))
		}
	})
	return served, claimed, "", nil
}

// inRecordedOrder is mocks in the order they were recorded.
func inRecordedOrder(mocks []*models.Mock) []*models.Mock {
	out := append([]*models.Mock(nil), mocks...)
	sort.SliceStable(out, func(i, j int) bool {
		ti, tj := out[i].Spec.ReqTimestampMock, out[j].Spec.ReqTimestampMock
		if !ti.Equal(tj) {
			return ti.Before(tj)
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// leafPaths are leaves' exact paths, sorted, without repeats.
func leafPaths(ls []learn.Leaf) []string {
	seen := map[string]bool{}
	var out []string
	for _, l := range ls {
		if !seen[l.Path] {
			seen[l.Path] = true
			out = append(out, l.Path)
		}
	}
	sort.Strings(out)
	return out
}

// servedExchange is the exchange F served from m (paired as rec, recorded
// as rp; the live request live, read as liveShape) as a later request of the
// test reads it: earlierOf, with the header values the comparison excuses
// (withExcused). Its path and query are read without keploy.yml's url noise:
// what a pattern masks is still a value the app sent there, and a later call
// that carries it carries a value that flowed.
//
// recorded is m with its recorded answer (one the agent parked on disk read
// back), or nil to read it back here: read as empty, the values it held would
// be no earlier place of the test. ok is false when it cannot be read back:
// the exchange is then not kept leaf by leaf, and a later request reads m as
// a raw mock (earlierRaw), which learns nothing when it cannot read it
// either.
func (n learnNoise) servedExchange(m, recorded *models.Mock, rec learn.Shape, rp httpParts, liveShape learn.Shape, live httpParts, served *models.Mock, own bool) (learn.Earlier, bool) {
	if recorded == nil {
		var err error
		if recorded, err = withResponse(m); err != nil {
			return learn.Earlier{}, false
		}
	}
	if len(n.url) > 0 {
		unmasked := n
		unmasked.url = nil
		rec, liveShape = unmasked.shape(rp, destOf(live, rp)), unmasked.shape(live, sameDest)
	}
	return withExcused(earlierOf(recorded, rec, liveShape, served, own), n, live, rp), true
}

// earlierOf is the exchange F just served from m (own: one of the test's own
// mocks), as a later request of the test reads it: each recorded leaf of its
// request with the live value, and each recorded leaf of its answer with the
// value served, each with its variable place; and m's payload
// (learn.Earlier.Payload), whose answer's places the learn step's offset rule
// reads against another recording's of the call.
func earlierOf(m *models.Mock, rec, live learn.Shape, served *models.Mock, own bool) learn.Earlier {
	e := learn.Earlier{Mock: m.Name, At: m.Spec.ReqTimestampMock, Own: own, Payload: learn.RawOf(m, true)}
	jsonReq := rec.Fixed["body"] == "json"
	for p, r := range rec.Leaves {
		place, _ := learn.VarPlace(r, jsonReq)
		e.Seen = append(e.Seen, learn.Seen{Side: learn.Request, Leaf: r, Live: live.Leaves[p].Value, Place: place})
	}
	// The parts of the request compared whole (a text or XML body, a
	// repeated query parameter, a file part), and its JSON body's object
	// keys, are no leaves, and no place a variable binds at: a later value
	// holding a word of one is accounted against it all the same.
	if k, ok := learn.KeysLeaf("body", rec.Body); ok {
		lk, _ := learn.KeysLeaf("body", live.Body)
		e.Seen = append(e.Seen, learn.Seen{Side: learn.Request, Leaf: k, Live: lk.Value})
	}
	// And the names of its parameters: query parameters, headers, form and
	// multipart fields ("qty[<id>]=1" of a Rails or PHP form).
	if names := paramNames(rec); names != "" {
		e.Seen = append(e.Seen, learn.Seen{Side: learn.Request, Leaf: learn.Leaf{Path: "request{names}", Type: learn.Keys, Value: names, Unnamed: true}, Live: paramNames(live)})
	}
	for _, name := range sortedFixed(rec.Fixed) {
		if r, ok := fixedText(name, rec.Fixed[name]); ok {
			lv, _ := fixedText(name, live.Fixed[name])
			e.Seen = append(e.Seen, learn.Seen{Side: learn.Request, Leaf: learn.Leaf{Path: name, Noise: name, Type: learn.String, Value: r}, Live: lv})
		}
	}
	sent := map[string]string{}
	if served != nil {
		for _, l := range respLeaves(served.Spec.HTTPResp) {
			sent[l.Path] = l.Value
		}
	}
	for _, r := range respLeaves(m.Spec.HTTPResp) {
		// respLeaves names body scalars only of a JSON answer.
		place, _ := learn.VarPlace(r, true)
		// Its Date header, within the span the exchange was recorded in and
		// served as recorded, is its server's clock read as it answered: no
		// value came from it (learn.AnswerStamp).
		stamp := r.Path == "header.date" && sent[r.Path] == r.Value && learn.AnswerStamp(r.Value, m.Spec.ReqTimestampMock, m.Spec.ResTimestampMock)
		e.Seen = append(e.Seen, learn.Seen{Side: learn.Response, Leaf: r, Live: sent[r.Path], Place: place, Stamp: stamp})
	}
	sort.Slice(e.Seen, func(i, j int) bool {
		if e.Seen[i].Side != e.Seen[j].Side {
			return e.Seen[i].Side == learn.Request
		}
		return e.Seen[i].Leaf.Path < e.Seen[j].Leaf.Path
	})
	return e
}

// paramNames are the names of a request's parameters, as its shape reads
// them, one per line in order: its query parameters, its headers (those it
// compares), and its form or multipart fields and file parts.
func paramNames(s learn.Shape) string {
	names := map[string]bool{}
	body := s.Fixed["body"] == "form" || s.Fixed["body"] == "multipart"
	add := func(path string) {
		if n, ok := strings.CutPrefix(path, "url.query."); ok {
			names[n] = true
		} else if n, ok := strings.CutPrefix(path, "header."); ok {
			names[n] = true
		} else if n, ok := strings.CutPrefix(path, "body."); ok && body {
			names[n] = true
		}
	}
	for p := range s.Leaves {
		add(p)
	}
	for p := range s.Fixed {
		add(p)
	}
	list := make([]string, 0, len(names))
	for n := range names {
		list = append(list, n)
	}
	sort.Strings(list)
	return strings.Join(list, "\n")
}

// sortedFixed are the names of a shape's fixed parts, in order.
func sortedFixed(fixed map[string]string) []string {
	out := make([]string, 0, len(fixed))
	for k := range fixed {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// fixedText is the text a fixed part of a request's shape (shape) carries,
// and whether it carries one the app may have made: a body read as text, a
// repeated query parameter's values, a file part's name and bytes; not the
// media type or how the body was read.
func fixedText(name, v string) (string, bool) {
	switch {
	case name == "body":
		if t, ok := strings.CutPrefix(v, "text "); ok {
			return t, true
		}
		return "", false
	case strings.HasPrefix(name, "url.query."):
		return strings.ReplaceAll(v, "\x00", "\n"), true
	case strings.HasPrefix(name, "body."):
		if t, ok := strings.CutPrefix(v, "file "); ok {
			return strings.Replace(t, "\x00", "\n", 1), true
		}
	}
	return "", false
}

// withExcused adds to an exchange F served the header values its comparison
// excuses (the static list, keploy.yml's header noise): never compared, they
// are still places a value the app made can be sent first (a request id it
// mints and carries on), and a later call that carries it uses it.
func withExcused(e learn.Earlier, n learnNoise, live, rec httpParts) learn.Earlier {
	for k, v := range rec.header {
		if !n.headers[k] {
			continue
		}
		lv, ok := live.header[k]
		if !ok {
			continue
		}
		path := "header." + k
		place, _ := learn.VarPlace(learn.Leaf{Path: path}, false)
		e.Seen = append(e.Seen, learn.Seen{Side: learn.Request, Leaf: learn.Leaf{Path: path, Noise: path, Type: learn.String, Value: v}, Live: lv, Place: place})
	}
	// The request's places first, as earlierOf orders them.
	sort.SliceStable(e.Seen, func(i, j int) bool {
		if e.Seen[i].Side != e.Seen[j].Side {
			return e.Seen[i].Side == learn.Request
		}
		return e.Seen[i].Leaf.Path < e.Seen[j].Leaf.Path
	})
	return e
}

// learnedMatch serves, under the proven switch outside record's full round, a
// request whose first exact match, modulo what is known, carries noise the
// learn step learned: the replays after F honour what F learned, in every
// part of the request (its path, query, headers and body). The test's own
// mocks come first, in recorded order, as in F; a request recorded several
// times answers with each recorded answer in turn (cursorPick). A request
// whose first exact match learned nothing, or that matches none, goes on to
// the matcher as before, and so does every recording that declares
// variables: the matcher's variable pass serves it (variables.go).
func (h *HTTP) learnedMatch(ctx context.Context, input *req, mockDb integrations.MockMemDb, n learnNoise, opts models.OutgoingOptions) (*models.Mock, error) {
	live := liveParts(ctx, input)
	liveShape := n.shape(live, sameDest)
	for {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		sc, err := h.learnScope(mockDb, learn.TrackerFrom(ctx), live, liveShape, n, true)
		if err != nil {
			return nil, nil
		}
		group := exactGroup(liveShape, sc, false)
		if len(group) == 0 {
			return nil, nil
		}
		hit := group[0]
		if len(hit.Spec.ReqBodyNoise) == 0 {
			return nil, nil
		}
		picked, commit, answered := h.cursorPick(hit, group, sc.owner(), mockDb, !opts.DisableStatefulMocks)
		if answered != nil {
			// Each recording the walk holds has answered: the strict pass
			// decides the call, and misses it the same way.
			return nil, nil
		}
		if !sigV4PartsHold(recordedParts(picked), live) {
			// Noise learned on a SigV4 Authorization is its signature's:
			// the strict matcher compares the rest by its parts (founder
			// decision 25), so this call is its to decide.
			return nil, nil
		}
		served, claimed, err := h.claim(context.WithValue(ctx, ownedKey{}, sc.owner()), picked, mockDb, nil, nil)
		if err != nil {
			return nil, err
		}
		if claimed {
			if commit != nil {
				commit()
			}
			return served, nil
		}
	}
}

// sigV4PartsHold reports whether a live request holds the AWS SigV4 parts of
// the recorded one the strict matcher compares before learned noise
// (founder decision 25, as ruled on SF-3): of a SigV4 Authorization
// (sigV4AuthorizationDiffs: its region, service, terminator and signed
// headers; never its access key id, its date or its signature), and of a
// presigned URL's
// X-Amz-Credential (presignedDiffs). A request recorded with neither holds.
// Noise learned at either place is its signature's and signing time's, and
// excuses none of these.
func sigV4PartsHold(rec, live httpParts) bool {
	if rv, ok := rec.header["authorization"]; ok {
		if _, sigV4 := parseSigV4Authorization(rv); sigV4 {
			diffs, ok := sigV4AuthorizationDiffs(rv, live.header["authorization"])
			if !ok || len(diffs) > 0 {
				return false
			}
		}
	}
	if _, presigned := parsePresignedSigV4(url.Values(rec.query)); presigned {
		byGrammar, diffs := presignedDiffs(url.Values(rec.query), url.Values(live.query))
		if byGrammar == nil || len(diffs) > 0 {
			return false
		}
	}
	return true
}

// diag is the miss as the mismatch report reads a matcher's: the candidates
// the learn step compared, and the one it refused or found closest.
func (m *learnMiss) diag() *matchDiag {
	d := &matchDiag{phase: models.MatchPhaseExhausted, candidates: len(m.pool), pool: m.pool}
	if len(m.pool) == 0 {
		d.phase = models.MatchPhaseNoMocks
	}
	if m.closest != nil {
		d.schemaMatched = []*models.Mock{m.closest}
	}
	d.answered = m.answered
	return d
}

// explain writes the learn step's reason onto the miss's report: its code,
// for judging, and what differed, for the person who reads it. Neither holds
// a value.
func (m *learnMiss) explain(r *models.MockMismatchReport) {
	if r == nil || m.answered != nil {
		return // a stateful walk's miss: the report says so (answeredSequence)
	}
	if m.moved {
		// The replay moved the round under the call: say where it was
		// read and what moved, as the Mongo and Postgres learn steps'
		// misses do. No reason: nothing of the request was compared.
		why := m.res.Why + ": keploy learns nothing of a call in a round it was not made in, and pairs it with none of that round's recordings. " +
			"If the application makes this call at a test's start or end, make the test, or the application's readiness, wait for it, then record again."
		r.NextSteps = strings.TrimSpace(why + " " + r.NextSteps)
		return
	}
	r.Reason = m.res.Reason
	why := "keploy's learn step: " + m.res.Why + "."
	switch m.res.Reason {
	case models.MissUnstableOrder:
		why += " The app sent the array in another order than it was recorded in (unstable_order)."
	case models.MissRequestChanged:
		why += " The request changed since it was recorded in a way keploy does not learn (request_changed)."
	}
	if m.steps != "" {
		r.NextSteps = why + " " + m.steps
		return
	}
	if r.NextSteps == "" {
		r.NextSteps = why
		return
	}
	r.NextSteps = why + " " + r.NextSteps
}
