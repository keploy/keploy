package http

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mismatch"
	"go.keploy.io/server/v3/pkg/bodydigest"
	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/pkg/templates"
	"go.keploy.io/server/v3/pkg/variables"
	"go.uber.org/zap"
)

// Under the proven switch, a mock that declares variables
// (models.MockVariables) is served by the variable pass alone:
//
//  1. Each of its request places must be in the live call, with the JSON type
//     the recording has there. The mock's RECORDED value is put back at each,
//     and the call must then be the recording exactly: method, URL path
//     (configured url noise only, never an id-shaped guess), query, header
//     keys, and a body that is byte-equal or the same JSON outside the known
//     noise (configured body noise, the mock's noise.req, the obfuscator's
//     values), the type kept at a noise place. No score, no lenient pass.
//  2. Its variables must hold. A variable already bound in the test must have
//     its value at every place of the call (bound once: a binding place
//     holds it too). One not yet bound is bound here only at its binding
//     place, to a value that is not empty and is no other variable's; a use
//     before the call that binds it does not hold. An echo variable belongs
//     to this call alone.
//  3. Among the mocks that fit and hold, the first in record order answers;
//     reusable recordings of one call answer in record order, each once, then
//     the last again (served marks, pickHit). It is consumed as any mock, its
//     binds are committed, and it is served with each answer place holding
//     its variable's value.
//
// No later pass sees such a mock. When one fits but its variables do not
// hold, only an exact recording of this very call may still answer it;
// otherwise the call is a miss whose evidence is the variable
// (MatchPhaseVariable).

// varPlace is one place of a variable in a mock, read: the value at place,
// or the word of it at (variables.ReadPlace: a place the recording holds as
// written is whole; else a word address, which another protocol's learn
// step writes where an HTTP call sent the value it copies inside a longer
// text).
type varPlace struct {
	raw   string
	place variables.Place
	at    variables.Address // at.Place is place; Word -1 and no step: whole
}

// whole reports whether the place is its value whole.
func (vp varPlace) whole() bool { return vp.at.Whole() }

// key names the place among the call's places: its address as written back.
func (vp varPlace) key() string { return vp.at.String() }

// varDecl is one variable of a mock, read.
type varDecl struct {
	name     string
	recorded string
	echo     bool
	// clock: the clock reads the learn step marked at the variable's bind
	// (models.MockVariable.Clock). It is read only where the call binds the
	// variable.
	clock  []models.ClockRead
	bind   *varPlace
	use    []varPlace
	answer []varPlace
}

// requestPlaces is the variable's bind place (first) and uses.
func (d varDecl) requestPlaces() []varPlace {
	out := make([]varPlace, 0, 1+len(d.use))
	if d.bind != nil {
		out = append(out, *d.bind)
	}
	return append(out, d.use...)
}

// hasVariables reports whether m is served by the variable pass.
func hasVariables(m *models.Mock) bool {
	return m != nil && len(m.Spec.Variables) > 0
}

// splitVariableMocks splits a pool into the mocks the variable pass serves,
// per-test before reusable and each in record order, and the rest, in the
// pool's order.
func splitVariableMocks(pool []*models.Mock) (withVars, rest []*models.Mock) {
	for _, m := range pool {
		if hasVariables(m) {
			withVars = append(withVars, m)
			continue
		}
		rest = append(rest, m)
	}
	tier := func(m *models.Mock) int {
		if m.TestModeInfo.Lifetime == models.LifetimePerTest {
			return 0
		}
		return 1
	}
	sort.SliceStable(withVars, func(i, j int) bool {
		a, b := withVars[i], withVars[j]
		if tier(a) != tier(b) {
			return tier(a) < tier(b)
		}
		return recordedBefore(a, b)
	})
	return withVars, rest
}

// readDecls reads m's variables, by name, or says why one cannot be followed.
// A request place is read against m's recorded request (variables.ReadPlace);
// an answer place against its recorded response, which may not be loaded yet,
// where it is answered (renderAnswers): here, as a word address.
func readDecls(m *models.Mock) ([]varDecl, string) {
	names := make([]string, 0, len(m.Spec.Variables))
	for name := range m.Spec.Variables {
		names = append(names, name)
	}
	sort.Strings(names)
	var rec reqView
	recorded := false
	if m.Spec.HTTPReq != nil {
		rec, recorded = recordedView(m.Spec.HTTPReq)
	}
	inRequest := func(p variables.Place) bool {
		_, ok := rec.leaf(p)
		return recorded && ok
	}
	parse := func(name, role, raw string, answer bool) (varPlace, string) {
		holds := inRequest
		if answer {
			holds = func(variables.Place) bool { return false }
		}
		a, err := variables.ReadPlace(raw, holds)
		if err != nil {
			return varPlace{}, fmt.Sprintf("the %s place of variable %s: %v", role, name, err)
		}
		if answer && a.Place.Part != variables.PartBody && a.Place.Part != variables.PartHeader {
			return varPlace{}, fmt.Sprintf("the answer place %s of variable %s is not a place of a response (body.<path> or header.<name>)", raw, name)
		}
		return varPlace{raw: raw, place: a.Place, at: a}, ""
	}
	seen := map[string]string{}
	out := make([]varDecl, 0, len(names))
	for _, name := range names {
		mv := m.Spec.Variables[name]
		if name == "" {
			return nil, "a variable has no name"
		}
		if mv.Recorded == "" {
			return nil, fmt.Sprintf("variable %s has no recorded value", name)
		}
		d := varDecl{name: name, recorded: mv.Recorded, echo: mv.Echo, clock: mv.Clock}
		if mv.Bind != "" {
			p, why := parse(name, "bind", mv.Bind, false)
			if why != "" {
				return nil, why
			}
			d.bind = &p
		}
		for _, raw := range mv.Use {
			p, why := parse(name, "use", raw, false)
			if why != "" {
				return nil, why
			}
			d.use = append(d.use, p)
		}
		for _, raw := range mv.Answer {
			p, why := parse(name, "answer", raw, true)
			if why != "" {
				return nil, why
			}
			d.answer = append(d.answer, p)
		}
		if d.echo && d.bind == nil {
			return nil, fmt.Sprintf("the echo variable %s has no bind place: an echo is what this call sends", name)
		}
		if d.bind == nil && len(d.use) == 0 && len(d.answer) == 0 {
			return nil, fmt.Sprintf("variable %s names no place", name)
		}
		for _, vp := range d.requestPlaces() {
			key := vp.key()
			if vp.place.Part == variables.PartHeader {
				key = strings.ToLower(key) // a header's name is read in any case
			}
			if other, dup := seen[key]; dup {
				return nil, fmt.Sprintf("the request place %s is declared twice (variables %s and %s)", key, other, name)
			}
			seen[key] = name
		}
		out = append(out, d)
	}
	return out, ""
}

// reqView is one HTTP request as the variable pass reads it.
type reqView struct {
	method   string
	segments []string // the URL path's segments, escaped, after the leading slash
	query    url.Values
	header   http.Header
	body     string
}

