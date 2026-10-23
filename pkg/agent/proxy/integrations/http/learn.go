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

func init() {
	// What a set's mocks learned is said leaf by leaf (learn.Legible).
	learn.RegisterLeafReader(models.HTTP, recordedLeaves)
	// The learn step learns an HTTP leaf by its words (learnPlan): one an
	// HTTP mock holds whole was learned before it did.
	learn.RegisterWordsKind(models.HTTP)
}

// recordedLeaves are an HTTP mock's recorded leaves by their exact place, as
// the learn step reads them, every header a leaf; none for a mock with no
// request.
func recordedLeaves(m *models.Mock) map[string]learn.Leaf {
	if m == nil || m.Spec.HTTPReq == nil {
		return nil
	}
	return learnNoise{headers: map[string]bool{}}.shape(recordedParts(m), sameDest).Leaves
}

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
			if len(vs) == 1 {
				add("body."+k, vs[0], strings.ContainsAny(k, ".[]"))
				continue
			}
			// A repeated key's values come in any order (formDiffs reads
			// them as members), so no one of them has a place a learned
			// entry could name; and joined, they would read as one value
			// holding ", ", whose words would admit one value sent as two:
			// compared whole, as a repeated query parameter is. (form is
			// this call's own parse: its values are sorted in place.)
			sort.Strings(vs)
			s.Fixed["body."+k] = strings.Join(vs, "\x00")
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
// field part is a leaf (body.<name>), each file part a fixed part (its file
// name and bytes), so a boundary minted per request is no difference. It
// reports false for a body it cannot read.
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
		if part.FileName() != "" {
			fixed["body."+name] = "file " + part.FileName() + "\x00" + string(data)
			continue
		}
		path := "body." + name
		leaves[path] = learn.Leaf{Path: path, Noise: path, Type: learn.String, Value: string(data), Unnamed: strings.ContainsAny(name, ".[]")}
	}
}

// known is what a comparison with the recorded mock m excuses: keploy.yml's
// noise, the noise learned onto m, and m's obfuscated values (Mock.Noise).
//
// What the learn step learned of a leaf's words (MockSpec.LearnedWords) it
// excuses by the live value alone (httpKnown.ExcusedLive): the leaf's slots
// may change, and every other byte of it is compared.
func (n learnNoise) known(m *models.Mock) learn.Known {
	return httpKnown{n: n, learned: m.Spec.ReqBodyNoise, words: m.Spec.LearnedWords, values: util.NewNoiseChecker(m.Noise)}
}

// knownAsRecorded is known without the noise learned onto m: the recording
// as the learn step first compared it, for a spent mock (learn.Candidate).
func (n learnNoise) knownAsRecorded(m *models.Mock) learn.Known {
	return httpKnown{n: n, values: util.NewNoiseChecker(m.Noise)}
}

type httpKnown struct {
	n       learnNoise
	learned map[string][]string
	// words are the words the learn step learned of leaves, by exact place
	// (MockSpec.LearnedWords): never excused by Excused, which sees one
	// value; ExcusedLive reads both.
	words  map[string]models.WordsEntry
	values *util.NoiseChecker
}

