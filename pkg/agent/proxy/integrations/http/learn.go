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

// learnMiss is a request the learn step left unanswered.
type learnMiss struct {
	res     learn.Result
	closest *models.Mock
	pool    []*models.Mock
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
	var sc scope
	perTest, err := mockDb.GetPerTestMocksInWindow()
	if err != nil {
		utils.LogError(h.Logger, err, "failed to get per-test mocks")
		return sc, errors.New("error while matching the request with the mocks")
	}
	session, err := mockDb.GetSessionMocks()
	if err != nil {
		utils.LogError(h.Logger, err, "failed to get session mocks")
		return sc, errors.New("error while matching the request with the mocks")
	}
	sc.visible = append(append(make([]*models.Mock, 0, len(perTest)+len(session)), perTest...), session...)
	httpPerTest, httpSession := inRecordedOrder(FilterHTTPMocks(perTest)), inRecordedOrder(FilterHTTPMocks(session))
	sc.http = append(append(make([]*models.Mock, 0, len(httpPerTest)+len(httpSession)), httpPerTest...), httpSession...)

	open := tracker.Open()
	inScope := map[string]bool{}
	owner, _ := mockDb.(integrations.TestOwnership)
	if integrations.ExactServing(mockDb) {
		// Under exact serving the store alone says what the open test owns:
		// what its per-test tier shows, as on the bare manager. A worker's
		// view made before exact serving began passes straight through, and
		// its allowlist is the scope it was made for, not the open test's.
		owner = nil
	}
	windows, _ := mockDb.(integrations.TestWindows)
	inFlight, _ := mockDb.(integrations.InFlightOwner)
	pools, _ := mockDb.(integrations.PoolMembership)
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
		own, poolsOwn := ownership(m, i < len(httpPerTest), owner, windows, inFlight, pools)
		// A protocol-phase mock (a handshake, a token fetch) is never a
		// test's own: the pool's, answering any number of the test's
		// requests (design §5.2), and never paired.
		own = own && open && !m.IsProtocolPhase()
		c := learn.Candidate{
			Shape:    n.shape(rp, d),
			Known:    n.known(m),
			Pairable: !m.IsProtocolPhase() && (own || pool && poolsOwn),
		}
		inScope[m.Name] = true
		if own && tracker.Answered(m.Name) {
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
		for _, m := range tracker.AnsweredMocks() {
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
	return sc, nil
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
// values: every mock any parser served earlier in the test, and every mock
// recorded before it that the scope shows, that was served before the test
// began, or that the store stages, served or not (visible: a start-up call
// a kept-alive app never makes again in F is one). Each is read once. The
// paired mock is not one, and neither is an exchange the learn step served
// (earlier): its leaves and fixed parts, with what F saw at each, are read
// instead (earlierOf). dc keeps each mock's text for the test, read with its
// answer (DriftCache.ReadRaw). err names one whose answer the agent parked on
// disk and that cannot be read back, and why; no mock is then returned, since
// what it held cannot be told.
func earlierRaw(dc *learn.DriftCache, paired *models.Mock, served, visible []*models.Mock, recorded func(visit func(*models.Mock) bool) error, earlier []learn.Earlier, prev *learn.PreviousCall) (out []learn.Raw, err error) {
	leafed := make(map[string]bool, len(earlier)+1)
	leafed[paired.Name] = true
	for _, e := range earlier {
		leafed[e.Mock] = true
	}
	read := map[string]bool{}
	once := func(m *models.Mock) bool {
		id := m.Name + "\x00" + string(m.Kind) + "\x00" + m.Spec.ReqTimestampMock.String()
		if read[id] {
			return false
		}
		read[id] = true
		return true
	}
	add := func(m *models.Mock, served bool) error {
		r, err := dc.ReadRaw(m, served)
		if err != nil {
			return fmt.Errorf("read back the recorded answer of %q: %w", m.Name, err)
		}
		out = append(out, r)
		return nil
	}
	for _, m := range served {
		prev.See(m)
		if m != nil && !leafed[m.Name] && once(m) {
			if err := add(m, true); err != nil {
				return nil, err
			}
		}
	}
	for _, m := range visible {
		if m != nil && !leafed[m.Name] && m.Spec.ReqTimestampMock.Before(paired.Spec.ReqTimestampMock) && once(m) {
			if err := add(m, false); err != nil {
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
		if m == nil || leafed[m.Name] || !m.Spec.ReqTimestampMock.Before(paired.Spec.ReqTimestampMock) || !once(m) {
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
	// misses instead.
	unclaimed := map[*models.Mock]uint64{}
	stuck := func(m *models.Mock) bool {
		at, ok := unclaimed[m]
		return ok && storeRevision(mockDb) == at
	}
	refused := func(m *models.Mock, res learn.Result, sc scope) (*models.Mock, *learnMiss, error) {
		h.Logger.Warn("keploy's mock store would not hand out the recording the learn step chose for this call; learning nothing from it",
			zap.String("mock", m.Name))
		res.Outcome, res.Reason, res.Why = learn.Miss, "", "keploy's mock store would not hand out "+m.Name+", which it still shows, so keploy cannot answer from it"
		return nil, &learnMiss{res: res, closest: m, pool: sc.http}, nil
	}
	for {
		if ctx.Err() != nil {
			return nil, nil, ctx.Err()
		}
		rev := storeRevision(mockDb)
		sc, err := h.learnScope(mockDb, tracker, live, liveShape, n, opts.LearnPool)
		if err != nil {
			return nil, nil, err
		}
		// What the test served so far is read after the scope: a mock a
		// concurrent call consumed before the scope was read is gone from
		// the scope, and the mock manager told the tracker of it before it
		// left (MockManager.observeServed), so it is in one of the two.
		test, earlier, served, before := tracker.Test()
		windows, _ := mockDb.(integrations.TestWindows)
		// Under a claim, in a full record (which re-makes the pool), what
		// the claim key's calls made outside every test sent first (a
		// Playwright beforeAll seeding a name) is the key's: an earlier
		// place of the key's tests, followed as a variable of the key. A
		// call outside every test reads only the key's earlier calls.
		keyScope := opts.LearnPool && tracker.Keyed()
		outsideCall := keyScope && !tracker.Open()
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

		res := learn.Pair(liveShape, sc.cands)
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
				picked, c = h.cursorPick(m, exactGroup(liveShape, sc, true), sc.owner(), mockDb, !opts.DisableStatefulMocks)
				if c != nil {
					commit = c
				}
			}
			if picked != m {
				rp := recordedParts(picked)
				rec = n.shape(rp, destOf(live, rp))
			}
			if stuck(picked) {
				return refused(picked, res, sc)
			}
			if own && !tracker.Answer(test, picked) {
				continue
			}
			// A recorded answer of the test's own that holds a value the
			// test made earlier is answered with this run's value, and what
			// that is learned (mintedAnswers).
			var minted pairing
			switch {
			case own:
				minted = h.mintedAnswers(tracker, test, picked, pairing{}, reservedNames(givenBindings(mockDb)), append(tracker.Minted(test), outsideMinted...))
			case outsideCall:
				minted = h.mintedAnswers(tracker, test, picked, pairing{}, nil, outsideMinted)
			}
			var (
				out     *models.Mock
				claimed bool
				why     string
			)
			if len(minted.vars) > 0 {
				out, claimed, why, err = h.claimLearned(ctx, picked, mockDb, minted, sc.owner())
			}
			if len(minted.vars) == 0 || why != "" {
				// The claim reads the scope's owners (ownedKey), as the
				// matcher's does: a stated recording no open test owns is a
				// reusable one, kept and served in turn (cursorPick), never
				// used up.
				out, claimed, err = h.claim(context.WithValue(ctx, ownedKey{}, sc.owner()), picked, mockDb, nil, nil)
			} else if claimed {
				h.logMintedAnswers(picked, minted.vars)
			}
			if err != nil || !claimed {
				if own {
					tracker.Unanswer(test, picked.Name)
				}
				if err != nil {
					return nil, nil, err
				}
				unclaimed[picked] = rev
				continue
			}
			commit()
			if e, ok := n.servedExchange(picked, nil, rec, recordedParts(picked), liveShape, live, out, own); ok {
				tracker.Served(test, e)
				if outsideCall {
					tracker.ServedOutside(e)
				}
			}
			return out, nil, nil

		case learn.Paired:
			m, c, own := sc.mocks[res.Index], sc.cands[res.Index], sc.own[res.Index]
			if stuck(m) {
				return refused(m, res, sc)
			}
			if !own && m.Spec.ReqTimestampMock.IsZero() {
				// A pool mock's earlier places are every recording made
				// before it: of one with no recorded time, none can be
				// told, so where its value came from cannot be either.
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
			raw, err := earlierRaw(tracker.Drifts(), m, served, append(append(ofPairedTest(before, m, windows, own), ofPairedTest(sc.visible, m, windows, own)...), ofPairedTest(staged, m, windows, own)...), recordedBeforeOf(mockDb, m, windows, own), earlier, prev)
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
				res.Outcome, res.Reason, res.Why = learn.Miss, "", "keploy cannot read back the recorded answer of an earlier call of the test, so it cannot tell whether what changed came from there"
				return nil, &learnMiss{res: res, closest: m, pool: sc.http}, nil
			}
			// A pool mock's call need not run on the clock F set for a
			// test (at the start-up and between tests none is set, and a
			// call made in a test was recorded in none): its clock reads
			// are read in its own window (learn.Tracker.CallWindowFrom),
			// as well as by the lead. Its previous recorded call is found
			// among every recording of the set, those on disk too, as the
			// earlier places streamed them.
			var w *learn.Window
			if !own {
				w = tracker.CallWindowFrom(prev.At(), m, input.arrived)
			}
			learned := learn.ClassifyIn(tracker.Drifts(), res.Diff.Drift, liveShape, c.Shape, m.Spec.ReqTimestampMock, earlier, raw, respLeaves(answered.Spec.HTTPResp), w)
			given := givenBindings(mockDb)
			plan, why := learnPlan(tracker, test, answered, c.Shape, own, outsideCall, learned, earlier, given)
			if why != "" {
				res.Outcome, res.Reason, res.Why = learn.Miss, models.MissRequestChanged, why
				return nil, &learnMiss{res: res, closest: m, pool: sc.http}, nil
			}
			switch {
			case own:
				plan = h.mintedAnswers(tracker, test, m, plan, reservedNames(given), append(tracker.Minted(test), outsideMinted...))
			case outsideCall:
				plan = h.mintedAnswers(tracker, test, m, plan, nil, outsideMinted)
			}
			if own && !tracker.Answer(test, m) {
				continue
			}
			out, claimed, why, err := h.claimLearned(ctx, m, mockDb, plan, sc.owner())
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
				unclaimed[m] = rev
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
		miss := &learnMiss{res: res, pool: sc.http}
		if res.Index >= 0 {
			miss.closest = sc.mocks[res.Index]
		}
		return nil, miss, nil
	}
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
			case from.Side == "":
				return p, r.Path + " carries a value the test's recording holds earlier (in " + from.Mock + "), where keploy cannot bind it yet: a call of another protocol, or inside its payload"
			case from.Side != learn.Request:
				return p, r.Path + " carries a value " + from.Mock + " answered earlier in the test, which keploy cannot follow from an answer"
			case !from.Whole || from.Var == "":
				src, w, ok := withinSource(r, l.Live, earlier)
				if !ok {
					src, w, ok = sharedSource(r, l.Live, earlier)
					inside = ok
				}
				if !ok {
					return p, r.Path + " carries a value " + from.Mock + " sent earlier in the test at " + from.Path + ", inside a longer value or at a place keploy cannot bind it"
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
			// (models.MockVariable.Clock).
			bv.Clock = learn.ClockReads(word, from.Live)
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
// echo, into the variable's bind). An answer place a variable of p answers
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
		if v.Outside {
			name, ok = tracker.OutsideName(v.Mock, v.Place)
		} else {
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
// value served, each with its variable place.
func earlierOf(m *models.Mock, rec, live learn.Shape, served *models.Mock, own bool) learn.Earlier {
	e := learn.Earlier{Mock: m.Name, At: m.Spec.ReqTimestampMock, Own: own}
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
		e.Seen = append(e.Seen, learn.Seen{Side: learn.Response, Leaf: r, Live: sent[r.Path], Place: place})
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
		picked, commit := h.cursorPick(hit, group, sc.owner(), mockDb, !opts.DisableStatefulMocks)
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
	return d
}

// explain writes the learn step's reason onto the miss's report: its code,
// for judging, and what differed, for the person who reads it. Neither holds
// a value.
func (m *learnMiss) explain(r *models.MockMismatchReport) {
	if r == nil {
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
	if r.NextSteps == "" {
		r.NextSteps = why
		return
	}
	r.NextSteps = why + " " + r.NextSteps
}