func liveView(input *req) reqView {
	v := reqView{method: input.method, header: input.header, body: string(input.body)}
	if input.url != nil {
		v.segments = pathSegments(input.url.EscapedPath())
		v.query = input.url.Query()
	}
	return v
}

func recordedView(r *models.HTTPReq) (reqView, bool) {
	u, err := url.Parse(r.URL)
	if err != nil {
		return reqView{}, false
	}
	v := reqView{
		method:   string(r.Method),
		segments: pathSegments(u.EscapedPath()),
		query:    url.Values{},
		header:   http.Header{},
		body:     r.Body,
	}
	// Recorded URLParams take precedence over the URL's own query, as the
	// matcher reads them (QueryParamsMatch).
	if len(r.URLParams) > 0 {
		for k, val := range r.URLParams {
			v.query.Set(k, val)
		}
	} else {
		v.query = u.Query()
	}
	for k, val := range r.Header {
		v.header.Set(k, val)
	}
	return v, true
}

func pathSegments(escaped string) []string {
	return strings.Split(strings.TrimPrefix(escaped, "/"), "/")
}

// held is the value at a place of a request.
type held struct {
	value string
	// kind is the JSON type of a body place.
	kind templates.LeafKind
	raw  string // the JSON text of a body place
	body bool
	// word: value is a word of the place's value (kind and raw are the
	// place's), compared as text.
	word bool
}

func (h held) number() bool { return h.body && h.kind == templates.LeafNumber && !h.word }

// empty reports whether the value cannot be a variable's: empty text, or a
// JSON null.
func (h held) empty() bool {
	return h.value == "" || (h.body && h.kind == templates.LeafNull)
}

// read returns the value at vp: the value at its place, or the word of it the
// place names (its kind and raw the place's); false where the request has no
// value there, or no such word.
func (v reqView) read(vp varPlace) (held, bool) {
	h, ok := v.leaf(vp.place)
	if !ok || vp.whole() {
		return h, ok
	}
	if h.body && h.kind == templates.LeafNull {
		return held{}, false
	}
	w, ok := vp.at.WordOf(h.value)
	if !ok {
		return held{}, false
	}
	h.value, h.word = w, true
	return h, true
}

// leaf returns the value at p, or false when the request has none there (or
// more than one: a repeated query parameter or header is no place).
func (v reqView) leaf(p variables.Place) (held, bool) {
	switch p.Part {
	case variables.PartBody:
		l, ok := variables.JSONLeafAt(v.body, p.Path)
		if !ok {
			return held{}, false
		}
		return held{value: l.Value(v.body), kind: l.Kind, raw: l.Raw(v.body), body: true}, true
	case variables.PartPath:
		if p.Index >= len(v.segments) {
			return held{}, false
		}
		s, err := url.PathUnescape(v.segments[p.Index])
		if err != nil {
			return held{}, false
		}
		return held{value: s}, true
	case variables.PartQuery:
		vals, ok := v.query[p.Name]
		if !ok || len(vals) != 1 {
			return held{}, false
		}
		return held{value: vals[0]}, true
	case variables.PartHeader:
		vals := v.header.Values(p.Name)
		if len(vals) != 1 {
			return held{}, false
		}
		return held{value: vals[0]}, true
	}
	return held{}, false
}

// putBackAt returns v with rec, the recorded value at vp, put back there: the
// request as it would be had the variable kept its recorded value. A word is
// put back at its word of v's value there (variables.Address.ReplaceWord,
// which reads the word back: a word for a word, so another place's word of
// the same value still names its word), as a string, every other byte of it
// kept; false where it cannot be.
func (v reqView) putBackAt(vp varPlace, rec held) (reqView, bool) {
	if vp.whole() {
		return v.putBack(vp.place, rec)
	}
	cur, ok := v.leaf(vp.place)
	if !ok {
		return v, false
	}
	out, ok := vp.at.ReplaceWord(cur.value, rec.value)
	if !ok {
		return v, false
	}
	text := held{value: out}
	if vp.place.Part == variables.PartBody {
		raw, err := json.Marshal(out)
		if err != nil {
			return v, false
		}
		text.raw = string(raw)
	}
	return v.putBack(vp.place, text)
}

// putBack returns v with the value rec at p (a body place's JSON text rec.raw).
func (v reqView) putBack(p variables.Place, rec held) (reqView, bool) {
	switch p.Part {
	case variables.PartBody:
		body, ok := variables.SpliceAt(v.body, p.Path, rec.raw)
		if !ok {
			return v, false
		}
		v.body = body
	case variables.PartPath:
		if p.Index >= len(v.segments) {
			return v, false
		}
		v.segments = append([]string(nil), v.segments...)
		v.segments[p.Index] = url.PathEscape(rec.value)
	case variables.PartQuery:
		q := make(url.Values, len(v.query))
		for k, vals := range v.query {
			q[k] = vals
		}
		q[p.Name] = []string{rec.value}
		v.query = q
	case variables.PartHeader:
		h := v.header.Clone()
		h.Del(p.Name)
		h.Set(p.Name, rec.value)
		v.header = h
	}
	return v, true
}

func (v reqView) asReq(input *req) *req {
	out := &req{method: v.method, header: v.header, body: []byte(v.body), raw: input.raw, encoded: input.sentBody(), arrived: input.arrived}
	u := url.URL{}
	if input.url != nil {
		u = *input.url
	}
	escaped := "/" + strings.Join(v.segments, "/")
	if p, err := url.PathUnescape(escaped); err == nil {
		u.Path = p
		u.RawPath = escaped
	}
	u.RawQuery = v.query.Encode()
	out.url = &u
	return out
}

// varProblem is why a mock that fits a call did not answer it. It is the
// miss's evidence.
type varProblem struct {
	mock  string
	name  string
	place string
	// kind is the field diff the problem is: DiffKindVariable when the place
	// held another value than the test's variable (expected: the variable's
	// value, actual: the call's), DiffKindTypeChanged when it held a value of
	// another JSON type (expected: the recording's, actual: the call's); ""
	// when the problem is no difference of values (a declaration, a use
	// before the binding call, an empty value, another variable's value): its
	// why says what it is.
	kind     models.MockFieldDiffKind
	expected string
	actual   string
	why      string
	next     string
	// given is name when the problem is a given's own: the call carried
	// another value than the test gave, or a value of another JSON type, or
	// this run's test did not give it before the call. The miss names it
	// (MockMismatchReport.Given).
	given string
}

func (p *varProblem) diff() string {
	return fmt.Sprintf("mock %s: %s", p.mock, p.why)
}