// ExcusedLive implements learn.KnownLive: a drifted leaf whose words the
// learn step learned is not compared by value where the live value is
// admitted by them (learn.WordsAdmit: its slots changed, nothing else), its
// type kept. Any other leaf is compared.
func (k httpKnown) ExcusedLive(rec, live learn.Leaf) (bool, bool) {
	e, ok := k.words[rec.Path]
	if !ok {
		return false, false
	}
	admitted, _ := learn.WordsAdmit(rec.Value, live.Value, e)
	return admitted, admitted
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
			// The claim reads the scope's owners (ownedKey), as the
			// matcher's does: a stated recording no open test owns is a
			// reusable one, kept and served in turn (cursorPick), never
			// used up.
			out, claimed, err := h.claim(context.WithValue(ctx, ownedKey{}, sc.owner()), picked, mockDb, nil, nil)
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
			// A call's clock reads are read in its own window too, a test's
			// own call included (r9 §3).
			w := tracker.CallWindowFrom(prev.At(), m, input.arrived)
			learned := learn.ClassifyIn(tracker.Drifts(), res.Diff.Drift, liveShape, c.Shape, m.Spec.ReqTimestampMock, earlier, raw, respLeaves(answered.Spec.HTTPResp), w)
			plan, why := learnPlan(tracker, test, answered, c.Shape, own, learned, earlier, givenBindings(mockDb), w)
			if why != "" {
				res.Outcome, res.Reason, res.Why, res.Refused = learn.Miss, models.MissRequestChanged, why, plan.refused
				return nil, &learnMiss{res: res, closest: m, pool: sc.http}, nil
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
					zap.String("mock", m.Name), zap.String("field", l.Recorded.Path), zap.String("as", plan.learnedAs(l)))
			}
			if e, ok := n.servedExchange(m, answered, c.Shape, recordedParts(m), liveShape, live, out, own); ok {
				// A variable a later call binds at a place of this one
				// learns its words in this call's window.
				e.Window = w
				tracker.Served(test, e)
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

// pairing is what the learn step learned on a paired mock: the words it
// learned of the leaves that drifted as noise (by exact place; never a leaf
// whole, design r9 §1), its variables (an echo and a call-local variable
// included, each with its words), the variables of the test bound at an
// earlier mock that it uses (binds, by that mock), each variable's value in
// this call, for its answer, and record's full round's value at each place
// whose words it learned (f, by mock, then place: learned.local.yaml). Of a
// request it refuses, refused names the word-level rule that refused it, if
// one did (models.RefusedTie, RefusedContainment): the miss's report says it
// (models.UnmatchedCall.Refused).
type pairing struct {
	words   map[string]models.WordsEntry
	vars    models.MockVariables
	binds   map[string]models.MockVariables
	values  map[string]variables.Binding
	f       map[string]map[string]string
	refused string
}

// learnedAs is what the plan learned the drifted leaf l as, as the learn
// step's log says it: "noise" when its words are noise, "echo" or
// "variable" when it is a variable's place (a leaf Classify found noise
// becomes one when another place of the call holds its value whole, r9 §2),
// and else what Classify found it (l.Kind).
func (p pairing) learnedAs(l learn.Learned) string {
	// (A place whose words are noise is one Classify found noise: l.Kind.)
	path := l.Recorded.Path
	at := func(vars models.MockVariables) string {
		names := make([]string, 0, len(vars))
		for n := range vars {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			v := vars[n]
			for _, place := range append([]string{v.Bind}, v.Use...) {
				if lp, ok := learn.LeafPath(place); ok && lp == path {
					if v.Echo {
						return string(learn.KindEcho)
					}
					return string(learn.KindVariable)
				}
			}
		}
		return ""
	}
	// This call's variables: an echo bound here, and every variable used
	// here (p.binds are bound at earlier mocks, at their places).
	if as := at(p.vars); as != "" {
		return as
	}
	return string(l.Kind)
}

// slotPlace is a place of the call the learn step learned words at, as the
// ties between places read it (tiePlaces): noise, an echo, a variable used
// here.
type slotPlace struct {
	l    learn.Learned
	kind learn.Kind
	// name is the variable at the place ("" for noise).
	name string
}

// learnPlan turns what Classify learned about mock m (paired as rec; own: one
// of the test's own mocks) into what is written onto the recording, or says
// why the request cannot be answered (request_changed). Of a leaf that is
// noise, only the words that changed are learned (learn.DriftCache.Words), and
// every other word of it is still compared; a leaf whose words cannot be
// learned is refused, never excused whole. A value never becomes noise when it
// flowed from an earlier place of the test (a variable) or comes back in the
// answer (an echo): when keploy cannot follow it as a variable, the request is
// refused instead. w is the call's window its clock reads were made in (nil:
// by the lead alone).
//
//   - A variable is bound at its earlier place, a request place of one of
//     the test's own HTTP mocks F served, which held the value whole, and is
//     used here: it holds when the live value here is the one F saw there.
//     Its words are learned at its bind, of its recorded value and F's there,
//     in the window of the call that sent it there (learn.Place.Window).
//   - An echo is a variable of this call alone (echo: true), bound here and
//     answered where the recorded answer holds the value whole, its words
//     learned of R and F. Leaves of this call that drifted from the same
//     recorded value are one variable.
//   - A value the test case's request gave (given: its template keys, which
//     keploy test binds for the test, #4789) is that key's variable, used
//     here under the key's name with no bind, whatever else Classify found:
//     the test case is earlier than any call it makes. Its name is the key's
//     alone in the test: no other variable takes it.
//
// Then the ties between the places whose words were learned (tiePlaces).
func learnPlan(tracker *learn.Tracker, test uint64, m *models.Mock, rec learn.Shape, own bool, learned []learn.Learned, earlier []learn.Earlier, given []variables.Binding, w *learn.Window) (pairing, string) {
	p := pairing{words: map[string]models.WordsEntry{}, vars: models.MockVariables{}, binds: map[string]models.MockVariables{}, values: map[string]variables.Binding{}, f: map[string]map[string]string{}}
	dc := tracker.Drifts()
	jsonReq := rec.Fixed["body"] == "json"
	reserved := make(map[string]bool, len(given))
	for _, g := range given {
		reserved[g.Name] = true
	}
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
	sawF := func(mock, place, live string) {
		if p.f[mock] == nil {
			p.f[mock] = map[string]string{}
		}
		p.f[mock][place] = live
	}
	var slots []slotPlace
	echoed := map[string]string{} // recorded value -> its echo variable
	for _, l := range learned {
		r := l.Recorded
		if g, ok := givenFor(r, l.Live, given); ok {
			// The test case gave this value: a use of its key, no bind.
			place, placeable := learn.VarPlace(r, jsonReq)
			switch {
			case !placeable:
				return p, r.Path + " carries the test case's " + g.Name + " at a place keploy cannot follow a variable to (a form or multipart field)"
			case !own:
				return p, r.Path + " carries the test case's " + g.Name + ", and only a test's own calls carry its variables"
			}
			v := p.vars[g.Name]
			v.Recorded, v.Use = g.Recorded, appendNew(v.Use, place)
			v.Answer = appendNew(v.Answer, answers(l, respLeavesOf(m))...)
			p.vars[g.Name] = v
			p.values[g.Name] = g
			continue
		}
		switch l.Kind {
		case learn.KindNoise:
			e, why := dc.Words(r.Value, l.Live.Value, w)
			if why != "" {
				p.refused = learn.RefusalClass(why)
				return p, r.Path + " changed, and keploy cannot tell which of its words changed alone: " + why
			}
			p.words[r.Path] = e
			sawF(m.Name, r.Path, l.Live.Value)
			slots = append(slots, slotPlace{l: l, kind: learn.KindNoise})

		case learn.KindVariable:
			from := l.From
			switch {
			case from.Side == "":
				return p, r.Path + " carries a value the test's recording holds earlier (in " + from.Mock + "), where keploy cannot bind it yet: a call of another protocol, or inside its payload"
			case from.Side != learn.Request:
				return p, r.Path + " carries a value " + from.Mock + " answered earlier in the test, which keploy cannot follow from an answer"
			case !from.Whole:
				// Tied to the earlier place's value in part: a partial tie, or
				// a changed piece glued inside it (B1).
				p.refused = models.RefusedTie
				if from.Glued {
					p.refused = models.RefusedContainment
				}
				return p, r.Path + " carries a value " + from.Mock + " sent earlier in the test at " + from.Path + ", inside a longer value or at a place keploy cannot bind it"
			case from.Var == "":
				return p, r.Path + " carries a value " + from.Mock + " sent earlier in the test at " + from.Path + ", inside a longer value or at a place keploy cannot bind it"
			case !own || !from.Own:
				return p, r.Path + " carries a value " + from.Mock + " sent earlier, and only a test's own calls carry its variables"
			}
			use, ok := learn.VarPlace(r, jsonReq)
			if !ok {
				return p, r.Path + " carries a value " + from.Mock + " sent earlier in the test, at a place keploy cannot follow a variable to (a form or multipart field)"
			}
			if !variables.SameValue(from.Live, l.Live.Value, r.Type == learn.Number) {
				return p, r.Path + " does not hold the value the test sent at " + from.Var + " of " + from.Mock + ", which it held when recorded"
			}
			// Its words, learned where it is bound: of its recorded value and
			// what F saw there, in the window of the call that sent it there
			// (Place.Window), never this one's: its value was read for that
			// call.
			words, why := dc.Words(r.Value, from.Live, from.Window)
			if why != "" {
				p.refused = learn.RefusalClass(why)
				return p, r.Path + " carries a value " + from.Mock + " sent earlier at " + from.Var + ", and keploy cannot tell which of its words changed alone there: " + why
			}
			name, ok := tracker.Name(test, from.Mock, from.Var, reserved)
			if !ok {
				return p, "the test ended before " + r.Path + " was learned"
			}
			v := p.vars[name]
			v.Recorded, v.Use = r.Value, appendNew(v.Use, use)
			v.Answer = appendNew(v.Answer, answers(l, respLeavesOf(m))...)
			p.vars[name] = v
			b := p.binds[from.Mock]
			if b == nil {
				b = models.MockVariables{}
				p.binds[from.Mock] = b
			}
			bv := b[name]
			bv.Recorded, bv.Bind = r.Value, from.Var
			// F's clock evidence, read where the variable is bound: a clock
			// read that moved by the lead there may share its live value with
			// another one read in the same second in a replay
			// (models.MockVariable.Clock).
			bv.Clock = learn.ClockReads(r.Value, from.Live)
			bv.Words = &words
			bv.Answer = appendNew(bv.Answer, earlierAnswers(earlier, from.Mock, r)...)
			b[name] = bv
			p.values[name] = variables.Binding{Name: name, Value: from.Live, Recorded: r.Value, Mock: from.Mock, Place: from.Var}
			if path, ok := learn.LeafPath(from.Var); ok {
				sawF(from.Mock, path, from.Live)
			}
			slots = append(slots, slotPlace{l: l, kind: learn.KindVariable, name: name})

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
				slots = append(slots, slotPlace{l: l, kind: learn.KindEcho, name: name})
				continue
			}
			words, why := dc.Words(r.Value, l.Live.Value, w)
			if why != "" {
				p.refused = learn.RefusalClass(why)
				return p, r.Path + " is echoed in the answer, and keploy cannot tell which of its words changed alone: " + why
			}
			name, ok := tracker.Name(test, m.Name, place, reserved)
			if !ok {
				return p, "the test ended before " + r.Path + " was learned"
			}
			echoed[r.Value] = name
			p.vars[name] = models.MockVariable{Recorded: r.Value, Bind: place, Answer: at, Echo: true, Words: &words}
			p.values[name] = variables.Binding{Name: name, Value: l.Live.Value, Recorded: r.Value, Mock: m.Name, Place: place}
			sawF(m.Name, r.Path, l.Live.Value)
			slots = append(slots, slotPlace{l: l, kind: learn.KindEcho, name: name})
		}
	}
	if why := tiePlaces(tracker, test, m, &p, slots, given, jsonReq, reserved, w); why != "" {
		return p, why
	}
	return p, ""
}

