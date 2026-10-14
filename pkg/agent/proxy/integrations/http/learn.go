package http

import (
	"bytes"
	"context"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"go.keploy.io/server/v3/pkg/agent/containerkey"
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
// number of its path segments. The destination is the container key when
// both the request and the recording name one (row 4k), and otherwise the
// Host the client sent: an HTTP mock does not record its scheme, and a port
// other than the scheme's is in the Host. The learnable leaves are the path
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
	// body is keploy.yml's request-body noise, with the request matcher's
	// semantics (matcher.NoiseLookup).
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
	}
	return n
}

// httpParts are the parts of an HTTP request the learn step reads.
type httpParts struct {
	method    string
	host      string // the Host, lowercased
	container string // the container key, when there is one
	path      string
	query     map[string][]string
	header    map[string]string // by lowercased name, a repeated header's values joined as recorded
	mediaType string
	body      []byte
}

func liveParts(ctx context.Context, in *req) httpParts {
	p := httpParts{method: in.method, path: in.url.Path, query: in.url.Query(), header: make(map[string]string, len(in.header)), body: in.body}
	for k, v := range in.header {
		p.header[strings.ToLower(k)] = strings.Join(v, ",")
	}
	p.host = strings.ToLower(p.header["host"])
	// The container the connection reaches, when it reaches one (row 4k).
	if k, ok := containerkey.FromContext(ctx); ok {
		p.container = k
	}
	p.mediaType = mediaTypeOf(p.header["content-type"])
	return p
}