// varHit is a mock the variable pass can serve, and what it binds.
type varHit struct {
	mock  *models.Mock
	decls []varDecl
	// binds are this call's new bindings of the test's variables, committed
	// once it is answered; local holds its echo variables, for its answers
	// only.
	binds []variables.Binding
	local map[string]variables.Binding
	// table is the scope the call was checked in (varScope.of), and gen that
	// table's scope then (variables.Table.Gen): its binds land there.
	table *variables.Table
	gen   uint64
	// saw is every binding of table the check read, by name, with its value:
	// a bind refused over one of them is no race (serveVariablesBy).
	saw map[string]string
}

// varScope is the scopes a call's variables are checked in, each read before
// the pool was (varScope.of): the store's table (integrations.VariableStore),
// and, under exact serving between tests, the next test's, which its calls in
// flight the pool view answers bind in (integrations.VariableScopes).
type varScope struct {
	table    *variables.Table
	gen      uint64
	ahead    *variables.Table
	aheadGen uint64
	inAhead  func(*models.Mock) bool
}

// readVarScope reads the scopes of mockDb, before the pool is read: a scope
// that begins after makes the binds of a call checked in it stale
// (variables.Table.BindAll).
func readVarScope(mockDb integrations.MockMemDb) varScope {
	var s varScope
	s.table = variableTable(mockDb)
	if s.table != nil {
		s.gen = s.table.Gen()
	}
	if vs, ok := mockDb.(integrations.VariableScopes); ok {
		s.ahead, s.inAhead = vs.AheadVariables(), vs.InAheadScope
		if s.ahead != nil {
			s.aheadGen = s.ahead.Gen()
		}
	}
	return s
}

// of is the table a call m answers binds in, and its scope when read.
func (s varScope) of(m *models.Mock) (*variables.Table, uint64) {
	if s.ahead != nil && s.inAhead != nil && s.inAhead(m) {
		return s.ahead, s.aheadGen
	}
	return s.table, s.gen
}

// moved reports whether a scope read began another since: the call is
// checked again against it.
func (s varScope) moved() bool {
	return s.table != nil && s.table.Gen() != s.gen || s.ahead != nil && s.ahead.Gen() != s.aheadGen
}

// variablePass returns the mocks of pool (in its order) that the live call
// fits and whose variables hold, and otherwise the evidence of the first that
// fits but whose variables do not hold, if any.
//
// scope is the variables' scopes the caller read before it fetched pool: each
// mock is checked in the one a call it answers binds in.
func (h *HTTP) variablePass(ctx context.Context, input *req, pool []*models.Mock, scope varScope, headerNoise, userBodyNoise map[string][]string, urlNoise []string) ([]*varHit, *varProblem, error) {
	live := liveView(input)
	var hits []*varHit
	var first *varProblem
	for _, m := range pool {
		if ctx.Err() != nil {
			return nil, nil, ctx.Err()
		}
		table, gen := scope.of(m)
		hit, problem := h.fitVariables(ctx, input, live, m, table, headerNoise, userBodyNoise, urlNoise)
		switch {
		case hit != nil:
			hit.table, hit.gen = table, gen
			hits = append(hits, hit)
		case problem != nil && (first == nil || (first.kind == "" && problem.kind != "")):
			// A value that differs is better evidence than a declaration
			// keploy cannot follow or a variable not yet bound.
			first = problem
		}
	}
	if len(hits) > 0 {
		return hits, nil, nil
	}
	return nil, first, nil
}

// fitVariables reads the live call against one mock. It returns a hit, or the
// evidence why a mock the call fits cannot answer it, or neither when the call
// is not this mock's.
func (h *HTTP) fitVariables(ctx context.Context, input *req, live reqView, m *models.Mock, table *variables.Table, headerNoise, userBodyNoise map[string][]string, urlNoise []string) (*varHit, *varProblem) {
	if m.Spec.HTTPReq == nil {
		return nil, nil
	}
	decls, bad := readDecls(m)
	rec, ok := recordedView(m.Spec.HTTPReq)
	if bad == "" && !ok {
		bad = "its recorded URL cannot be read"
	}
	refuse := func(why string) (*varHit, *varProblem) {
		// A declaration keploy cannot follow is refused, never served as
		// literal. It is this call's evidence when the call is to the mock's
		// route: its destination (strict.go), method and URL path, a segment
		// it declares a variable's aside.
		if !sameRoute(live, rec, m) || !sameDestination(liveDestination(ctx, input), recordedDestination(m)) {
			return nil, nil
		}
		return nil, &varProblem{
			mock: m.Name,
			why:  "its variables cannot be followed: " + why,
			next: "the variables block of " + m.Name + " is not one keploy can follow; re-record the test (or remove that block to replay the call as recorded)",
		}
	}
	if bad != "" {
		return refuse(bad)
	}
	// Learned noise at a SigV4 Authorization or a presigned URL's signing
	// parameters is put back below; the parts the strict matcher compares
	// by grammar must hold first (founder decision 25), or the call is the
	// strict matcher's to decide.
	if !sigV4PartsHold(recordedParts(m), liveParts(ctx, input)) {
		return nil, nil
	}

	// The variables' places, read on both sides; then the recorded values put
	// back into the live call.
	liveAt := map[string]held{}
	asRecorded := live
	var typeChange *varProblem
	clocks := clocksOf(m)
	jsonBody := variables.IsJSONDoc(m.Spec.HTTPReq.Body)
	clockAt := map[string]models.ClockOffset{} // a variable's place -> the read of the clock learned there
	for k, o := range clocks {
		if raw, ok := learn.VarPlace(learn.Leaf{Path: k}, jsonBody); ok {
			if place, err := variables.Parse(raw); err == nil {
				clockAt[place.String()] = o
			}
		}
	}
	for _, d := range decls {
		for _, vp := range d.requestPlaces() {
			if o, clock := clockAt[vp.place.String()]; clock {
				// Two learnings that disagree on what the place holds: a
				// variable's value, which its recorded value is put back
				// at, and a read of the clock, which is checked as it
				// came. keploy cannot hold one value to both.
				return refuse(fmt.Sprintf("%s was learned both as variable %s and as %s, a read of the application's clock: keploy cannot hold one value to both, so it refuses the call. Record the test again", vp.raw, d.name, o.String()))
			}
			r, ok := rec.read(vp)
			if !ok {
				return refuse(fmt.Sprintf("its recorded request holds no single value at %s (variable %s)", vp.raw, d.name))
			}
			if r.word && r.body && r.kind != templates.LeafString {
				// A word is a word of a string, as Mongo's replay reads one.
				return refuse(fmt.Sprintf("its recorded request holds %s at %s, which is no string to hold a word of (variable %s)", r.raw, vp.raw, d.name))
			}
			if !variables.SameValue(d.recorded, r.value, r.number()) {
				return refuse(fmt.Sprintf("its recorded request holds %q at %s, not variable %s's recorded value %q", r.value, vp.raw, d.name, d.recorded))
			}
			l, ok := live.read(vp)
			if !ok {
				return nil, nil // the call has no such place: it is not this mock's call
			}
			if l.body && l.kind != r.kind && typeChange == nil {
				typeChange = &varProblem{
					mock: m.Name, name: d.name, place: vp.raw, kind: models.DiffKindTypeChanged, expected: r.raw, actual: l.raw,
					why:  fmt.Sprintf("variable %s at %s: the call sent %s where the recording has %s, a value of another JSON type", d.name, vp.raw, l.raw, r.raw),
					next: "the application changed the type of the value it sends at " + vp.raw + ": if that is intended, re-record the test; otherwise fix the application",
				}
				if models.IsGivenVariable(d.name) {
					typeChange.given = d.name
				}
			}
			if asRecorded, ok = asRecorded.putBackAt(vp, r); !ok {
				if vp.whole() {
					return nil, nil
				}
				return refuse(fmt.Sprintf("keploy cannot put variable %s's recorded value %q back at %s in this call, so it cannot compare the rest of the call with the recording", d.name, r.value, vp.raw))
			}
			liveAt[vp.key()] = l
		}
	}

	// What the learn step learned as noise at one exact place of this call
	// (a path segment, a query value, a header value, one element of an
	// array) is not compared either: its recorded value is put back there
	// too, the place's presence and JSON type still required.
	for k := range m.Spec.ReqBodyNoise {
		raw, ok := learn.VarPlace(learn.Leaf{Path: k}, jsonBody)
		if !ok {
			continue // a body path in the matcher's dialect: the body noise
		}
		place, err := variables.Parse(raw)
		if err != nil {
			continue
		}
		if _, isVar := liveAt[place.String()]; isVar {
			continue
		}
		if _, clock := clocks[k]; clock {
			// A read of the clock is checked as it came (strictDiff),
			// never excused by noise on the same place.
			continue
		}
		r, ok := rec.leaf(place)
		if !ok {
			continue
		}
		l, ok := live.leaf(place)
		if !ok || l.body && l.kind != r.kind {
			return nil, nil
		}
		if asRecorded, ok = asRecorded.putBack(place, r); !ok {
			return nil, nil
		}
	}

	// 1. The call, with the recorded values put back, is the recording.
	if !h.fitsRecording(ctx, asRecorded.asReq(input), m, headerNoise, userBodyNoise, urlNoise) {
		return nil, nil
	}
	if typeChange != nil {
		return nil, typeChange
	}
	if table == nil {
		return nil, &varProblem{mock: m.Name, why: "this keploy's mock store keeps no variables, so it cannot follow them", next: "run the keploy agent of this keploy version"}
	}
	return holdVariables(m, decls, liveAt, table, refuse)
}

