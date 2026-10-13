package http

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mismatch"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/util"
	"go.keploy.io/server/v3/pkg/bodydigest"
	"go.keploy.io/server/v3/pkg/models"
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
//     repeated recordings of one request answer in record order (the stateful
//     cursor). It is consumed as any mock, its binds are committed, and it is
//     served with each answer place holding its variable's value.
//
// No later pass sees such a mock. When one fits but its variables do not
// hold, only an exact recording of this very call may still answer it;
// otherwise the call is a miss whose evidence is the variable
// (MatchPhaseVariable).

// varPlace is one place of a variable in a mock, read.
type varPlace struct {
	raw   string
	place variables.Place
}

// varDecl is one variable of a mock, read.
type varDecl struct {
	name     string
	recorded string
	echo     bool
	bind     *varPlace
	use      []varPlace
	answer   []varPlace
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
		if !a.Spec.ReqTimestampMock.Equal(b.Spec.ReqTimestampMock) {
			return a.Spec.ReqTimestampMock.Before(b.Spec.ReqTimestampMock)
		}
		return a.Name < b.Name
	})
	return withVars, rest
}

// readDecls reads m's variables, by name, or says why one cannot be followed.
func readDecls(m *models.Mock) ([]varDecl, string) {
	names := make([]string, 0, len(m.Spec.Variables))
	for name := range m.Spec.Variables {
		names = append(names, name)
	}
	sort.Strings(names)
	parse := func(name, role, raw string, answer bool) (varPlace, string) {
		p, err := variables.Parse(raw)
		if err != nil {
			return varPlace{}, fmt.Sprintf("the %s place of variable %s: %v", role, name, err)
		}
		if answer && p.Part != variables.PartBody && p.Part != variables.PartHeader {
			return varPlace{}, fmt.Sprintf("the answer place %s of variable %s is not a place of a response (body.<path> or header.<name>)", raw, name)
		}
		return varPlace{raw: raw, place: p}, ""
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
		d := varDecl{name: name, recorded: mv.Recorded, echo: mv.Echo}
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
			key := vp.place.String()
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
}

func (h held) number() bool { return h.body && h.kind == templates.LeafNumber }

// empty reports whether the value cannot be a variable's: empty text, or a
// JSON null.
func (h held) empty() bool {
	return h.value == "" || (h.body && h.kind == templates.LeafNull)
}

// read returns the value at p, or false when the request has none there (or
// more than one: a repeated query parameter or header is no place).
func (v reqView) read(p variables.Place) (held, bool) {
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

// putBack returns v with the recorded value rec at p: the request as it would
// be had the variable kept its recorded value.
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
	out := &req{method: v.method, header: v.header, body: []byte(v.body), raw: input.raw}
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
	mock     string
	name     string
	place    string
	expected string
	actual   string
	why      string
	next     string
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
}

// variablePass returns the mocks of pool (in its order) that the live call
// fits and whose variables hold, and otherwise the evidence of the first that
// fits but whose variables do not hold, if any.
func (h *HTTP) variablePass(ctx context.Context, input *req, pool []*models.Mock, table *variables.Table, headerNoise, userBodyNoise map[string][]string, urlNoise []string) ([]*varHit, *varProblem, error) {
	live := liveView(input)
	var hits []*varHit
	var first *varProblem
	for _, m := range pool {
		if ctx.Err() != nil {
			return nil, nil, ctx.Err()
		}
		hit, problem := h.fitVariables(ctx, input, live, m, table, headerNoise, userBodyNoise, urlNoise)
		switch {
		case hit != nil:
			hits = append(hits, hit)
		case problem != nil && first == nil:
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
		// route: its method and URL path, a segment it declares a variable's
		// aside.
		if !sameRoute(live, rec, m) {
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

	// The variables' places, read on both sides; then the recorded values put
	// back into the live call.
	liveAt := map[string]held{}
	asRecorded := live
	var typeChange *varProblem
	for _, d := range decls {
		for _, vp := range d.requestPlaces() {
			r, ok := rec.read(vp.place)
			if !ok {
				return refuse(fmt.Sprintf("its recorded request holds no single value at %s (variable %s)", vp.raw, d.name))
			}
			if !variables.SameValue(d.recorded, r.value, r.number()) {
				return refuse(fmt.Sprintf("its recorded request holds %q at %s, not variable %s's recorded value %q", r.value, vp.raw, d.name, d.recorded))
			}
			l, ok := live.read(vp.place)
			if !ok {
				return nil, nil // the call has no such place: it is not this mock's call
			}
			if l.body && l.kind != r.kind && typeChange == nil {
				typeChange = &varProblem{
					mock: m.Name, name: d.name, place: vp.raw, expected: r.raw, actual: l.raw,
					why:  fmt.Sprintf("variable %s at %s: the call sent %s where the recording has %s, a value of another JSON type", d.name, vp.raw, l.raw, r.raw),
					next: "the application changed the type of the value it sends at " + vp.raw + ": if that is intended, re-record the test; otherwise fix the application",
				}
			}
			if asRecorded, ok = asRecorded.putBack(vp.place, r); !ok {
				return nil, nil
			}
			liveAt[vp.place.String()] = l
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

	// 2. The variables hold.
	hit := &varHit{mock: m, decls: decls, local: map[string]variables.Binding{}}
	taken := map[string]variables.Binding{} // this call's new bindings, by name
	for _, d := range decls {
		places := d.requestPlaces()
		if d.echo {
			v := liveAt[d.bind.place.String()]
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
			if !variables.SameValue(bound.Recorded, d.recorded, false) {
				// Recorded with another value than the one the test bound: a
				// call of another test (reusable mocks are seen by every
				// test), not this one.
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
				sent = liveAt[d.use[0].place.String()].value
			} else {
				place = d.answer[0].raw
			}
			return nil, &varProblem{
				mock: m.Name, name: d.name, place: place, actual: sent,
				why:  fmt.Sprintf("variable %s is used at %s before the call that binds it was made in this test", d.name, place),
				next: fmt.Sprintf("the call that first carries variable %s was not made, or not before this one: if the application now makes its calls in another order or no longer makes that call, re-record the test", d.name),
			}
		}
		v := liveAt[d.bind.place.String()]
		if v.empty() {
			return nil, emptyBind(m.Name, d, v)
		}
		if p := holdsAt(m.Name, d, places, v.value, m.Name, d.bind.raw, liveAt); p != nil {
			return nil, p
		}
		// Two variables are two values: a value the test bound already, as
		// another variable recorded with another value, is that one's.
		others := table.Snapshot()
		for _, b := range taken {
			others = append(others, b)
		}
		for _, b := range others {
			if b.Name != d.name && b.Value == v.value && b.Recorded != d.recorded {
				return nil, &varProblem{
					mock: m.Name, name: d.name, place: d.bind.raw, expected: "a value other than variable " + b.Name + "'s", actual: v.value,
					why:  fmt.Sprintf("this call binds variable %s at %s to %q, which is variable %s's value in this test (bound at %s %s); they were two values when the test was recorded", d.name, d.bind.raw, v.value, b.Name, b.Mock, b.Place),
					next: fmt.Sprintf("the application sent %s's value where it sent %s's when the test was recorded: fix the application, or re-record the test if that is intended", b.Name, d.name),
				}
			}
		}
		taken[d.name] = variables.Binding{Name: d.name, Value: v.value, Recorded: d.recorded, Mock: m.Name, Place: d.bind.raw}
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
		l := liveAt[vp.place.String()]
		if variables.SameValue(want, l.value, l.number()) {
			continue
		}
		verb := "sent"
		if d.bind != nil && vp.raw == d.bind.raw {
			verb = "sent, at a place that binds it,"
		}
		return &varProblem{
			mock: mock, name: d.name, place: vp.raw, expected: want, actual: l.value,
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
			if p, err := variables.Parse(raw); err == nil && p.Part == variables.PartPath {
				declared[p.Index] = true
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
// m's recorded request exactly, outside the known noise.
func (h *HTTP) fitsRecording(ctx context.Context, asRecorded *req, m *models.Mock, headerNoise, userBodyNoise map[string][]string, urlNoise []string) bool {
	matched, err := h.SchemaMatch(ctx, asRecorded, []*models.Mock{m}, headerNoise, urlNoise, false)
	if err != nil || len(matched) != 1 {
		return false
	}
	recBody := m.Spec.HTTPReq.Body
	if string(asRecorded.body) == recBody {
		return true
	}
	if !variables.IsJSONDoc(recBody) || !variables.IsJSONDoc(string(asRecorded.body)) {
		// A body that is not JSON holds no variable place: as recorded, or
		// equal outside the obfuscator's values.
		ok, _ := h.ExactBodyMatch(asRecorded.body, []*models.Mock{m})
		return ok
	}
	var noise variables.Noise
	for k, regs := range m.Spec.ReqBodyNoise {
		noise.Add(k, regs)
	}
	for k, regs := range userBodyNoise {
		noise.Add(k, regs)
	}
	if nc := util.NewNoiseChecker(m.Noise); nc != nil {
		noise.RecordedValueIsNoise = nc.IsNoisy
	}
	return variables.SameJSONUnder(recBody, string(asRecorded.body), noise)
}

// pickHit chooses the hit that answers: the first in record order, and among
// repeated recordings of that very request, the one the stateful cursor is at.
// commit advances the cursor once the call is answered (nil when none).
func (h *HTTP) pickHit(hits []*varHit, mockDb integrations.MockMemDb, statefulMocks bool) (*varHit, func()) {
	mocks := make([]*models.Mock, len(hits))
	byMock := make(map[*models.Mock]*varHit, len(hits))
	for i, hit := range hits {
		mocks[i] = hit.mock
		byMock[hit.mock] = hit
	}
	picked, commit := h.cursorPick(hits[0].mock, mocks, mockDb, statefulMocks)
	if hit, ok := byMock[picked]; ok {
		return hit, commit
	}
	return hits[0], nil
}

// serveVariables answers the call with a hit: loads its response, renders its
// answers, commits its binds and consumes it. The binds are committed all or
// none, under one lock, before the mock is consumed: a variable another
// connection bound meanwhile to another value, or a value another variable
// took meanwhile, makes the call a miss with nothing consumed. retry is true
// when another connection took the mock first (the binds stay: they are what
// the app sent at its binding places).
func (h *HTTP) serveVariables(ctx context.Context, hit *varHit, mockDb integrations.MockMemDb, table *variables.Table) (served *models.Mock, problem *varProblem, retry bool, err error) {
	loaded, err := withResponse(hit.mock)
	if err != nil {
		return nil, nil, false, err
	}
	rendered, problem := h.renderAnswers(loaded, hit, table)
	if problem != nil {
		return nil, problem, false, nil
	}
	if bad, with, ok := table.BindAll(hit.binds); !ok {
		why := fmt.Sprintf("variable %s was bound to %q at %s %s while this call, which binds it to %q at %s, was being answered", bad.Name, with.Value, with.Mock, with.Place, bad.Value, bad.Place)
		if with.Name != bad.Name {
			why = fmt.Sprintf("this call binds variable %s at %s to %q, which variable %s took meanwhile (at %s %s); they were two values when the test was recorded", bad.Name, bad.Place, bad.Value, with.Name, with.Mock, with.Place)
		}
		return nil, &varProblem{
			mock: hit.mock.Name, name: bad.Name, place: bad.Place, expected: with.Value, actual: bad.Value,
			why: why, next: "two calls of the test bound its variables at once in a way the recording did not: fix the application, or re-record the test if that is intended",
		}, false, nil
	}
	for _, b := range hit.binds {
		h.Logger.Debug("variable bound", zap.String("variable", b.Name), zap.String("value", b.Value),
			zap.String("recorded", b.Recorded), zap.String("mock", b.Mock), zap.String("place", b.Place))
	}
	if !h.updateMock(ctx, hit.mock, mockDb, nil) {
		return nil, nil, true, nil
	}
	return rendered, nil, false, nil
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
	changed := map[string]bool{}
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
	body, ok := variables.ApplyEdits(resp.Body, edits)
	if !ok {
		return nil, &varProblem{mock: served.Name, why: "two of its answer places overlap", next: "re-record the test"}
	}
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
			h.Logger.Warn("a variable changed the body of a recorded answer, and keploy serves this header as recorded: it cannot tell how the value was computed",
				zap.String("mock", served.Name), zap.String("header", k))
		}
	}
	out := served.ShallowCopy()
	rc := *resp
	rc.Body = body
	rc.Header = header
	out.Spec.HTTPResp = &rc
	return out, nil
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
func variableMismatchReport(request *http.Request, liveBody []byte, dest, actualKey string, diag *matchDiag, httpMocks []*models.Mock) *models.MockMismatchReport {
	p := diag.variable
	var diffs []models.MockFieldDiff
	if p.place != "" {
		diffs = append(diffs, models.MockFieldDiff{Path: p.place, Kind: models.DiffKindVariable, Expected: p.expected, Actual: p.actual})
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
	return b.Build()
}

// exactOnly answers a call whose variables did not hold in the recording it
// fit, with an exact recording of this very call (a mock without variables),
// if one is in the pool: the URL as recorded (configured url noise only) and
// the body as recorded (the obfuscator's values aside). No lenient pass runs.
// retry is true when another connection took the mock first.
func (h *HTTP) exactOnly(ctx context.Context, input *req, pool []*models.Mock, mockDb integrations.MockMemDb, headerNoise map[string][]string, urlNoise []string, statefulMocks bool) (*models.Mock, bool, bool, error) {
	if len(pool) == 0 {
		return nil, false, false, nil
	}
	schemaMatched, err := h.SchemaMatch(ctx, input, pool, headerNoise, urlNoise, false)
	if err != nil || len(schemaMatched) == 0 {
		return nil, false, false, err
	}
	ok, best := h.ExactBodyMatch(input.body, schemaMatched)
	if !ok {
		return nil, false, false, nil
	}
	best, commitCursor := h.cursorPick(best, schemaMatched, mockDb, statefulMocks)
	served, claimed, err := h.claim(ctx, best, mockDb, nil, nil)
	if err != nil {
		return nil, false, false, err
	}
	if !claimed {
		return nil, false, true, nil
	}
	if commitCursor != nil {
		commitCursor()
	}
	return served, true, false, nil
}

// asRecordedForDiff is the live call to diff against m, a mock with variables
// that it did not fit: m's recorded values put back at its variables' places.
// Those places are not where the call differs (a variable takes a new value on
// every run), so the diff and its hint name only what does differ, and never
// suggest noise for a variable's place. A call without such a mock, or one
// whose variables cannot be read, is diffed as it is.
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
	for _, d := range decls {
		for _, vp := range d.requestPlaces() {
			r, okRec := rec.read(vp.place)
			if _, okLive := live.read(vp.place); !okRec || !okLive {
				continue
			}
			if v, ok := live.putBack(vp.place, r); ok {
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