func recordedParts(m *models.Mock) httpParts {
	r := m.Spec.HTTPReq
	p := httpParts{method: string(r.Method), path: r.URL, header: make(map[string]string, len(r.Header)), body: []byte(r.Body)}
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
	p.container, _ = containerkey.Recorded(m)
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

// sameDestination reports whether a recorded request went where the live one
// goes: to the same container when both name one, to the same Host otherwise.
func sameDestination(live, rec httpParts) bool {
	if live.container != "" && rec.container != "" {
		return live.container == rec.container
	}
	return live.host == rec.host
}

// destOf is the destination in a recorded request's operation key, for the
// live request live.
func destOf(live, rec httpParts) string {
	switch {
	case sameDestination(live, rec):
		return sameDest
	case rec.container != "":
		return rec.container
	}
	return rec.host
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
func (n learnNoise) known(m *models.Mock) learn.Known {
	return httpKnown{n: n, learned: m.Spec.ReqBodyNoise, values: util.NewNoiseChecker(m.Noise)}
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
	for k, v := range r.Header {
		lk := strings.ToLower(k)
		if lk == "content-length" {
			continue
		}
		out = append(out, learn.Leaf{Path: "header." + lk, Noise: "header." + lk, Type: learn.String, Value: v})
	}
	if v, ok := learn.DecodeJSON([]byte(r.Body)); ok {
		body := map[string]learn.Leaf{}
		learn.JSONLeaves("body", v, body)
		for _, l := range body {
			out = append(out, l)
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
	owner, _ := mockDb.(integrations.TestOwnership)
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
		// A test's own mock is pairable while its test is open. The pool's
		// is pairable only in a full record (pool), and a mock is the pool's
		// only when the store can say so: in a view that scopes the pool to
		// a test, a visible mock that test does not own; anywhere else, the
		// session tier. A per-test mock outside its open test (between
		// tests every test's are visible) is never paired.
		perTestTier := i < len(httpPerTest)
		own, poolsOwn := open && perTestTier, !perTestTier
		if owner != nil {
			owned, known := owner.OwnedByTest(m)
			own = open && known && owned
			if known {
				poolsOwn = !owned
			}
		}
		c := learn.Candidate{
			Shape:    n.shape(rp, d),
			Known:    n.known(m),
			Pairable: !m.IsProtocolPhase() && (own || pool && poolsOwn),
		}
		if own {
			sc.cands, sc.mocks, sc.own = append(sc.cands, c), append(sc.mocks, m), append(sc.own, true)
		} else {
			poolCands, poolMocks = append(poolCands, c), append(poolMocks, m)
		}
	}
	sc.cands, sc.mocks = append(sc.cands, poolCands...), append(sc.mocks, poolMocks...)
	for range poolCands {
		sc.own = append(sc.own, false)
	}
	return sc, nil
}

// exactGroup are the candidates that match live exactly, modulo what is
// known: what a request recorded several times answers from, in turn
// (cursorPick), and nothing else.
func exactGroup(live learn.Shape, sc scope) []*models.Mock {
	var out []*models.Mock
	for i, c := range sc.cands {
		if c.Shape.Key != live.Key {
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

// earlierRaw are the mocks of the test's recording that came before a
// request recorded at at, as Classify reads them when no leaf names their
// values: every mock any parser served earlier in the test, and every mock
// the scope shows that was recorded before it. The paired mock is not one,
// and neither is an exchange the learn step served (earlier): its leaves,
// with what F saw at each, are read instead.
func earlierRaw(paired *models.Mock, served, visible []*models.Mock, earlier []learn.Earlier) []learn.Raw {
	leafed := make(map[string]bool, len(earlier)+1)
	leafed[paired.Name] = true
	for _, e := range earlier {
		leafed[e.Mock] = true
	}
	var out []learn.Raw
	for _, m := range served {
		if m != nil && !leafed[m.Name] {
			out = append(out, learn.RawOf(m, true))
		}
	}
	for _, m := range visible {
		if m != nil && !leafed[m.Name] && m.Spec.ReqTimestampMock.Before(paired.Spec.ReqTimestampMock) {
			out = append(out, learn.RawOf(m, false))
		}
	}
	return out
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
	for {
		if ctx.Err() != nil {
			return nil, nil, ctx.Err()
		}
		sc, err := h.learnScope(mockDb, tracker, live, liveShape, n, opts.LearnPool)
		if err != nil {
			return nil, nil, err
		}
		// What the test served so far is read after the scope: a mock a
		// concurrent call consumed before the scope was read is gone from
		// the scope, and the mock manager told the tracker of it before it
		// left (MockManager.observeServed), so it is in one of the two.
		test, earlier, served := tracker.Test()

		res := learn.Pair(liveShape, sc.cands)
		switch res.Outcome {
		case learn.Exact:
			m := sc.mocks[res.Index]
			// A request recorded several times answers with each recorded
			// answer in turn (cursorPick), among the recordings it matches
			// exactly, as outside the learn step.
			picked, commit := h.cursorPick(m, exactGroup(liveShape, sc), mockDb, !opts.DisableStatefulMocks)
			out, claimed, err := h.claim(ctx, picked, mockDb, nil, nil)
			if err != nil {
				return nil, nil, err
			}
			if !claimed {
				continue
			}
			if commit != nil {
				commit()
			}
			rec := sc.cands[res.Index].Shape
			own := sc.own[res.Index]
			if picked != m {
				rp := recordedParts(picked)
				rec = n.shape(rp, destOf(live, rp))
				for i, c := range sc.mocks {
					if c == picked {
						own = sc.own[i]
					}
				}
			}
			tracker.Served(test, earlierOf(picked, rec, liveShape, out, own))
			return out, nil, nil

		case learn.Paired:
			m, c, own := sc.mocks[res.Index], sc.cands[res.Index], sc.own[res.Index]
			earlier := earlierThan(earlier, m)
			learned := learn.Classify(res.Diff.Drift, liveShape, c.Shape, m.Spec.ReqTimestampMock, earlier,
				earlierRaw(m, served, sc.visible, earlier), respLeaves(m.Spec.HTTPResp))
			plan, why := learnPlan(tracker, test, m, c.Shape, own, learned, earlier)
			if why != "" {
				res.Outcome, res.Reason, res.Why = learn.Miss, models.MissRequestChanged, why
				return nil, &learnMiss{res: res, closest: m, pool: sc.http}, nil
			}
			out, claimed, why, err := h.claimLearned(ctx, m, mockDb, plan)
			if err != nil {
				return nil, nil, err
			}
			if why != "" {
				res.Outcome, res.Reason, res.Why = learn.Miss, models.MissRequestChanged, why
				return nil, &learnMiss{res: res, closest: m, pool: sc.http}, nil
			}
			if !claimed {
				continue
			}
			for _, l := range learned {
				h.Logger.Info("learned a field that changed between record and replay",
					zap.String("mock", m.Name), zap.String("field", l.Recorded.Path), zap.String("as", string(l.Kind)))
			}
			tracker.Served(test, earlierOf(m, c.Shape, liveShape, out, own))
			return out, nil, nil
		}
		miss := &learnMiss{res: res, pool: sc.http}
		if res.Index >= 0 {
			miss.closest = sc.mocks[res.Index]
		}
		return nil, miss, nil
	}
}

// pairing is what the learn step learned on a paired mock: its noise, its
// variables (an echo included), the variables of the test bound at an earlier
// mock that it uses (binds, by that mock), and each variable's value in this
// call, for its answer.
type pairing struct {
	noise  map[string][]string
	vars   models.MockVariables
	binds  map[string]models.MockVariables
	values map[string]variables.Binding
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
//   - An echo is a variable of this call alone (echo: true), bound here and
//     answered where the recorded answer holds the value whole. Leaves of
//     this call that drifted from the same recorded value are one variable.
func learnPlan(tracker *learn.Tracker, test uint64, m *models.Mock, rec learn.Shape, own bool, learned []learn.Learned, earlier []learn.Earlier) (pairing, string) {
	p := pairing{noise: map[string][]string{}, vars: models.MockVariables{}, binds: map[string]models.MockVariables{}, values: map[string]variables.Binding{}}
	jsonReq := rec.Fixed["body"] == "json"
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
	for _, l := range learned {
		r := l.Recorded
		switch l.Kind {
		case learn.KindNoise:
			p.noise[r.Path] = []string{}

		case learn.KindVariable:
			from := l.From
			switch {
			case from.Side == "":
				return p, r.Path + " carries a value the test's recording holds earlier (in " + from.Mock + "), where keploy cannot bind it yet: a call of another protocol, or inside its payload"
			case from.Side != learn.Request:
				return p, r.Path + " carries a value " + from.Mock + " answered earlier in the test, which keploy cannot follow from an answer"
			case !from.Whole || from.Var == "":
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
			name, ok := tracker.Name(test, from.Mock, from.Var)
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
			bv.Answer = appendNew(bv.Answer, earlierAnswers(earlier, from.Mock, r)...)
			b[name] = bv
			p.values[name] = variables.Binding{Name: name, Value: from.Live, Recorded: r.Value, Mock: from.Mock, Place: from.Var}

		case learn.KindEcho:
			place, ok := learn.VarPlace(r, jsonReq)
			if !ok {
				return p, r.Path + " is echoed in the answer, at a place keploy cannot bind it (a form or multipart field)"
			}
			at := answers(l, l.Echo)
			if len(at) == 0 {
				return p, r.Path + " is echoed in the answer only inside a longer value, which keploy cannot answer with this run's value"
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
			name, ok := tracker.Name(test, m.Name, place)
			if !ok {
				return p, "the test ended before " + r.Path + " was learned"
			}
			echoed[r.Value] = name
			p.vars[name] = models.MockVariable{Recorded: r.Value, Bind: place, Answer: at, Echo: true}
			p.values[name] = variables.Binding{Name: name, Value: l.Live.Value, Recorded: r.Value, Mock: m.Name, Place: place}
		}
	}
	return p, ""
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
// took m first.
func (h *HTTP) claimLearned(ctx context.Context, m *models.Mock, mockDb integrations.MockMemDb, p pairing) (served *models.Mock, claimed bool, why string, err error) {
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
	claimed = h.consumeWith(m, mockDb, func(spec *models.MockSpec) {
		if len(p.noise) > 0 {
			spec.ReqBodyNoise = mergeReqBodyNoise(spec.ReqBodyNoise, p.noise)
		}
		if len(p.vars) > 0 {
			vars := spec.Variables.Clone()
			if vars == nil {
				vars = models.MockVariables{}
			}
			for name, v := range p.vars {
				vars[name] = v
			}
			spec.Variables = vars
		}
		if len(p.binds) > 0 {
			spec.Binds = p.binds
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
		group := exactGroup(liveShape, sc)
		if len(group) == 0 {
			return nil, nil
		}
		hit := group[0]
		if len(hit.Spec.ReqBodyNoise) == 0 {
			return nil, nil
		}
		picked, commit := h.cursorPick(hit, group, mockDb, !opts.DisableStatefulMocks)
		served, claimed, err := h.claim(ctx, picked, mockDb, nil, nil)
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