// holdVariables checks the variables of m on a call that fits its recording
// (liveAt holds the call's values at m's places): it returns the hit and what
// it binds, or the evidence why they do not hold, or neither when the call is
// another test's.
func holdVariables(m *models.Mock, decls []varDecl, liveAt map[string]held, table *variables.Table, refuse func(string) (*varHit, *varProblem)) (*varHit, *varProblem) {
	// 2. The variables hold.
	hit := &varHit{mock: m, decls: decls, local: map[string]variables.Binding{}, saw: map[string]string{}}
	taken := map[string]variables.Binding{} // this call's new bindings, by name
	for _, d := range decls {
		places := d.requestPlaces()
		if d.echo {
			v := liveAt[d.bind.key()]
			if v.empty() {
				return nil, emptyBind(m.Name, d, v)
			}
			if p := holdsAt(m.Name, d, places, v.value, m.Name, d.bind.raw, liveAt); p != nil {
				return nil, p
			}
			hit.local[d.name] = variables.Binding{Name: d.name, Value: v.value, Recorded: d.recorded, Mock: m.Name, Place: d.bind.raw}
			continue
		}
		if bound, ok := table.Lookup(d.name); ok {
			hit.saw[bound.Name] = bound.Value
			if !variables.SameValue(bound.Recorded, d.recorded, false) {
				if bound.Given && !models.IsGivenVariable(d.name) {
					// A template key holds one recorded value for the whole
					// set: a call that names it with another is no call of
					// another test, but a declaration keploy cannot follow.
					return refuse(fmt.Sprintf("it names variable %s after the template key the test case's request sends, but records it as %q where config.yaml records %q: the learn step and the templates disagree", d.name, d.recorded, bound.Recorded))
				}
				// Recorded with another value than the one the test bound: a
				// call of another test (reusable mocks are seen by every
				// test), not this one. A value a test gave (a given) is the
				// test's alone, so one recorded with another value is
				// another test's too.
				return nil, nil
			}
			if p := holdsAt(m.Name, d, places, bound.Value, bound.Mock, bound.Place, liveAt); p != nil {
				return nil, p
			}
			continue
		}
		if d.bind == nil {
			place, sent := "", ""
			if len(d.use) > 0 {
				place = d.use[0].raw
				sent = liveAt[d.use[0].key()].value
			} else {
				place = d.answer[0].raw
			}
			if models.IsGivenVariable(d.name) {
				// No call binds a given: the test gives it.
				return nil, &varProblem{
					mock: m.Name, name: d.name, place: place, actual: sent, given: d.name,
					why:  fmt.Sprintf("the recording's test gave %s, which this call carries at %s; this run's test did not give it before this call", d.name, place),
					next: fmt.Sprintf("give %s in the test before the application makes this call, as the test did when it was recorded; if the test no longer gives it, re-record the test", d.name),
				}
			}
			why := fmt.Sprintf("variable %s is used at %s before the call that binds it was made in this test", d.name, place)
			if models.RunVariable(d.name) {
				// The test set's variable: bound by the start-up's call or
				// an earlier test's, which this replay did not make first.
				why = fmt.Sprintf("variable %s is used at %s, and the call that binds it, made by the test set's start-up or an earlier test, was not made before it in this replay", d.name, place)
			}
			return nil, &varProblem{
				mock: m.Name, name: d.name, place: place, actual: sent,
				why:  why,
				next: fmt.Sprintf("the call that first carries variable %s was not made, or not before this one: if the application now makes its calls in another order or no longer makes that call, re-record the test", d.name),
			}
		}
		v := liveAt[d.bind.key()]
		if v.empty() {
			return nil, emptyBind(m.Name, d, v)
		}
		if p := holdsAt(m.Name, d, places, v.value, m.Name, d.bind.raw, liveAt); p != nil {
			return nil, p
		}
		// Two variables are two values: a value the test bound already, as
		// another variable recorded with another value, is that one's, and
		// so is one the next test's calls in flight bound while this call
		// binds the run's (variables.Table.Beside, which the bind checks).
		others := table.Beside()
		for _, b := range others {
			hit.saw[b.Name] = b.Value
		}
		for _, b := range taken {
			others = append(others, b)
		}
		for _, b := range others {
			if variables.TwoValues(b, variables.Binding{Name: d.name, Value: v.value, Recorded: d.recorded, Clock: d.clock}) {
				return nil, &varProblem{
					mock: m.Name, name: d.name, place: d.bind.raw, expected: "a value other than variable " + b.Name + "'s", actual: v.value,
					why:  fmt.Sprintf("this call binds variable %s at %s to %q, which is variable %s's value in this test (bound at %s %s); they were two values when the test was recorded", d.name, d.bind.raw, v.value, b.Name, b.Mock, b.Place),
					next: fmt.Sprintf("the application sent %s's value where it sent %s's when the test was recorded: fix the application, or re-record the test if that is intended", b.Name, d.name),
				}
			}
		}
		taken[d.name] = variables.Binding{Name: d.name, Value: v.value, Recorded: d.recorded, Mock: m.Name, Place: d.bind.raw, Clock: d.clock}
	}
	for _, d := range decls {
		if b, ok := taken[d.name]; ok {
			hit.binds = append(hit.binds, b)
		}
	}
	return hit, nil
}

