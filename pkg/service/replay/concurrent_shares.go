package replay

import (
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"

	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// Shared recordings, under the proven switch (keploy record's full round
// only, config.Test.ShareConcurrent). A recording made while several tests'
// requests were open is listed under the test whose request came first
// (models.DeriveWindowMappings), which time alone cannot tell made it. Its
// entry names its concurrency group (models.MockEntry.Group,
// models.Mapping.Groups): the tests whose windows held it.
//
//   - Plan. A grouped recording whose kind registered its keys
//     (models.ConcurrentKeysOf) and that is shareable is a candidate of every
//     test of its group, and required of none: each test's own (definite)
//     recordings are what it lists less those. A candidate answers at most
//     once in the staging, for whichever of its tests makes its call first
//     (the agent's claims); the test's own answer before it.
//   - Settlement, at the set's end, before the per-test evidence passes and
//     every hook: the call each candidate answered pins it to the test that
//     made it (models.SetShares.Pins), and every case sharing cannot prove
//     charges the tests it may be (a data-plane miss with the reason
//     models.MissConcurrentAmbiguous or MissConcurrentConflict, or an
//     unconsumed mock), each charged test FAILED. Every recording ends
//     pinned, owned by a charged test, or shared (owned by none, as a call no
//     kept test may have made).
//   - keploy record's judging writes the settlement onto mappings.yaml before
//     its confirming round, which, like every other replay, serves each
//     recording to the test that lists it.
//
// Every other replay never shares: it serves the recorded ownership, and the
// misses and unconsumed recordings of a test recorded beside others say so
// (annotateConcurrent).

// sharing reports whether this replay shares the recordings made while
// several tests' requests were open between those tests: keploy record's full
// round (config.Test.ShareConcurrent, beside Learn), under the proven switch.
func (r *Replayer) sharing() bool {
	return r.learning() && r.config.Test.ShareConcurrent
}

// concurrentServer is the instrumentation that says whether its agent serves
// a test the recordings it shares (the keploy agent client: what its last
// StoreMocks answer said, models.ExactConcurrentHeader).
type concurrentServer interface {
	ServesConcurrent() bool
}

// agentServesConcurrent reports whether the agent the set's mocks were just
// stored on said it serves shared recordings.
func (r *Replayer) agentServesConcurrent() bool {
	cs, ok := r.instrumentation.(concurrentServer)
	return ok && cs.ServesConcurrent()
}

// planShares narrows, for keploy record's full round, the set's plan to share
// the recordings made while several tests' requests were open between those
// tests (exactPlan.shareConcurrent), when the agent the set's mocks were just
// stored on serves them; it returns mappings as each test is then required
// to make them. Any other replay, a set that states no concurrency, or an
// agent that does not say it serves them (said once, with why) keeps the
// recorded ownership whole: mappings as they are.
func (r *Replayer) planShares(testSetID string, mappings map[string][]models.MockEntry, cases []*models.TestCase) map[string][]models.MockEntry {
	if r.exact == nil || r.exact.share == nil || !r.sharing() {
		return mappings
	}
	if !r.agentServesConcurrent() {
		r.logger.Warn("keploy's agent does not serve a test the recordings it shares with the tests recorded beside it: each recording made while several tests' requests were open stays the test whose request came first",
			zap.String("testSetID", testSetID),
			zap.String("next_step", "run keploy record with a keploy agent of the same version"))
		return mappings
	}
	return r.exact.shareConcurrent(mappings, cases)
}

// setConcurrency is what a set's mapping says of its concurrency.
type setConcurrency struct {
	groups map[string]models.ConcurrencyGroup
	// entries are the grouped entries, and the marked ones, by mock name.
	entries map[string]groupedEntry
	// ofTest are the groups that name each test, in mapping order.
	ofTest map[string][]string
}

// groupedEntry is one entry of a mapping that names a group or a marker.
type groupedEntry struct {
	entry models.MockEntry
	// listedBy is the test that lists it, "" for one Mapping.Shared lists.
	listedBy string
}

// concurrencyOf reads a set's concurrency from its mapping: groups, the
// tests' entries and the shared ones. nil when it states none.
func concurrencyOf(groups []models.ConcurrencyGroup, tests map[string][]models.MockEntry, shared []models.MockEntry) *setConcurrency {
	if len(groups) == 0 && len(shared) == 0 {
		marked := false
		for _, entries := range tests {
			for _, e := range entries {
				if e.Unresolved != "" {
					marked = true
				}
			}
		}
		if !marked {
			return nil
		}
	}
	c := &setConcurrency{groups: map[string]models.ConcurrencyGroup{}, entries: map[string]groupedEntry{}, ofTest: map[string][]string{}}
	for _, g := range groups {
		c.groups[g.ID] = g
		for _, t := range g.Tests {
			c.ofTest[t] = append(c.ofTest[t], g.ID)
		}
	}
	for _, test := range keysSorted(tests) {
		for _, e := range tests[test] {
			if e.Group != "" || e.Unresolved != "" {
				if _, dup := c.entries[e.Name]; !dup {
					c.entries[e.Name] = groupedEntry{entry: e, listedBy: test}
				}
			}
		}
	}
	for _, e := range shared {
		if _, dup := c.entries[e.Name]; !dup && e.Name != "" {
			c.entries[e.Name] = groupedEntry{entry: e}
		}
	}
	return c
}

// group is the group of mock name, and whether it names a known one.
func (c *setConcurrency) group(name string) (models.ConcurrencyGroup, bool) {
	if c == nil {
		return models.ConcurrencyGroup{}, false
	}
	e, ok := c.entries[name]
	if !ok || e.entry.Group == "" {
		return models.ConcurrencyGroup{}, false
	}
	g, ok := c.groups[e.entry.Group]
	return g, ok
}

// with are the other tests of the groups that name test, in window order of
// their groups, each once, and how many the recording did not keep.
func (c *setConcurrency) with(test string) ([]string, int) {
	if c == nil {
		return nil, 0
	}
	var out []string
	seen := map[string]bool{test: true}
	unkept := 0
	for _, id := range c.ofTest[test] {
		g := c.groups[id]
		unkept = max(unkept, g.Unkept)
		for _, t := range g.Tests {
			if !seen[t] {
				seen[t] = true
				out = append(out, t)
			}
		}
	}
	return out, unkept
}

// shareInfo is what the shares read of one held recording a kind keys.
type shareInfo struct {
	mock      *models.Mock
	keys      models.ConcurrentKeys
	statement string
	shareable bool
	// whyNot is why it is not shareable.
	whyNot    string
	positions map[string]string
}

// shareUse is one consume of a recording the shares read: by which test, in
// which of its runs, and what its consume said (what the learn step learned
// on it).
type shareUse struct {
	test  string
	run   int
	state models.MockState
}

// sharePlan is the plan's part of the shares: what the mapping says, what
// each held recording the shares read is, and, at run time, who consumed
// what.
type sharePlan struct {
	conc *setConcurrency
	// info is every held recording a kind keys, by name.
	info map[string]*shareInfo
	// sharing: the plan shares (keploy record's full round, an agent that
	// serves shared recordings).
	sharing bool
	// cands are each test's shared recordings, in recorded order.
	cands map[string][]string
	// candOf are the tests each shared recording is a candidate of, in
	// window order.
	candOf map[string][]string
	// definite are each test's own recordings the kinds key (its view's
	// recordings of the statements the shares compare).
	definite map[string][]string
	// worker are the worker-scope recordings the kinds key.
	worker []string
	// inputs and notLearnable are what the learn step reads of each test.
	inputs       map[string][]string
	notLearnable map[string][]string

	// runs counts the times each test was opened in the set's run.
	runs map[string]int
	// served are the tests opened with shared recordings.
	served map[string]bool
	// unread are the tests whose consumption, in their latest run, could
	// not be read; read, the run each test's consumption was last read in.
	unread map[string]bool
	read   map[string]int
	// uses are the consumes of the recordings the shares read, by name.
	uses map[string][]shareUse
	// settled is what the set's end settled (settleShares), for the hook.
	settled models.SetShares
}

// newSharePlan is the plan's shares for a set whose concurrency is conc,
// whose test cases' mappings are mappings, whose worker scope is worker, and
// whose held recordings are stored. nil when the set states no concurrency.
func newSharePlan(conc *setConcurrency, worker []models.MockEntry, stored ...[]*models.Mock) *sharePlan {
	if conc == nil {
		return nil
	}
	sp := &sharePlan{conc: conc, info: map[string]*shareInfo{}, runs: map[string]int{}, served: map[string]bool{}, unread: map[string]bool{}, read: map[string]int{}, uses: map[string][]shareUse{}}
	for _, list := range stored {
		for _, mk := range list {
			if mk == nil || mk.Name == "" || sp.info[mk.Name] != nil || mk.IsProtocolPhase() {
				continue
			}
			keys, ok := models.ConcurrentKeysOf(mk.Kind)
			if !ok {
				continue
			}
			st, ok := keys.Statement(mk)
			if !ok {
				continue
			}
			in := &shareInfo{mock: mk, keys: keys, statement: st, positions: keys.Positions(mk)}
			in.shareable, in.whyNot = keys.Shareable(mk)
			sp.info[mk.Name] = in
		}
	}
	for _, e := range worker {
		if sp.info[e.Name] != nil {
			sp.worker = append(sp.worker, e.Name)
		}
	}
	return sp
}

// candidate reports whether the grouped entry name is shared between its
// group's tests: a recording the agent holds, of a kind that keys it, that
// is shareable, and that no replay has marked unresolved.
func (sp *sharePlan) candidate(name string) bool {
	if sp == nil {
		return false
	}
	e, ok := sp.conc.entries[name]
	if !ok || e.entry.Group == "" || e.entry.Unresolved != "" {
		return false
	}
	if _, ok := sp.conc.group(name); !ok {
		return false
	}
	in := sp.info[name]
	return in != nil && in.shareable
}

// shareConcurrent narrows the plan to share the set's grouped recordings
// between their groups' tests (keploy record's full round, with an agent that
// serves them): each test owns its definite recordings only, and is served
// its candidates after them; a shared recording (models.Mapping.Shared) is
// in no pool. It returns mappings with each test's definite entries only,
// which is what each test is required to make. cases are the set's test
// cases, whose requests give the learn step their inputs.
func (p *exactPlan) shareConcurrent(mappings map[string][]models.MockEntry, cases []*models.TestCase) map[string][]models.MockEntry {
	sp := p.share
	if sp == nil {
		return mappings
	}
	sp.sharing = true
	sp.cands, sp.candOf, sp.definite = map[string][]string{}, map[string][]string{}, map[string][]string{}
	narrowed := make(map[string][]models.MockEntry, len(mappings))
	for test, entries := range mappings {
		var keep []models.MockEntry
		for _, e := range entries {
			if !sp.candidate(e.Name) {
				keep = append(keep, e)
				if sp.info[e.Name] != nil {
					sp.definite[test] = append(sp.definite[test], e.Name)
				}
			}
		}
		if keep == nil {
			keep = []models.MockEntry{}
		}
		narrowed[test] = keep
	}
	// The candidates of each test, in recorded order.
	var names []string
	for name := range sp.conc.entries {
		if sp.candidate(name) {
			names = append(names, name)
		}
	}
	sort.SliceStable(names, func(i, j int) bool {
		a, b := sp.info[names[i]].mock, sp.info[names[j]].mock
		if !a.Spec.ReqTimestampMock.Equal(b.Spec.ReqTimestampMock) {
			return a.Spec.ReqTimestampMock.Before(b.Spec.ReqTimestampMock)
		}
		return names[i] < names[j]
	})
	shared := map[string]bool{}
	for _, name := range names {
		g, _ := sp.conc.group(name)
		for _, t := range g.Tests {
			sp.cands[t] = append(sp.cands[t], name)
		}
		sp.candOf[name] = append([]string(nil), g.Tests...)
		if sp.conc.entries[name].listedBy == "" {
			shared[name] = true
		}
	}
	// What each test owns: its definite recordings the agent holds.
	for test, entries := range narrowed {
		owned := make([]string, 0, len(entries))
		held := map[string]bool{}
		for _, n := range p.owned[test] {
			held[n] = true
		}
		for _, e := range entries {
			if held[e.Name] {
				owned = append(owned, e.Name)
			}
		}
		p.owned[test] = owned
	}
	if len(shared) > 0 {
		pool := p.pool[:0:0]
		for _, n := range p.pool {
			if !shared[n] {
				pool = append(pool, n)
			}
		}
		p.pool = pool
	}
	p.ownsTestMocks = p.ownsTestMocks || len(names) > 0
	sp.inputs = map[string][]string{}
	for _, tc := range cases {
		if tc != nil && len(sp.cands[tc.Name]) > 0 {
			sp.inputs[tc.Name] = requestInputs(tc)
		}
	}
	sp.notLearnable = sp.notLearnableOf()
	return narrowed
}

// notLearnableOf is, by test, its candidates the learn step may never pair a
// call of it with: those holding, at a place that tells their statement's
// recordings apart (a place where another recording of the statement holds
// another value), a whole value another test of their group sent in its
// request that this test's request does not hold.
func (sp *sharePlan) notLearnableOf() map[string][]string {
	// values: by statement and place, the distinct values its recordings
	// hold there.
	values := map[string]map[string]map[string]bool{}
	for _, in := range sp.info {
		byPlace := values[in.statement]
		if byPlace == nil {
			byPlace = map[string]map[string]bool{}
			values[in.statement] = byPlace
		}
		for k, v := range in.positions {
			if byPlace[k] == nil {
				byPlace[k] = map[string]bool{}
			}
			byPlace[k][v] = true
		}
	}
	inputSet := map[string]map[string]bool{}
	for t, vs := range sp.inputs {
		set := map[string]bool{}
		for _, v := range vs {
			set[v] = true
		}
		inputSet[t] = set
	}
	out := map[string][]string{}
	for test, cands := range sp.cands {
		own := inputSet[test]
		for _, name := range cands {
			in := sp.info[name]
			refused := false
			for _, k := range keysSorted(in.positions) {
				v := in.positions[k]
				if v == "" || len(values[in.statement][k]) < 2 || own[v] {
					continue
				}
				for _, y := range sp.candOf[name] {
					if y != test && inputSet[y][v] {
						refused = true
						break
					}
				}
				if refused {
					break
				}
			}
			if refused {
				out[test] = append(out[test], name)
			}
		}
	}
	return out
}

// requestInputs are the whole leaf values of a test case's request, as the
// HTTP learn step reads leaves: its URL path segments, its query values, its
// form values and its JSON body's scalars; for gRPC, the scalars of its
// messages. Request values: compared, never logged.
func requestInputs(tc *models.TestCase) []string {
	if tc == nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	add := func(v string) {
		if v != "" && !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	switch tc.Kind {
	case models.HTTP:
		req := tc.HTTPReq
		path, query := req.URL, ""
		if i := strings.IndexByte(path, '?'); i >= 0 {
			path, query = path[:i], path[i+1:]
		}
		if i := strings.Index(path, "://"); i >= 0 {
			path = path[i+3:]
			if j := strings.IndexByte(path, '/'); j >= 0 {
				path = path[j:]
			} else {
				path = ""
			}
		}
		for _, seg := range strings.Split(path, "/") {
			add(unescapeLoose(seg))
		}
		for _, kv := range strings.Split(query, "&") {
			if i := strings.IndexByte(kv, '='); i >= 0 {
				add(unescapeLoose(kv[i+1:]))
			}
		}
		for _, v := range req.URLParams {
			add(v)
		}
		for _, f := range req.Form {
			for _, v := range f.Values {
				add(v)
			}
		}
		if v, ok := learn.DecodeJSON([]byte(req.Body)); ok {
			leaves := map[string]learn.Leaf{}
			learn.JSONLeaves("body", v, leaves)
			for _, k := range keysSorted(leaves) {
				add(leaves[k].Value)
			}
		}
	case models.GRPC_EXPORT:
		for _, msg := range append([]models.GrpcLengthPrefixedMessage{tc.GrpcReq.Body}, tc.GrpcReq.AdditionalMessages...) {
			for _, m := range grpcScalar.FindAllStringSubmatch(msg.DecodedData, -1) {
				if m[1] != "" {
					add(m[1])
				} else {
					add(m[2])
				}
			}
		}
	}
	return out
}

// grpcScalar is a quoted string, or a bare value after a field's colon, of a
// gRPC message as keploy writes it decoded.
var grpcScalar = regexp.MustCompile(`"((?:[^"\\]|\\.)*)"|:\s*([^\s{}"\[\],]+)`)

// unescapeLoose is s with its percent escapes undone, s itself when it has
// one that is not.
func unescapeLoose(s string) string {
	if !strings.ContainsRune(s, '%') && !strings.ContainsRune(s, '+') {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c == '+':
			b.WriteByte(' ')
		case c == '%' && i+2 < len(s) && isHex(s[i+1]) && isHex(s[i+2]):
			b.WriteByte(unhex(s[i+1])<<4 | unhex(s[i+2]))
			i += 2
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

func isHex(c byte) bool {
	return '0' <= c && c <= '9' || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F'
}

func unhex(c byte) byte {
	switch {
	case '0' <= c && c <= '9':
		return c - '0'
	case 'a' <= c && c <= 'f':
		return c - 'a' + 10
	}
	return c - 'A' + 10
}

// concurrentServing is what test name is served of the shares: its
// candidates, those the learn step may not pair a call of it with, and its
// request's inputs; none when the plan does not share.
func (p *exactPlan) concurrentServing(name string) (cands, notLearnable, inputs []string) {
	if p == nil || p.share == nil || !p.share.sharing {
		return nil, nil, nil
	}
	sp := p.share
	return append([]string(nil), sp.cands[name]...), append([]string(nil), sp.notLearnable[name]...), append([]string(nil), sp.inputs[name]...)
}

// noteRun notes that test name was opened again: its run number, which
// what it consumes from now is filed under.
func (sp *sharePlan) noteRun(name string, served bool) {
	if sp == nil {
		return
	}
	sp.runs[name]++
	delete(sp.unread, name)
	if served {
		sp.served[name] = true
	}
}

// noteShares notes what test consumed in its current run, of the recordings
// the shares read; readErr is why its consumption could not be read, nil
// when it was.
func (p *exactPlan) noteShares(test string, consumed []models.MockState, readErr error) {
	if p == nil || p.share == nil || !p.share.sharing {
		return
	}
	sp := p.share
	if readErr != nil {
		sp.unread[test] = true
		return
	}
	run := sp.runs[test]
	sp.read[test] = run
	for _, st := range consumed {
		if sp.info[st.Name] == nil {
			continue
		}
		sp.uses[st.Name] = append(sp.uses[st.Name], shareUse{test: test, run: run, state: st})
	}
}

// learnedAt is the places the learn step learned on a consumed recording: its
// noise, and the bound places of its echo variables, sorted.
func learnedAt(st models.MockState) []string {
	set := map[string]bool{}
	for k := range st.ReqBodyNoise {
		set[k] = true
	}
	for _, v := range st.Variables {
		if v.Echo && v.Bind != "" {
			set[v.Bind] = true
		}
	}
	return keysSorted(set)
}

// echoedAt is the bound places of a consumed recording's echo variables.
func echoedAt(st models.MockState) []string {
	set := map[string]bool{}
	for _, v := range st.Variables {
		if v.Echo && v.Bind != "" {
			set[v.Bind] = true
		}
	}
	return keysSorted(set)
}

// subset reports whether every element of a is in b.
func subset(a, b []string) bool {
	in := make(map[string]bool, len(b))
	for _, x := range b {
		in[x] = true
	}
	for _, x := range a {
		if !in[x] {
			return false
		}
	}
	return true
}

// shareCharge is what the settlement charges a test with.
type shareCharge struct {
	calls      []models.UnmatchedCall
	unconsumed []models.UnconsumedMock
}

// settlement is the work of settleShares.
type settlement struct {
	p       *exactPlan
	sp      *sharePlan
	lookup  map[string]mockDisplayInfo
	ran     map[string]bool
	charges map[string]*shareCharge
	shares  models.SetShares
	// resolved are the shared recordings the settlement placed (pinned,
	// moved, or left in Mapping.Shared).
	resolved map[string]bool
	// ambiguous are the shared recordings of an ambiguous class.
	ambiguous map[string]bool
}

// charge charges test with call.
func (s *settlement) charge(test string, call models.UnmatchedCall) {
	c := s.charges[test]
	if c == nil {
		c = &shareCharge{}
		s.charges[test] = c
	}
	for _, have := range c.calls {
		if have.ClosestMock == call.ClosestMock && have.Reason == call.Reason {
			return
		}
	}
	c.calls = append(c.calls, call)
}

// chargeUnconsumed charges test with a shared recording none of its tests made.
func (s *settlement) chargeUnconsumed(test string, u models.UnconsumedMock) {
	c := s.charges[test]
	if c == nil {
		c = &shareCharge{}
		s.charges[test] = c
	}
	c.unconsumed = append(c.unconsumed, u)
}

// call is the charge of a shared recording mock's call, with reason and note.
func (s *settlement) call(mock, reason string, note *models.ConcurrentNote, why string) models.UnmatchedCall {
	info := s.lookup[mock]
	protocol, summary := info.protocol, info.summary
	if in := s.sp.info[mock]; in != nil && in.keys.Describe != nil {
		if p, sum := in.keys.Describe(in.mock); p != "" {
			protocol, summary = p, sum
		}
	}
	if in := s.sp.info[mock]; in != nil {
		if protocol == "" {
			protocol = string(in.mock.Kind)
		}
		if summary == "" {
			summary = models.MockSummaryFromSpec(in.mock)
		}
	}
	return models.UnmatchedCall{
		Protocol:      protocol,
		ActualSummary: summary,
		ClosestMock:   mock,
		Kind:          models.MissInTest,
		Reason:        reason,
		Concurrent:    note,
		NextSteps:     why + "; record these tests one request at a time",
	}
}

// note is the concurrency note of mock for test.
func (s *settlement) note(test, mock string) *models.ConcurrentNote {
	g, _ := s.sp.conc.group(mock)
	n := &models.ConcurrentNote{Unkept: g.Unkept}
	for _, t := range g.Tests {
		if t != test {
			n.With = append(n.With, t)
		}
	}
	if o := s.sp.conc.entries[mock].listedBy; o != "" && o != test {
		n.AttributedTo = o
	}
	return n
}

// owner is where shared recording name stays when the settlement moves it to
// no test: the test that lists it when the settlement charged it, else the
// first of its tests the settlement charged, else "" (none was).
func (s *settlement) owner(name string, prefer ...string) string {
	for _, t := range prefer {
		if t != "" && s.charges[t] != nil {
			return t
		}
	}
	if o := s.sp.conc.entries[name].listedBy; o != "" && s.charges[o] != nil {
		return o
	}
	for _, t := range s.sp.candOf[name] {
		if s.charges[t] != nil {
			return t
		}
	}
	return ""
}

// move moves shared recording name to test, with marker.
func (s *settlement) move(name, test, marker string) {
	if s.shares.Moves == nil {
		s.shares.Moves = map[string]models.ShareMove{}
	}
	s.shares.Moves[name] = models.ShareMove{To: test, Marker: marker}
	s.resolved[name] = true
}

// settleShares settles, at the set's end of keploy record's full round, which
// test made each shared recording's call (the package comment's settlement),
// and charges results with what sharing cannot prove: each charged test is
// FAILED, with the charge's category, its call a data-plane miss
// (MissConcurrentAmbiguous, MissConcurrentConflict) or an unconsumed mock,
// which every evidence reader holds. consumers are the tests that consumed
// each mock; complete says the set's run completed; lookup is the report's
// mock lookup. It returns the tests it charged, sorted, and keeps what it
// settled for the set's end hook (SetSharesHook). A no-op with the switch
// off, and for a plan that does not share.
func (p *exactPlan) settleShares(results []models.TestResult, consumers map[string][]string, complete bool, lookup map[string]mockDisplayInfo) []string {
	if !provenswitch.On() || p == nil || p.share == nil || !p.share.sharing {
		return nil
	}
	sp := p.share
	if len(sp.served) == 0 {
		sp.settled = models.SetShares{State: models.SharesNone}
		return nil
	}
	if !complete {
		sp.settled = models.SetShares{State: models.SharesIncomplete, Why: "the test set's run did not complete"}
		return nil
	}
	for _, t := range keysSorted(sp.served) {
		if sp.unread[t] || sp.read[t] != sp.runs[t] {
			sp.settled = models.SetShares{State: models.SharesIncomplete, Why: "what " + t + " consumed could not be read"}
			return nil
		}
	}
	s := &settlement{p: p, sp: sp, lookup: lookup, ran: map[string]bool{}, charges: map[string]*shareCharge{},
		shares: models.SetShares{State: models.SharesComplete}, resolved: map[string]bool{}, ambiguous: map[string]bool{}}
	for _, r := range results {
		if r.Status != models.TestStatusIgnored {
			s.ran[r.TestCaseID] = true
		}
	}
	var cands []string
	for name := range sp.candOf {
		cands = append(cands, name)
	}
	sort.Strings(cands)

	// Each candidate's consumer: one test, on its latest run. A candidate
	// consumed by several tests, or by its test on a run it did not end on
	// (a retry that did not make it again), is unresolved.
	consumer := map[string]string{}
	state := map[string]models.MockState{}
	retried := map[string]bool{}
	for _, name := range cands {
		uses := sp.uses[name]
		if len(uses) == 0 {
			continue
		}
		tests := map[string]bool{}
		last := true
		for _, u := range uses {
			tests[u.test] = true
		}
		t := uses[len(uses)-1].test
		if len(tests) == 1 {
			lastRun := sp.runs[t]
			consumedLast := false
			for _, u := range uses {
				if u.run == lastRun {
					consumedLast = true
				}
			}
			last = consumedLast
		}
		if len(tests) > 1 || !last {
			retried[name] = true
			continue
		}
		consumer[name] = t
		state[name] = uses[len(uses)-1].state
	}

	// The components: the recordings of one statement one test's view
	// can show both of are linked (its own, its candidates, the worker
	// scope's), closed transitively.
	uf := newUnionFind()
	for _, test := range keysSorted(s.ran) {
		byStatement := map[string]string{}
		view := append(append(append([]string(nil), sp.definite[test]...), sp.cands[test]...), sp.worker...)
		for _, name := range view {
			in := sp.info[name]
			if in == nil {
				continue
			}
			uf.add(name)
			if first, ok := byStatement[in.statement]; ok {
				uf.union(first, name)
			} else {
				byStatement[in.statement] = name
			}
		}
	}
	// Each consumed recording's learned places, whoever consumed it.
	learned := map[string][]string{}
	echoed := map[string][]string{}
	for name, uses := range sp.uses {
		if len(uses) == 0 {
			continue
		}
		st := uses[len(uses)-1].state
		if l := learnedAt(st); len(l) > 0 {
			learned[name] = l
			echoed[name] = echoedAt(st)
		}
	}
	members := uf.components()
	isCand := func(name string) bool { _, ok := sp.candOf[name]; return ok }
	componentOf := map[string]int{}
	for i, names := range members {
		for _, n := range names {
			componentOf[n] = i
		}
	}

	// Classes: in each component, the recordings with equal request keys
	// once every place learned in the component is excused. One that holds
	// a candidate and whose answers differ is ambiguous.
	classNote := map[string][]string{}
	for _, names := range members {
		lset, eset := map[string]bool{}, map[string]bool{}
		for _, n := range names {
			for _, k := range learned[n] {
				lset[k] = true
			}
			for _, k := range echoed[n] {
				eset[k] = true
			}
		}
		L, E := keysSorted(lset), keysSorted(eset)
		classes := map[string][]string{}
		for _, n := range names {
			in := sp.info[n]
			key, ok := in.keys.Request(in.mock, L)
			if !ok {
				key = "\x00unkeyed\x00" + n
			}
			classes[key] = append(classes[key], n)
		}
		for _, key := range keysSorted(classes) {
			class := classes[key]
			if len(class) < 2 {
				continue
			}
			hasCand := false
			answers := map[string]bool{}
			for _, n := range class {
				hasCand = hasCand || isCand(n)
				in := sp.info[n]
				a, ok := in.keys.Answer(in.mock, E)
				if !ok {
					a = "\x00unkeyed\x00" + n
				}
				answers[a] = true
			}
			if !hasCand || len(answers) < 2 {
				continue
			}
			sort.Strings(class)
			for _, n := range class {
				classNote[n] = class
				if isCand(n) {
					s.ambiguous[n] = true
				}
			}
		}
	}
	// An ambiguous class charges every test that consumed one of its
	// recordings, and every candidate test (or owner) of those none
	// consumed.
	for _, n := range keysSorted(classNote) {
		class := classNote[n]
		why := fmt.Sprintf("its call was recorded while other tests' requests were open: %d recordings of this call that keploy cannot tell apart got different answers (%s)", len(class), strings.Join(class, ", "))
		byWho := consumers[n]
		if t, ok := consumer[n]; ok {
			byWho = []string{t}
		}
		if len(byWho) > 0 {
			for _, t := range byWho {
				note := s.note(t, n)
				note.Class, note.Case = class, models.ConcurrentCaseClass
				s.charge(t, s.call(n, models.MissConcurrentAmbiguous, note, why))
			}
			continue
		}
		var tests []string
		if isCand(n) {
			tests = sp.candOf[n]
		} else if o := s.ownerOfDefinite(n); o != "" {
			tests = []string{o}
		}
		for _, t := range tests {
			if s.ran[t] {
				note := s.note(t, n)
				note.Class, note.Case = class, models.ConcurrentCaseClass
				s.charge(t, s.call(n, models.MissConcurrentAmbiguous, note, why))
			}
		}
	}

	// A conflict (the learn step paired a call with a candidate another
	// test answered with) charges that test too.
	conflictTo := map[string]string{}
	for _, r := range results {
		for _, c := range r.FailureInfo.UnmatchedCalls {
			if c.Reason != models.MissConcurrentConflict || c.Concurrent == nil || c.Concurrent.Claimant == "" {
				continue
			}
			x := c.Concurrent.Claimant
			why := "its call and " + r.TestCaseID + "'s both pair with " + c.ClosestMock + ", recorded once while both were open: one of them made a call it did not record"
			note := s.note(x, c.ClosestMock)
			note.Claimant, note.Case = r.TestCaseID, models.ConcurrentCaseConflict
			s.charge(x, s.call(c.ClosestMock, models.MissConcurrentConflict, note, why))
			if c.ClosestMock != "" && isCand(c.ClosestMock) {
				conflictTo[c.ClosestMock] = x
			}
		}
	}

	// Retries: a candidate answered for different tests, or not on its
	// test's last run, charges every candidate test of it that ran.
	for _, n := range cands {
		if !retried[n] || s.ambiguous[n] {
			continue
		}
		for _, t := range sp.candOf[n] {
			if s.ran[t] {
				note := s.note(t, n)
				note.Case = models.ConcurrentCaseRetry
				s.charge(t, s.call(n, models.MissConcurrentAmbiguous, note, "its attempts were answered by different recordings"))
			}
		}
	}

	// Corroboration: a learned candidate pair pins only when every other
	// learned candidate pair of its statement in its component drifted at
	// the places it did at least, or a pair of the test's own of the
	// statement drifted there.
	// A pair of a test's own (definite) recording: its attribution is not
	// in doubt. A worker-scope recording any test may answer with is none.
	definite := map[string]bool{}
	for _, names := range sp.definite {
		for _, n := range names {
			definite[n] = true
		}
	}
	definiteDrift := map[string][][]string{}
	for name, l := range learned {
		if definite[name] && !isCand(name) {
			in := sp.info[name]
			definiteDrift[in.statement] = append(definiteDrift[in.statement], l)
		}
	}
	uncorroborated := map[string]bool{}
	for _, n := range cands {
		t, ok := consumer[n]
		d := learned[n]
		if !ok || len(d) == 0 || s.ambiguous[n] || conflictTo[n] != "" {
			continue
		}
		in := sp.info[n]
		others, superset := 0, true
		for _, m := range members[componentOf[n]] {
			if m == n || !isCand(m) || len(learned[m]) == 0 || sp.info[m].statement != in.statement {
				continue
			}
			if _, consumed := consumer[m]; !consumed {
				continue
			}
			others++
			superset = superset && subset(d, learned[m])
		}
		ok = others >= 1 && superset
		for _, dd := range definiteDrift[in.statement] {
			ok = ok || subset(d, dd)
		}
		if !ok {
			uncorroborated[n] = true
			why := "its call drifted from its recording at " + strings.Join(d, ", ") + ", where no other call of this statement did"
			note := s.note(t, n)
			note.Case, note.Places = models.ConcurrentCaseCorroboration, append([]string(nil), d...)
			s.charge(t, s.call(n, models.MissConcurrentAmbiguous, note, why))
		}
	}

	// Leftovers: a candidate no test consumed, outside an ambiguous class.
	// With every test of its group kept, one of them did not make a call it
	// recorded: each that ran is charged. With one the recording did not
	// keep, that one may have made it: none is, and it is shared. The test
	// whose miss the learn step found closest to it is named.
	closestOf := map[string]string{}
	for _, r := range results {
		for _, c := range r.FailureInfo.UnmatchedCalls {
			if c.ClosestMock != "" && c.CountsAgainstTest() && closestOf[c.ClosestMock] == "" {
				closestOf[c.ClosestMock] = r.TestCaseID
			}
		}
	}
	for _, n := range cands {
		if _, consumed := consumer[n]; consumed || retried[n] || s.ambiguous[n] {
			continue
		}
		g, _ := sp.conc.group(n)
		if g.Unkept > 0 {
			continue
		}
		in := sp.info[n]
		for _, t := range g.Tests {
			if !s.ran[t] {
				continue
			}
			var with []string
			for _, o := range g.Tests {
				if o != t {
					with = append(with, o)
				}
			}
			closest := ""
			if c := closestOf[n]; c != "" && slices.Contains(g.Tests, c) {
				closest = c
			}
			s.chargeUnconsumed(t, models.UnconsumedMock{Name: n, Kind: string(in.mock.Kind), Concurrent: strings.Join(with, ", "), Closest: closest})
		}
	}

	// Where each candidate goes. Pinned: its consumer. Every other case to a
	// charged test, or to Mapping.Shared.
	for _, n := range cands {
		t, consumed := consumer[n]
		switch {
		case conflictTo[n] != "":
			s.move(n, conflictTo[n], "")
		case s.ambiguous[n] && consumed:
			s.move(n, t, models.MockEntryUnresolvedAmbiguous)
		case s.ambiguous[n]:
			if o := s.owner(n); o != "" {
				s.move(n, o, models.MockEntryUnresolvedAmbiguous)
			}
		case retried[n]:
			if o := s.owner(n); o != "" {
				s.move(n, o, "")
			}
		case uncorroborated[n]:
			s.move(n, t, "")
		case consumed:
			if s.shares.Pins == nil {
				s.shares.Pins = map[string]string{}
			}
			s.shares.Pins[n] = t
			s.resolved[n] = true
		default:
			g, _ := sp.conc.group(n)
			if g.Unkept > 0 {
				if sp.conc.entries[n].listedBy != "" {
					s.shares.ToShared = append(s.shares.ToShared, n)
				}
				s.resolved[n] = true
				continue
			}
			if o := s.owner(n); o != "" {
				s.move(n, o, "")
			}
		}
	}
	// Every candidate ends pinned, shared, or with a charged test: no test
	// the settlement did not charge keeps a call it did not make.
	for _, n := range cands {
		if s.resolved[n] {
			if mv, ok := s.shares.Moves[n]; ok && s.charges[mv.To] == nil {
				sp.settled = models.SetShares{State: models.SharesIncomplete, Why: n + " would stay with " + mv.To + ", which made no call of it and was not charged"}
				return nil
			}
			continue
		}
		sp.settled = models.SetShares{State: models.SharesIncomplete, Why: "no test of " + n + "'s group that ran could be charged with it"}
		return nil
	}
	sort.Strings(s.shares.ToShared)
	sp.settled = s.shares
	return s.apply(results)
}

// ownerOfDefinite is the test whose own recording name is, "" when none is.
func (s *settlement) ownerOfDefinite(name string) string {
	for _, t := range keysSorted(s.sp.definite) {
		for _, n := range s.sp.definite[t] {
			if n == name {
				return t
			}
		}
	}
	return ""
}

// apply writes the charges onto results: each charged test's last row gets
// them, and every row of it that is not IGNORED is FAILED, with the charge's
// category. It returns the tests it charged, sorted.
func (s *settlement) apply(results []models.TestResult) []string {
	lastRow := map[string]int{}
	for i, r := range results {
		lastRow[r.TestCaseID] = i
	}
	var charged []string
	for _, t := range keysSorted(s.charges) {
		i, ok := lastRow[t]
		if !ok {
			continue
		}
		c := s.charges[t]
		r := &results[i]
		r.FailureInfo.UnmatchedCalls = append(r.FailureInfo.UnmatchedCalls, c.calls...)
		r.FailureInfo.UnconsumedMocks = append(r.FailureInfo.UnconsumedMocks, c.unconsumed...)
		for j := range results {
			if results[j].TestCaseID != t || results[j].Status == models.TestStatusIgnored {
				continue
			}
			results[j].Status = models.TestStatusFailed
		}
		if len(c.calls) > 0 {
			missCategory(r)
		}
		if len(c.unconsumed) > 0 {
			r.FailureInfo.Category = appendCategoryUnique(r.FailureInfo.Category, models.DependencyMissing)
		}
		charged = append(charged, t)
	}
	return charged
}

// setShares is what the set's end settled, for the set's end hook: a set that
// ended other than PASSED or FAILED, whose shares were complete, has them
// incomplete, as its verdicts are not judged.
func (p *exactPlan) setShares(status models.TestSetStatus) (models.SetShares, bool) {
	if p == nil || p.share == nil || !p.share.sharing {
		return models.SetShares{}, false
	}
	s := p.share.settled
	if s.State == models.SharesComplete && status != models.TestSetStatusPassed && status != models.TestSetStatusFailed {
		return models.SetShares{State: models.SharesIncomplete, Why: "the test set ended " + string(status)}, true
	}
	return s, true
}

// stripUnresolved is consumed less what the learn step learned on the shared
// recordings the settlement did not pin (moved, or left shared): their
// pairing is not proven, so nothing of it is written onto the recording. The
// states of the others are kept as they are.
func (p *exactPlan) stripUnresolved(consumed map[string]models.MockState) map[string]models.MockState {
	if p == nil || p.share == nil || !p.share.sharing {
		return consumed
	}
	sp := p.share
	out := make(map[string]models.MockState, len(consumed))
	for name, st := range consumed {
		if _, cand := sp.candOf[name]; cand && sp.settled.Pins[name] == "" {
			st.ReqBodyNoise, st.Variables, st.Binds = nil, nil, nil
		}
		out[name] = st
	}
	return out
}

// consumerOf is, of the shared recordings, the test each was pinned to: the
// test that made its call.
func (p *exactPlan) consumerOf() map[string]string {
	if p == nil || p.share == nil {
		return nil
	}
	return p.share.settled.Pins
}

// annotateConcurrent says, under the proven switch, on each miss and each
// unconsumed recording of a test recorded while other tests' requests were
// open, what keploy knows of that: the tests open beside it, the test its
// recordings were attributed to, and why they were not shared (their kind,
// what their recording states, or that only keploy record's full round shares
// them), instead of a bare "had no recorded answer". A test that lists a
// recording a full round could not give to one test
// (models.MockEntryUnresolvedAmbiguous) is charged concurrent_ambiguous in
// every replay that does not share, so no replay can admit it. It returns
// the tests it charged so.
func (p *exactPlan) annotateConcurrent(results []models.TestResult, lookup map[string]mockDisplayInfo) []string {
	if !provenswitch.On() || p == nil || p.share == nil {
		return nil
	}
	sp := p.share
	var charged []string
	for i := range results {
		r := &results[i]
		test := r.TestCaseID
		with, unkept := sp.conc.with(test)
		for j := range r.FailureInfo.UnmatchedCalls {
			c := &r.FailureInfo.UnmatchedCalls[j]
			if c.Concurrent != nil || !c.CountsAgainstTest() || len(with) == 0 {
				continue
			}
			n := &models.ConcurrentNote{With: with, Unkept: unkept}
			n.AttributedTo, n.WhyNot, n.Kind, n.Stated = sp.attribution(test, c.ClosestMock)
			c.Concurrent = n
		}
		for j := range r.FailureInfo.UnconsumedMocks {
			u := &r.FailureInfo.UnconsumedMocks[j]
			if u.Concurrent != "" {
				continue
			}
			if g, ok := sp.conc.group(u.Name); ok {
				var with []string
				for _, t := range g.Tests {
					if t != test {
						with = append(with, t)
					}
				}
				u.Concurrent = strings.Join(with, ", ")
			}
		}
		if sp.sharing || r.Status == models.TestStatusIgnored {
			continue
		}
		for _, name := range keysSorted(sp.conc.entries) {
			e := sp.conc.entries[name]
			if e.listedBy != test || e.entry.Unresolved != models.MockEntryUnresolvedAmbiguous {
				continue
			}
			s := &settlement{sp: sp, lookup: lookup}
			note := s.note(test, name)
			note.Case = models.ConcurrentCaseUnresolved
			r.FailureInfo.UnmatchedCalls = append(r.FailureInfo.UnmatchedCalls, s.call(name, models.MissConcurrentAmbiguous, note,
				"keploy record's full round could not tell which of the tests open while it was recorded made this call"))
			r.Status = models.TestStatusFailed
			missCategory(r)
			charged = appendOnce(charged, test)
		}
	}
	return charged
}

// attribution is, for a miss of test whose closest recording is closest ("" for
// none), the test the call's recordings were attributed to, and why they were
// not shared with test (models.ConcurrentNotShared*), with the kind keploy
// shares none of, or what the recording states that keploy does not share.
func (sp *sharePlan) attribution(test, closest string) (attributedTo, whyNot, kind, stated string) {
	reason := func(name string) (string, string, string) {
		in := sp.info[name]
		switch {
		case !sp.sharing:
			return models.ConcurrentNotSharedStrict, "", ""
		case in == nil:
			return models.ConcurrentNotSharedKind, kindOf(sp, name), ""
		case !in.shareable:
			return models.ConcurrentNotSharedStates, "", in.whyNot
		}
		return "", "", ""
	}
	if e, ok := sp.conc.entries[closest]; ok && closest != "" {
		if e.listedBy == "" {
			return "", models.ConcurrentNotSharedUnkept, "", ""
		}
		w, k, st := reason(closest)
		if e.listedBy != test {
			return e.listedBy, w, k, st
		}
		return "", w, k, st
	}
	// No closest recording: the first grouped recording of another test of
	// test's groups names whom, and why, its calls were attributed.
	groups := map[string]bool{}
	for _, id := range sp.conc.ofTest[test] {
		groups[id] = true
	}
	for _, name := range keysSorted(sp.conc.entries) {
		e := sp.conc.entries[name]
		if e.listedBy == "" || e.listedBy == test || !groups[e.entry.Group] {
			continue
		}
		if w, k, st := reason(name); w != "" {
			return e.listedBy, w, k, st
		}
	}
	return "", "", "", ""
}

// kindOf is the kind of the grouped entry name.
func kindOf(sp *sharePlan, name string) string {
	return sp.conc.entries[name].entry.Kind
}

// unionFind is a union-find over mock names.
type unionFind struct {
	parent map[string]string
	order  []string
}

func newUnionFind() *unionFind { return &unionFind{parent: map[string]string{}} }

func (u *unionFind) add(x string) {
	if _, ok := u.parent[x]; !ok {
		u.parent[x] = x
		u.order = append(u.order, x)
	}
}

func (u *unionFind) find(x string) string {
	for u.parent[x] != x {
		u.parent[x] = u.parent[u.parent[x]]
		x = u.parent[x]
	}
	return x
}

func (u *unionFind) union(a, b string) {
	u.add(a)
	u.add(b)
	ra, rb := u.find(a), u.find(b)
	if ra != rb {
		if ra < rb {
			u.parent[rb] = ra
		} else {
			u.parent[ra] = rb
		}
	}
}

// components are the sets, each sorted, in order of their least name.
func (u *unionFind) components() [][]string {
	by := map[string][]string{}
	for _, x := range u.order {
		r := u.find(x)
		by[r] = append(by[r], x)
	}
	out := make([][]string, 0, len(by))
	for _, r := range keysSorted(by) {
		names := by[r]
		sort.Strings(names)
		out = append(out, names)
	}
	return out
}

// keysSorted is m's keys, sorted.
func keysSorted[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// logShares says what the settlement decided: the calls it pinned, and the
// tests it charged.
func (r *Replayer) logShares(testSetID string, charged []string) {
	if r.exact == nil || r.exact.share == nil || !r.exact.share.sharing {
		return
	}
	s := r.exact.share.settled
	switch s.State {
	case models.SharesIncomplete:
		r.logger.Warn("keploy could not settle which test made each call recorded while several tests' requests were open: the test set's shares are incomplete, so keploy record does not judge it",
			zap.String("testSetID", testSetID), zap.String("reason", s.Why))
	case models.SharesComplete:
		r.logger.Info("settled which test made each call recorded while several tests' requests were open",
			zap.String("testSetID", testSetID), zap.Int("pinned", len(s.Pins)), zap.Int("moved", len(s.Moves)), zap.Int("shared", len(s.ToShared)))
	}
	if len(charged) > 0 {
		r.logger.Error("tests failed: keploy cannot tell which of the tests recorded together made some of their calls ("+provenswitch.Env+"=1); the report says why under unmatched_calls and unconsumed_mocks",
			zap.String("testSetID", testSetID), zap.Strings("tests", charged))
	}
}