// tiePlaces reads the ties between the places learnPlan learned words at
// (slots: noise, echoes, variables used here), and between each and the test
// case's values (given), design r9 §2, the instant check first
// (learn.DriftCache.TieOf):
//
//   - two places whose every changed word lies inside an accepted clock read
//     other than a UUID's are exempt, and never held equal (N2);
//   - two places of noise that hold one recorded value whole, and one live
//     value (twins), are one variable of this call alone (echo: true, no
//     answer), bound at the first place in sorted order and used at the
//     others, with the first's words (a UUID in a path and in the body); two
//     that F saw hold two values are two values that were equal when
//     recorded, each learned apart, as a value recorded equal to a test
//     case's is not its;
//   - anything else that ties two places is refused (N8): a partial tie, a
//     value glued inside another, a test case's value inside a place.
//
// It changes p for a call-local variable: the places' noise words go, and
// the variable takes the first's.
func tiePlaces(tracker *learn.Tracker, test uint64, m *models.Mock, p *pairing, slots []slotPlace, given []variables.Binding, jsonReq bool, reserved map[string]bool, w *learn.Window) string {
	dc := tracker.Drifts()
	sort.SliceStable(slots, func(i, j int) bool { return slots[i].l.Recorded.Path < slots[j].l.Recorded.Path })
	value := func(s slotPlace) [2]string { return [2]string{s.l.Recorded.Value, s.l.Live.Value} }
	// group is, of each place of noise tied whole to an earlier one in
	// sorted order, that first place's index.
	group := make([]int, len(slots))
	for i := range group {
		group[i] = i
	}
	for i := range slots {
		a := slots[i]
		for _, g := range given {
			// A value recorded equal to the key's, and another now, is a
			// value of its own (two counters both at 1 when recorded are two
			// values): no tie. One that carries the key's inside it is.
			if dc.TieOf([2]string{g.Recorded, g.Value}, value(a), w) == learn.TiePartial {
				p.refused = dc.TieClass([2]string{g.Recorded, g.Value}, value(a), w)
				return a.l.Recorded.Path + " carries the test case's " + g.Name + " inside it, with more beside it that changed, which keploy cannot follow (N8)"
			}
		}
		for j := i + 1; j < len(slots); j++ {
			b := slots[j]
			switch dc.TieOf(value(a), value(b), w) {
			case learn.TieExempt, learn.TieNone:
				continue
			case learn.TiePartial:
				p.refused = dc.TieClass(value(a), value(b), w)
				return a.l.Recorded.Path + " and " + b.l.Recorded.Path + " changed together, the one carried inside the other or glued to it, which keploy cannot follow as one value (N8)"
			}
			// One recorded value, whole, at both places: one value of the
			// call when F saw one value at both (twins), two values that were
			// equal when recorded otherwise, each learned apart.
			if !variables.SameValue(a.l.Live.Value, b.l.Live.Value, a.l.Recorded.Type == learn.Number) {
				continue
			}
			switch {
			case a.kind == learn.KindNoise && b.kind == learn.KindNoise:
				if group[j] == j {
					group[j] = group[i]
				}
			case a.name != "" && a.name == b.name:
				// One variable already.
			default:
				p.refused = models.RefusedTie
				return a.l.Recorded.Path + " and " + b.l.Recorded.Path + " held one value when recorded and now, and keploy learned them apart, so it cannot follow them as one value"
			}
		}
	}
	// Each group of places of noise that hold one value: one variable of
	// this call alone, bound at its first place and used at the others.
	members := map[int][]int{}
	for i, g := range group {
		if g != i {
			members[g] = append(members[g], i)
		}
	}
	firsts := make([]int, 0, len(members))
	for g := range members {
		firsts = append(firsts, g)
	}
	sort.Ints(firsts)
	for _, g := range firsts {
		first := slots[g].l
		bind, ok := learn.VarPlace(first.Recorded, jsonReq)
		if !ok {
			p.refused = models.RefusedTie
			return first.Recorded.Path + " holds one value with another place of the call, at a place keploy cannot bind it (a form or multipart field)"
		}
		// A group's places are noise, whose words learnPlan learned.
		words := p.words[first.Recorded.Path]
		v := models.MockVariable{Recorded: first.Recorded.Value, Bind: bind, Echo: true, Words: &words}
		for _, i := range members[g] {
			use, ok := learn.VarPlace(slots[i].l.Recorded, jsonReq)
			if !ok {
				p.refused = models.RefusedTie
				return slots[i].l.Recorded.Path + " holds one value with " + first.Recorded.Path + ", at a place keploy cannot follow a variable to (a form or multipart field)"
			}
			v.Use = appendNew(v.Use, use)
			delete(p.words, slots[i].l.Recorded.Path)
			delete(p.f[m.Name], slots[i].l.Recorded.Path)
		}
		delete(p.words, first.Recorded.Path)
		name, ok := tracker.Name(test, m.Name, bind, reserved)
		if !ok {
			return "the test ended before " + first.Recorded.Path + " was learned"
		}
		p.vars[name] = v
		p.values[name] = variables.Binding{Name: name, Value: first.Live.Value, Recorded: first.Recorded.Value, Mock: m.Name, Place: bind}
	}
	return ""
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
	for _, b := range table.Snapshot() {
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
	for _, g := range given {
		if r.Value != "" && variables.SameValue(g.Recorded, r.Value, number) && variables.SameValue(g.Value, live.Value, number) {
			return g, true
		}
	}
	return variables.Binding{}, false
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
		// The words learned as noise ride out as noise did, on the consumed
		// copy's spec (MockSpec.LearnedWords), with F's values beside them
		// for the set's learned.local.yaml.
		if len(p.words) > 0 {
			spec.LearnedWords = models.MergeWords(spec.LearnedWords, p.words)
		}
		spec.SetLearnedF(models.MergeLearnedF(spec.LearnedF(), p.f))
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
		if len(hit.Spec.ReqBodyNoise) == 0 && len(hit.Spec.LearnedWords) == 0 {
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
	r.Reason, r.Refused = m.res.Reason, m.res.Refused
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