// holdsAt checks that every request place of d holds want, the value the
// variable has (taken at boundMock boundPlace).
func holdsAt(mock string, d varDecl, places []varPlace, want, boundMock, boundPlace string, liveAt map[string]held) *varProblem {
	for _, vp := range places {
		l := liveAt[vp.key()]
		if variables.SameValue(want, l.value, l.number()) {
			continue
		}
		verb := "sent"
		if d.bind != nil && vp.raw == d.bind.raw {
			verb = "sent, at a place that binds it,"
		}
		if models.IsGivenVariable(d.name) {
			// The test gave the value (boundPlace: where, if it said).
			where := ""
			if boundPlace != "" {
				where = " (" + boundPlace + ")"
			}
			return &varProblem{
				mock: mock, name: d.name, place: vp.raw, kind: models.DiffKindVariable, expected: want, actual: l.value, given: d.name,
				why:  fmt.Sprintf("the test gave %s = %q%s, and this call %s %q at %s: the application did not pass on the value the test gave", d.name, want, where, verb, l.value, vp.raw),
				next: fmt.Sprintf("the application sent another value at %s than %s, the value the test gave: fix the application; if the call no longer carries the test's value, re-record the test", vp.raw, d.name),
			}
		}
		return &varProblem{
			mock: mock, name: d.name, place: vp.raw, kind: models.DiffKindVariable, expected: want, actual: l.value,
			why:  fmt.Sprintf("variable %s is %q in this test (taken at %s %s), and this call %s %q at %s", d.name, want, boundMock, boundPlace, verb, l.value, vp.raw),
			next: fmt.Sprintf("the application sent another value at %s than the one it made in this test: if that is intended, re-record the test; otherwise fix the application", vp.raw),
		}
	}
	return nil
}

func emptyBind(mock string, d varDecl, v held) *varProblem {
	text := `""`
	if v.body && v.kind == templates.LeafNull {
		text = "null"
	}
	return &varProblem{
		mock: mock, name: d.name, place: d.bind.raw, expected: "a value", actual: v.value,
		why:  fmt.Sprintf("variable %s would be bound at %s to %s: an empty value is no variable's value", d.name, d.bind.raw, text),
		next: "the application sent no value where the recording has variable " + d.name + "; fix the application, or re-record the test",
	}
}

// sameRoute reports whether a call is to the route of m's recorded request:
// one method, and one URL path but at the segments m declares variables at.
func sameRoute(live, rec reqView, m *models.Mock) bool {
	if live.method != rec.method || len(live.segments) != len(rec.segments) {
		return false
	}
	declared := map[int]bool{}
	for _, mv := range m.Spec.Variables {
		for _, raw := range append([]string{mv.Bind}, mv.Use...) {
			// A segment's place, or a word of it (a path place has no name
			// a word address could misread).
			if a, err := variables.ParseAddress(raw); err == nil && a.Place.Part == variables.PartPath {
				declared[a.Place.Index] = true
			}
		}
	}
	for i := range live.segments {
		if !declared[i] && live.segments[i] != rec.segments[i] {
			return false
		}
	}
	return true
}

// fitsRecording reports whether the call (the recorded values put back) is
// m's recorded request exactly, outside the known noise: the strict
// comparison every other mock is matched with under the proven switch
// (strictDiff, row S1), so a mock with variables is held to the same rules:
// the destination, every header value (the credentials included) and the
// body field by field. Known noise widens only this comparison: the
// variables are checked at their places apart from it, so a noise entry over
// a variable's place never excuses a wrong value there.
func (h *HTTP) fitsRecording(ctx context.Context, asRecorded *req, m *models.Mock, headerNoise, userBodyNoise map[string][]string, urlNoise []string) bool {
	return len(strictDiff(ctx, asRecorded, liveDestination(ctx, asRecorded), m, h.newStrictNoise(headerNoise, userBodyNoise, urlNoise))) == 0
}

// pickHit chooses the hit that answers: the first of hits, and among the
// reusable (ConsumeCursorSaturate) recordings of this call whose variables
// hold, the first in record order not yet served, else the last (a call past
// them stays on it, as a saturating cursor does). Each recording's served
// mark (servedPick) is the call's own: recordings of one call that differ
// only at their variable places (a fresh key each, which the dependency
// echoes) are taken in turn; a call whose set of fitting recordings grows
// from one call to the next (an added header) carries on through them; and
// two callers whose calls fit two sets of recordings (two tokens) each take
// theirs in order. A recording whose variables do not hold (its variable not
// yet bound, or another test's) is passed over and keeps its turn. Record
// order is recordedBefore's, as splitVariableMocks orders a tier. Under exact
// serving the walk is servedWalk's, over the recordings the store stages
// that member says are this call too (spent ones: stagedGroup); answered,
// non-nil, is then the call's miss. commit marks the answer served once the
// call is answered (nil when no mark applies).
func (h *HTTP) pickHit(hits []*varHit, own func(*models.Mock) bool, mockDb integrations.MockMemDb, statefulMocks bool, member func(*models.Mock) bool) (*varHit, func(), *answeredSequence) {
	first := hits[0]
	if !statefulMocks || first.mock.TestModeInfo.Consume != models.ConsumeCursorSaturate {
		return first, nil, nil
	}
	_, exact := integrations.SequenceViewOf(mockDb)
	cs, ok := mockDb.(integrations.MockCursor)
	if !ok && !exact {
		return first, nil, nil
	}
	byMock := make(map[*models.Mock]*varHit, len(hits))
	stateful := make([]*models.Mock, 0, len(hits))
	for _, hit := range hits {
		if hit.mock.TestModeInfo.Consume == models.ConsumeCursorSaturate {
			byMock[hit.mock] = hit
			stateful = append(stateful, hit.mock)
		}
	}
	// Under the background rules the open test's own and the set's
	// background are two bands (statefulBand).
	mocks, cs, bg, ofBand := statefulBand(first.mock, stateful, own, mockDb, cs)
	sort.SliceStable(mocks, func(i, j int) bool { return recordedBefore(mocks[i], mocks[j]) })
	i, commit, answered := bandPick(mocks, cs, bg, mockDb, statefulMember(member, ofBand))
	if answered != nil {
		return nil, nil, answered
	}
	return byMock[mocks[i]], commit, nil
}

