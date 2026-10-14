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
// noise, the noise learned onto m, m's correlations, and m's obfuscated
// values (Mock.Noise).
func (n learnNoise) known(m *models.Mock) learn.Known {
	k := httpKnown{n: n, learned: m.Spec.ReqBodyNoise, values: util.NewNoiseChecker(m.Noise)}
	if len(m.Spec.Correlations) > 0 {
		k.correlated = make(map[string]bool, len(m.Spec.Correlations))
		for _, c := range m.Spec.Correlations {
			k.correlated[c.RequestPath] = true
		}
	}
	return k
}

type httpKnown struct {
	n          learnNoise
	learned    map[string][]string
	correlated map[string]bool
	values     *util.NoiseChecker
}

// Excused implements learn.Known. Learned noise and correlations keep their
// type; keploy.yml's noise and obfuscated values excuse type and presence.
// What the learn step learned names a leaf by its exact path, an array
// position included; noise learned before it (by the request-body learner
// it replaces) names it in the request matcher's dialect.
func (k httpKnown) Excused(l learn.Leaf) (bool, bool) {
	if learnedCovers(k.learned, l.Path) || l.Noise != l.Path && hasKey(k.learned, l.Noise) || k.correlated[l.Path] {
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

// respLeaves are the leaves of a mock's recorded answer a correlation can
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

// bindings are the live values a request gives the correlations of the mock
// it matched: each one's request leaf, by its exact path.
func bindings(live learn.Shape, corrs []models.FieldCorrelation) map[string]string {
	if len(corrs) == 0 {
		return nil
	}
	out := make(map[string]string, len(corrs))
	for _, c := range corrs {
		if l, ok := live.Leaves[c.RequestPath]; ok {
			out[c.RequestPath] = l.Value
		}
	}
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
		if m == nil || m.Spec.HTTPReq == nil {
			continue
		}
		rp := recordedParts(m)
		d := destOf(live, rp)
		// The key first: a body is decoded only for a candidate.
		if operationKey(rp.method, d, len(n.segments(rp.path))) != liveShape.Key {
			continue
		}
		own := open && i < len(httpPerTest)
		if owner != nil {
			owned, known := owner.OwnedByTest(m)
			own = open && known && owned
		}
		c := learn.Candidate{
			Shape:    n.shape(rp, d),
			Known:    n.known(m),
			Pairable: !m.IsProtocolPhase() && (own || pool),
		}
		if own {
			sc.cands, sc.mocks = append(sc.cands, c), append(sc.mocks, m)
		} else {
			poolCands, poolMocks = append(poolCands, c), append(poolMocks, m)
		}
	}
	sc.cands, sc.mocks = append(sc.cands, poolCands...), append(sc.mocks, poolMocks...)
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

// earlierRaw are the mocks of the test's recording that came before a
// request recorded at at, as Classify reads them when no leaf names their
// values: every mock any parser served earlier in the test, and every mock
// the scope shows that was recorded before it. The paired mock is not one.
func earlierRaw(paired *models.Mock, served, visible []*models.Mock) []learn.Raw {
	var out []learn.Raw
	for _, m := range served {
		if m != nil && m.Name != paired.Name {
			out = append(out, learn.RawOf(m, true))
		}
	}
	for _, m := range visible {
		if m != nil && m.Name != paired.Name && m.Spec.ReqTimestampMock.Before(paired.Spec.ReqTimestampMock) {
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
// learned onto that mock (noise, a correlation, or a variable); otherwise it
// is a miss with its reason. It returns the mock to serve, or the miss.
func (h *HTTP) learnMatch(ctx context.Context, input *req, mockDb integrations.MockMemDb, n learnNoise, opts models.OutgoingOptions) (*models.Mock, *learnMiss, error) {
	tracker := learn.TrackerFrom(ctx)
	test, earlier, served := tracker.Test()
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

		res := learn.Pair(liveShape, sc.cands)
		switch res.Outcome {
		case learn.Exact:
			m := sc.mocks[res.Index]
			// A request recorded several times answers with each recorded
			// answer in turn (cursorPick), among the recordings it matches
			// exactly, as outside the learn step.
			picked, commit := h.cursorPick(m, exactGroup(liveShape, sc), mockDb, !opts.DisableStatefulMocks)
			out, claimed, err := h.claimLearned(ctx, picked, mockDb, nil, nil, bindings(liveShape, picked.Spec.Correlations))
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
			if picked != m {
				rp := recordedParts(picked)
				rec = n.shape(rp, destOf(live, rp))
			}
			tracker.Served(test, earlierOf(picked, rec, liveShape, out))
			return out, nil, nil

		case learn.Paired:
			m, c := sc.mocks[res.Index], sc.cands[res.Index]
			learned := learn.Classify(res.Diff.Drift, liveShape, c.Shape, m.Spec.ReqTimestampMock, earlier,
				earlierRaw(m, served, sc.visible), respLeaves(m.Spec.HTTPResp))
			noise := map[string][]string{}
			var corrs []models.FieldCorrelation
			for _, l := range learned {
				switch l.Kind {
				case learn.KindVariable:
					if !tracker.Variables().Follow(m.Name, l.Live, l.From) {
						res.Outcome, res.Reason = learn.Miss, models.MissRequestChanged
						res.Why = l.Recorded.Path + " carries a value an earlier call of the test (" + l.From.Mock + ") recorded, and keploy cannot follow it from there yet"
						return nil, &learnMiss{res: res, closest: m, pool: sc.http}, nil
					}
				case learn.KindCorrelation:
					corrs = append(corrs, models.FieldCorrelation{RequestPath: l.Recorded.Path, ResponsePaths: leafPaths(l.Echo), RecordedValue: l.Recorded.Value})
				default:
					noise[l.Recorded.Path] = []string{}
				}
			}
			all := append(append([]models.FieldCorrelation(nil), m.Spec.Correlations...), corrs...)
			out, claimed, err := h.claimLearned(ctx, m, mockDb, noise, corrs, bindings(liveShape, all))
			if err != nil {
				return nil, nil, err
			}
			if !claimed {
				continue
			}
			for _, l := range learned {
				h.Logger.Info("learned a field that changed between record and replay",
					zap.String("mock", m.Name), zap.String("field", l.Recorded.Path), zap.String("as", string(l.Kind)))
			}
			tracker.Served(test, earlierOf(m, c.Shape, liveShape, out))
			return out, nil, nil
		}
		miss := &learnMiss{res: res, pool: sc.http}
		if res.Index >= 0 {
			miss.closest = sc.mocks[res.Index]
		}
		return nil, miss, nil
	}
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

// earlierOf is the exchange F just served from m, as a later request of the
// test reads it: each recorded leaf of its request with the live value, and
// each recorded leaf of its answer with the value served.
func earlierOf(m *models.Mock, rec, live learn.Shape, served *models.Mock) learn.Earlier {
	e := learn.Earlier{Mock: m.Name, At: m.Spec.ReqTimestampMock}
	for p, r := range rec.Leaves {
		e.Seen = append(e.Seen, learn.Seen{Side: learn.Request, Leaf: r, Live: live.Leaves[p].Value})
	}
	sent := map[string]string{}
	if served != nil {
		for _, l := range respLeaves(served.Spec.HTTPResp) {
			sent[l.Path] = l.Value
		}
	}
	for _, r := range respLeaves(m.Spec.HTTPResp) {
		e.Seen = append(e.Seen, learn.Seen{Side: learn.Response, Leaf: r, Live: sent[r.Path]})
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
// request whose first exact match, modulo what is known, carries what the
// learn step learned (noise or correlations): the replays after F honour what
// F learned, in every part of the request (its path, query, headers and
// body), and render each correlation where F found its echo. The test's own
// mocks come first, in recorded order, as in F; a request recorded several
// times answers with each recorded answer in turn (cursorPick). A request
// whose first exact match learned nothing, or that matches none, goes on to
// the matcher as before.
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
		if len(hit.Spec.ReqBodyNoise) == 0 && len(hit.Spec.Correlations) == 0 {
			return nil, nil
		}
		picked, commit := h.cursorPick(hit, group, mockDb, !opts.DisableStatefulMocks)
		served, claimed, err := h.claim(ctx, picked, mockDb, nil, bindings(liveShape, picked.Spec.Correlations))
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