// variableMember is, for the variable pass's stateful walk (pickHit), a
// recording the store stages that the call fits, whether or not its
// variables hold (fitVariables gives a hit or a problem): a spent recording
// of the call is one, whatever it bound.
func (h *HTTP) variableMember(ctx context.Context, input *req, scope varScope, headerNoise, userBodyNoise map[string][]string, urlNoise []string) func(*models.Mock) bool {
	var live *reqView
	return func(m *models.Mock) bool {
		if !hasVariables(m) {
			return false
		}
		if live == nil {
			v := liveView(input)
			live = &v
		}
		table, _ := scope.of(m)
		hit, problem := h.fitVariables(ctx, input, *live, m, table, headerNoise, userBodyNoise, urlNoise)
		return hit != nil || problem != nil
	}
}

// serveVariables answers the call with a hit: loads its response, renders its
// answers, commits its binds and consumes it. The binds are committed all or
// none, under one lock, before the mock is consumed, and only in the scope the
// call was checked in. retry is true when the call must be checked again: a
// test began since (a straggler of the previous test binds nothing in this
// one), another connection bound one of its variables or took one of its
// values meanwhile (the next check sees that, and either another recording
// answers the call or the call is a miss with that evidence), or another
// connection took the mock first. Nothing is consumed before the binds hold.
// A bind refused over a binding the check read (varHit.read) is no race but
// a disagreement of the check and the table: a problem, never a retry, which
// would be refused again forever.
func (h *HTTP) serveVariables(ctx context.Context, hit *varHit, mockDb integrations.MockMemDb) (served *models.Mock, problem *varProblem, retry bool, err error) {
	return h.serveVariablesBy(hit, mockDb, func(m *models.Mock) bool { return h.updateMock(ctx, m, mockDb, nil) })
}

// serveVariablesBy is serveVariables with consume taking the mock from the
// mock store (false: another connection took it first): the HTTP parser's
// updateMock, or another parser's own consume (VariableMatch). A background
// recording mockDb refuses the call (integrations.RefusesAnswer: the call is
// one of the open test's own) binds nothing and is refused as at its consume
// door: the call is checked again, and the recording's variables stay
// unbound for the call it is the recording of.
func (h *HTTP) serveVariablesBy(hit *varHit, mockDb integrations.MockMemDb, consume func(*models.Mock) bool) (served *models.Mock, problem *varProblem, retry bool, err error) {
	if integrations.RefusesAnswer(mockDb, hit.mock) {
		return nil, nil, true, nil
	}
	loaded, err := withResponse(hit.mock)
	if err != nil {
		return nil, nil, false, err
	}
	rendered, problem := h.renderAnswers(loaded, hit, hit.table)
	if problem != nil {
		return nil, problem, false, nil
	}
	if bad, with, stale, ok := hit.table.BindAll(hit.gen, hit.binds); !ok {
		if !stale && hit.read(with) {
			// Not another connection's bind meanwhile: the check passed
			// over a binding the table refuses it beside. Checking the call
			// again would pass and be refused again, forever.
			return nil, &varProblem{
				mock: hit.mock.Name, name: bad.Name, place: bad.Place, expected: "a value variable " + with.Name + " does not hold", actual: bad.Value,
				why:  fmt.Sprintf("this call binds variable %s at %s to %q, which the test's variables refuse beside variable %s (bound at %s %s to %q), though the check of the call let it", bad.Name, bad.Place, bad.Value, with.Name, with.Mock, with.Place, with.Value),
				next: "the check of the call and its bind disagree, which is a keploy bug: report it with this test set",
			}, false, nil
		}
		return nil, nil, true, nil
	}
	for _, b := range hit.binds {
		h.Logger.Debug("variable bound", zap.String("variable", b.Name), zap.String("value", b.Value),
			zap.String("recorded", b.Recorded), zap.String("mock", b.Mock), zap.String("place", b.Place))
	}
	if !consume(hit.mock) {
		return nil, nil, true, nil
	}
	return rendered, nil, false, nil
}

// read reports whether b was in force when the call was checked, as the check
// read it, or is one of the call's own binds: a bind refused over it is no
// race.
func (hit *varHit) read(b variables.Binding) bool {
	if v, ok := hit.saw[b.Name]; ok && v == b.Value {
		return true
	}
	for _, own := range hit.binds {
		if own.Name == b.Name && own.Value == b.Value {
			return true
		}
	}
	return false
}

// renderAnswers returns a copy of served (its response loaded) whose answer
// places hold their variables' values, with digest headers of the recorded
// body computed again over the new one.
func (h *HTTP) renderAnswers(served *models.Mock, hit *varHit, table *variables.Table) (*models.Mock, *varProblem) {
	var answers int
	for _, d := range hit.decls {
		answers += len(d.answer)
	}
	if answers == 0 {
		return served, nil
	}
	resp := served.Spec.HTTPResp
	if resp == nil {
		return nil, &varProblem{mock: served.Name, why: "it declares answer places but has no recorded response", next: "re-record the test"}
	}
	valueOf := func(name string) (variables.Binding, bool) {
		if b, ok := hit.local[name]; ok {
			return b, true
		}
		for _, b := range hit.binds {
			if b.Name == name {
				return b, true
			}
		}
		return table.Lookup(name)
	}
	header := maps.Clone(resp.Header)
	var edits []variables.Edit
	var words []wordPut
	changed := map[string]bool{}
	// An answer place as written is whole where the recorded response
	// holds it so, else a word address (variables.ReadPlace).
	inAnswer := func(p variables.Place) bool {
		if p.Part == variables.PartHeader {
			_, ok := headerKey(resp.Header, p.Name)
			return ok
		}
		_, ok := variables.JSONLeafAt(resp.Body, p.Path)
		return p.Part == variables.PartBody && ok
	}
	for _, d := range hit.decls {
		if len(d.answer) == 0 {
			continue
		}
		b, ok := valueOf(d.name)
		if !ok {
			// fitVariables leaves no answer without a value; this guards that.
			return nil, &varProblem{mock: served.Name, name: d.name, why: fmt.Sprintf("it answers variable %s, which no call has bound in this test", d.name), next: "re-record the test"}
		}
		for _, vp := range d.answer {
			if a, err := variables.ReadPlace(vp.raw, inAnswer); err == nil {
				vp.place, vp.at = a.Place, a
			}
			if !vp.whole() {
				words = append(words, wordPut{vp: vp, name: d.name, recorded: d.recorded, value: b.Value})
				continue
			}
			switch vp.place.Part {
			case variables.PartBody:
				e, why := variables.AnswerEdit(resp.Body, vp.place.Path, d.recorded, b.Value)
				if why != "" {
					return nil, &varProblem{mock: served.Name, name: d.name, place: vp.raw, expected: d.recorded, actual: b.Value,
						why: fmt.Sprintf("its answer place %s cannot hold variable %s: %s", vp.raw, d.name, why), next: "re-record the test"}
				}
				edits = append(edits, e)
			case variables.PartHeader:
				key, ok := headerKey(header, vp.place.Name)
				if !ok || header[key] != d.recorded || len(resp.HeaderLineLengths[key]) > 1 {
					return nil, &varProblem{mock: served.Name, name: d.name, place: vp.raw, expected: d.recorded,
						why: fmt.Sprintf("its recorded response does not hold variable %s's recorded value %q as the one value of %s", d.name, d.recorded, vp.raw), next: "re-record the test"}
				}
				header[key] = b.Value
				changed[strings.ToLower(key)] = true
			}
		}
	}
	edits, w, why := answerWords(resp, header, changed, edits, words)
	if why != "" {
		return nil, &varProblem{mock: served.Name, name: w.name, place: w.vp.raw, expected: w.recorded, actual: w.value,
			why: fmt.Sprintf("its answer place %s cannot hold variable %s: %s", w.vp.raw, w.name, why), next: "re-record the test"}
	}
	body, ok := variables.ApplyEdits(resp.Body, edits)
	if !ok {
		return nil, &varProblem{mock: served.Name, why: "two of its answer places overlap", next: "re-record the test"}
	}
	note := served.Spec.Metadata[models.MetadataBodyDigestsKey]
	if body != resp.Body {
		for k, v := range resp.Header {
			lk := strings.ToLower(k)
			if changed[lk] || !bodydigest.IsHeader(lk) {
				continue
			}
			if of, ok := bodydigest.Form(lk, v, resp.Body); ok {
				header[k] = of(body)
				continue
			}
			if bodydigest.Noted(note, lk) {
				// Recorded over the body as its server sent it (encoded): the
				// serializer recomputes it over the bytes sent
				// (buildMockResponseBytes).
				continue
			}
			h.Logger.Warn("a variable changed the body of a recorded answer, and keploy serves this header as recorded: it cannot tell how the value was computed",
				zap.String("mock", served.Name), zap.String("header", k))
		}
	}
	out := served.ShallowCopy()
	rc := *resp
	rc.Body = body
	rc.Header = header
	out.Spec.HTTPResp = &rc
	// A digest header a variable set holds the variable's value, which the
	// serializer must not recompute over the body.
	if trimmed := bodydigest.Without(note, changed); trimmed != note {
		meta := make(map[string]string, len(served.Spec.Metadata))
		for k, v := range served.Spec.Metadata {
			meta[k] = v
		}
		if trimmed == "" {
			delete(meta, models.MetadataBodyDigestsKey)
		} else {
			meta[models.MetadataBodyDigestsKey] = trimmed
		}
		out.Spec.Metadata = meta
	}
	return out, nil
}

// wordPut is a variable's value to answer at a word of a recorded answer's
// value (vp.at).
type wordPut struct {
	vp                    varPlace
	name, recorded, value string
}

// wordedText is one value of a recorded answer that words are put in.
type wordedText struct {
	leaf templates.Leaf // a body string's
	key  string         // a header's
	text string
	puts []wordPut
}

// answerWords writes each word into the answer: a word of a body string or of
// a header's one value, the recorded text holding the variable's recorded
// value at it, the words of one value in turn, each a word for a word
// (ReplaceWord reads it back), so that every later word address of the value
// still names its word.
// A value a whole place answers too (edits, changed) is written whole, where
// its text holds each word put there: the words then say nothing the whole
// does not; else the word is refused. Mongo's replay writes words so. It
// returns edits with the body's words, header with theirs, or the word and
// why it cannot be answered.
func answerWords(resp *models.HTTPResp, header map[string]string, changed map[string]bool, edits []variables.Edit, words []wordPut) ([]variables.Edit, wordPut, string) {
	texts := map[string]*wordedText{}
	var order []string
	for _, w := range words {
		var id, orig string
		t := &wordedText{}
		switch w.vp.place.Part {
		case variables.PartBody:
			l, ok := variables.JSONLeafAt(resp.Body, w.vp.place.Path)
			if !ok || l.Kind != templates.LeafString {
				return nil, w, "the recorded response holds no string there to hold a word of"
			}
			id, orig, t.leaf = "body "+strconv.Itoa(l.Start), l.Value(resp.Body), l
		case variables.PartHeader:
			key, ok := headerKey(resp.Header, w.vp.place.Name)
			if !ok || len(resp.HeaderLineLengths[key]) > 1 {
				return nil, w, "the recorded response holds no one value of that header"
			}
			id, orig, t.key = "header "+key, resp.Header[key], key
		default:
			return nil, w, "it is not a place of a response"
		}
		if got, ok := w.vp.at.WordOf(orig); !ok || !variables.SameValue(w.recorded, got, false) {
			return nil, w, fmt.Sprintf("the recorded response holds %q there, not the variable's recorded value", orig)
		}
		if texts[id] == nil {
			t.text = orig
			texts[id] = t
			order = append(order, id)
		}
		t = texts[id]
		out, ok := w.vp.at.ReplaceWord(t.text, w.value)
		if !ok {
			return nil, w, fmt.Sprintf("the value %q cannot stand at that word", w.value)
		}
		t.text = out
		t.puts = append(t.puts, w)
	}
	for _, id := range order {
		t := texts[id]
		whole, wrote := "", false
		if t.key != "" {
			whole, wrote = header[t.key], changed[strings.ToLower(t.key)]
		} else {
			for _, e := range edits {
				if e.Start == t.leaf.Start && e.End == t.leaf.End {
					wrote = json.Unmarshal([]byte(e.Raw), &whole) == nil
					if !wrote {
						return nil, t.puts[0], "a variable answers that value whole, as no string"
					}
				}
			}
		}
		if wrote {
			for _, w := range t.puts {
				if got, ok := w.vp.at.WordOf(whole); !ok || got != w.value {
					return nil, w, "another variable answers that value whole, with another word there"
				}
			}
			continue
		}
		if t.key != "" {
			header[t.key] = t.text
			changed[strings.ToLower(t.key)] = true
			continue
		}
		raw, err := json.Marshal(t.text)
		if err != nil {
			return nil, t.puts[0], "keploy cannot write it as JSON"
		}
		edits = append(edits, variables.Edit{Start: t.leaf.Start, End: t.leaf.End, Raw: string(raw)})
	}
	return edits, wordPut{}, ""
}

func headerKey(h map[string]string, name string) (string, bool) {
	var found string
	n := 0
	for k := range h {
		if strings.EqualFold(k, name) {
			found = k
			n++
		}
	}
	return found, n == 1
}

// variableTable is the replay's variable table, or nil when the mock store
// keeps none.
func variableTable(mockDb integrations.MockMemDb) *variables.Table {
	if vs, ok := mockDb.(integrations.VariableStore); ok {
		return vs.Variables()
	}
	return nil
}

// variableMismatchReport is the miss report of a call a recorded call fit
// except for a test variable: the variable, the place, the value it has in
// the test and the one the call sent, and what to do.
func variableMismatchReport(request *http.Request, liveBody []byte, dest, actualKey string, diag *matchDiag, httpMocks []*models.Mock, table *variables.Table) *models.MockMismatchReport {
	p := diag.variable
	var diffs []models.MockFieldDiff
	if p.place != "" && p.kind != "" {
		diffs = append(diffs, models.MockFieldDiff{Path: p.place, Kind: p.kind, Expected: p.expected, Actual: p.actual})
	}
	pool := diag.pool
	if pool == nil {
		pool = httpMocks
	}
	var closest *models.Mock
	for _, m := range pool {
		if m != nil && m.Name == p.mock && m.Spec.HTTPReq != nil {
			closest = m
			break
		}
	}
	if closest != nil {
		redactFieldDiffs(diffs, closest.Noise)
	}
	b := mismatch.NewReport(mismatch.ProtocolHTTP, actualKey).
		WithDestination(dest).
		WithComparedDestinations(comparedDestinations(pool, diag.candidates)).
		WithPhase(models.MatchPhaseVariable, diag.candidates).
		WithClosest(p.mock, diffs).
		WithDiff(p.diff()).
		WithNextSteps(p.next)
	if closest != nil {
		b = b.WithRenderedRequests(renderMockRequest(closest.Spec.HTTPReq, closest.Noise), renderLiveRequest(request, liveBody, closest.Noise))
	}
	r := b.Build()
	r.Given = p.given
	// The problem's own place, the path places the closest mock and the
	// mocks on this call's route declare, and the segments that hold a value
	// the test bound (routePathPlaces) hold the application's values: the
	// miss's shape writes them {} (models.MissShape).
	if models.IsPathPlace(p.place) {
		r.VariablePlaces = append(r.VariablePlaces, p.place)
	}
	for _, place := range routePathPlaces(request, closest, pool, table) {
		if !slices.Contains(r.VariablePlaces, place) {
			r.VariablePlaces = append(r.VariablePlaces, place)
		}
	}
	return r
}

// withRoutePlaces is r carrying, under the proven switch, the URL path places
// a test variable holds on the call (routePathPlaces): segments that hold the
// application's values, whatever else differs. The miss then names its path
// escaped, as a place counts segments, so its shape writes them {}
// (models.MissShape). A place of the closest mock off its route only writes
// more {}. With the switch off, r is as built.
func withRoutePlaces(r *models.MockMismatchReport, request *http.Request, closest *models.Mock, pool []*models.Mock, table *variables.Table) *models.MockMismatchReport {
	if !provenswitch.On() || r == nil || request.URL == nil {
		return r
	}
	if places := routePathPlaces(request, closest, pool, table); len(places) > 0 {
		r.ActualSummary = request.Method + " " + request.URL.EscapedPath()
		r.VariablePlaces = places
	}
	return r
}

// routePathPlaces are the URL path places a test variable holds on a call:
// those the closest mock declares; those of every mock in the pool whose
// route the call is on (sameRoute: its method and path, the segments it
// declares aside), so a closer recording without variables does not hide
// them; and every segment that holds a value the test's variables have
// (table), so a call made again after the mock that declares its place was
// consumed, and left the pool, does not keep it. Under the proven switch
// only.
func routePathPlaces(request *http.Request, closest *models.Mock, pool []*models.Mock, table *variables.Table) []string {
	out := models.DeclaredPathPlaces(closest)
	live := reqView{method: request.Method}
	if request.URL != nil {
		live.segments = pathSegments(request.URL.EscapedPath())
	}
	if table != nil {
		bound := map[string]bool{}
		for _, b := range table.Snapshot() {
			if b.Value != "" {
				bound[b.Value] = true
			}
		}
		for i, seg := range live.segments {
			value, err := url.PathUnescape(seg)
			if err == nil && bound[value] {
				if place := fmt.Sprintf("url.path[%d]", i); !slices.Contains(out, place) {
					out = append(out, place)
				}
			}
		}
	}
	for _, m := range pool {
		if m == nil || m.Spec.HTTPReq == nil || len(m.Spec.Variables) == 0 {
			continue
		}
		rec, ok := recordedView(m.Spec.HTTPReq)
		if !ok || !sameRoute(live, rec, m) {
			continue
		}
		for _, place := range models.DeclaredPathPlaces(m) {
			if !slices.Contains(out, place) {
				out = append(out, place)
			}
		}
	}
	return out
}

// exactOnly answers a call whose variables did not hold in the recording it
// fit, with a recording of this very call (a mock without variables), if one
// is in the pool: the strict comparison every other mock is matched with
// (strictServe, row S1). No lenient pass runs. retry is true when another
// connection took the mock first.
func (h *HTTP) exactOnly(ctx context.Context, input *req, pool []*models.Mock, mockDb integrations.MockMemDb, headerNoise, userBodyNoise map[string][]string, urlNoise []string, statefulMocks bool) (*models.Mock, bool, bool, error) {
	if len(pool) == 0 {
		return nil, false, false, nil
	}
	served, _, retry, err := h.strictServe(ctx, input, pool, pool, mockDb, h.newStrictNoise(headerNoise, userBodyNoise, urlNoise), statefulMocks)
	if err != nil || retry || served == nil {
		return nil, false, retry, err
	}
	return served, true, false, nil
}

// asRecordedForDiff is the live call to diff against m, a mock with variables
// that it did not fit: m's recorded values put back at its variables' places,
// when the call is to m's route and holds a value of the recorded JSON type
// there. Those places are not where the call differs (a variable takes a new
// value on every run), so the field diffs and their hint name only what does
// differ, and never suggest noise for a variable's place. It is used for the
// field diffs only: the report renders the call as it was received. A call
// to another route, or one whose variables cannot be read, is diffed as it is.
func asRecordedForDiff(request *http.Request, liveBody []byte, m *models.Mock) (*http.Request, []byte) {
	if !hasVariables(m) || m.Spec.HTTPReq == nil || request == nil || request.URL == nil {
		return request, liveBody
	}
	decls, bad := readDecls(m)
	rec, ok := recordedView(m.Spec.HTTPReq)
	if bad != "" || !ok {
		return request, liveBody
	}
	in := &req{method: request.Method, url: request.URL, header: request.Header, body: liveBody}
	live := liveView(in)
	if !sameRoute(live, rec, m) {
		return request, liveBody
	}
	for _, d := range decls {
		for _, vp := range d.requestPlaces() {
			r, okRec := rec.read(vp)
			l, okLive := live.read(vp)
			if !okRec || !okLive || l.kind != r.kind {
				continue
			}
			if v, ok := live.putBackAt(vp, r); ok {
				live = v
			}
		}
	}
	as := live.asReq(in)
	out := request.Clone(request.Context())
	out.URL = as.url
	out.Header = live.header
	return out, []byte(live.body)
}
