package mock

import (
	"bytes"
	"context"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	httpMatcher "go.keploy.io/server/v3/pkg/matcher/http"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// CaseOutcome is one recorded request replayed: what the app answered this run, compared with the recording.
type CaseOutcome struct {
	Flow    string
	Case    *models.TestCase
	Actual  *models.HTTPResp // nil when the runner did not make this request
	Passed  bool
	Result  *models.Result
	Skipped bool
}

// FlowMocks is what one test used and missed this run, next to what its recording lists.
type FlowMocks struct {
	Flow     string
	Expected []models.MockEntry
	Consumed []models.MockState
	Missed   []models.UnmatchedCall
	// Outcome is the verdict of the test's latest run this replay
	// (models.ScopeOutcome*, ScopeOutcomeGated when the replay told it not to
	// run); "" when its harness reported none.
	Outcome string
}

// CaseReader is an optional MappingDB extension: which cases each flow recorded.
type CaseReader interface {
	GetCases(ctx context.Context, testSetID string) (map[string][]string, error)
}

// replayDetail is everything the outcome needs to say what each test did this run.
type replayDetail struct {
	windows  []models.ScopeWindow
	starts   []models.ScopeWindow
	expected map[string][]models.MockEntry
	recorded map[string][]*models.TestCase
	actual   []*models.TestCase
	failed   []string
}

// actualCapture keeps the app's incoming requests during a replay, in memory only.
type actualCapture struct {
	done  chan struct{}
	seen  atomic.Int64
	mu    sync.Mutex
	cases []*models.TestCase
}

// watchIncoming starts keeping the app's incoming requests when requests are on; nil when they are off.
func (m *mockService) watchIncoming(ctx context.Context, on bool) *actualCapture {
	if !on {
		return nil
	}
	incoming, err := m.instrumentation.(IncomingReader).GetIncoming(ctx, models.IncomingOptions{Filters: m.config.Record.Filters})
	if err != nil {
		m.logger.Warn("the app's responses will not be compared with the recording: could not read its incoming requests", zap.Error(err))
		return nil
	}
	c := &actualCapture{done: make(chan struct{})}
	go func() {
		defer utils.Recover(m.logger)
		defer close(c.done)
		for tc := range incoming {
			c.seen.Add(1)
			c.mu.Lock()
			c.cases = append(c.cases, tc)
			c.mu.Unlock()
		}
	}()
	return c
}

// wait blocks until the incoming stream has been drained, or grace has passed.
func (c *actualCapture) wait(grace time.Duration) {
	if c == nil {
		return
	}
	select {
	case <-c.done:
	case <-time.After(grace):
	}
}

func (c *actualCapture) list() []*models.TestCase {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]*models.TestCase(nil), c.cases...)
}

var idSegment = regexp.MustCompile(`^([0-9]+|[0-9a-fA-F]{12,}|[0-9a-fA-F]{8}-[0-9a-fA-F-]{27})$`)

// normalisePath keeps the path of a URL with ids blanked, so the same call made twice looks the same.
func normalisePath(rawURL string) string {
	path := rawURL
	if u, err := url.Parse(rawURL); err == nil && u.Path != "" {
		path = u.Path
	}
	parts := strings.Split(path, "/")
	for i, p := range parts {
		if idSegment.MatchString(p) {
			parts[i] = "{id}"
		}
	}
	return strings.Join(parts, "/")
}

// requestKey is what makes two requests the same call: the method and the path with ids blanked.
func requestKey(method, rawURL string) string {
	return strings.ToUpper(method) + " " + normalisePath(rawURL)
}

func exactKey(method, rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return strings.ToUpper(method) + " " + rawURL
	}
	return strings.ToUpper(method) + " " + u.Path + "?" + u.Query().Encode()
}

func flowAt[V any](windows []models.ScopeWindow, at time.Time, known map[string]V) (string, bool) {
	best, top := -1, -1
	for i, w := range windows {
		if at.Before(w.Start) || at.After(w.End) {
			continue
		}
		if _, ok := known[w.Name]; ok && (best == -1 || w.Start.After(windows[best].Start)) {
			best = i
		}
		if top == -1 || len(w.Name) < len(windows[top].Name) {
			top = i
		}
	}
	if best == -1 {
		best = top
	}
	if best == -1 {
		return "", false
	}
	return windows[best].Name, true
}

// pairCases matches the requests the runner made this run with the cases each flow recorded, in order within the flow.
func pairCases(windows []models.ScopeWindow, recorded map[string][]*models.TestCase, actual []*models.TestCase, compare func(*models.TestCase, *models.HTTPResp) (bool, *models.Result)) []CaseOutcome {
	if len(windows) == 0 {
		return nil
	}
	byFlow := make(map[string][]*models.TestCase)
	for _, a := range actual {
		if a.Kind == models.GRPC_EXPORT {
			continue
		}
		if flow, ok := flowAt(windows, caseTime(a), recorded); ok {
			byFlow[flow] = append(byFlow[flow], a)
		}
	}
	flows := make([]string, 0, len(recorded))
	for flow := range recorded {
		flows = append(flows, flow)
	}
	sort.Strings(flows)
	var out []CaseOutcome
	for _, flow := range flows {
		seen := byFlow[flow]
		sort.SliceStable(seen, func(i, j int) bool { return caseTime(seen[i]).Before(caseTime(seen[j])) })
		used := make([]bool, len(seen))
		cases := recorded[flow]
		outs := make([]CaseOutcome, len(cases))
		for i, rc := range cases {
			outs[i] = CaseOutcome{Flow: flow, Case: rc, Skipped: rc.Kind == models.GRPC_EXPORT}
		}
		for _, key := range []func(method, rawURL string) string{exactKey, requestKey} {
			for i, rc := range cases {
				if outs[i].Skipped || outs[i].Actual != nil {
					continue
				}
				want := key(string(rc.HTTPReq.Method), rc.HTTPReq.URL)
				for j, a := range seen {
					if used[j] || key(string(a.HTTPReq.Method), a.HTTPReq.URL) != want {
						continue
					}
					used[j] = true
					resp := a.HTTPResp
					outs[i].Actual = &resp
					outs[i].Passed, outs[i].Result = compare(rc, &resp)
					break
				}
			}
		}
		out = append(out, outs...)
	}
	return out
}

// attributeMocks puts each served mock and each miss under the test that was running when it happened.
func attributeMocks(windows []models.ScopeWindow, expected map[string][]models.MockEntry, consumed []models.MockState, misses []models.UnmatchedCall) []FlowMocks {
	byFlow := make(map[string]*FlowMocks)
	get := func(flow string) *FlowMocks {
		if f, ok := byFlow[flow]; ok {
			return f
		}
		f := &FlowMocks{Flow: flow}
		byFlow[flow] = f
		return f
	}
	for flow, entries := range expected {
		get(flow).Expected = entries
	}
	for _, c := range consumed {
		if c.Timestamp == 0 {
			continue // an older agent does not say when it served a mock
		}
		if flow, ok := flowAt(windows, time.Unix(0, c.Timestamp), expected); ok {
			get(flow).Consumed = append(get(flow).Consumed, c)
		}
	}
	for _, miss := range misses {
		if miss.At.IsZero() {
			continue
		}
		if flow, ok := flowAt(windows, miss.At, expected); ok {
			get(flow).Missed = append(get(flow).Missed, miss)
		}
	}
	// The verdict of a test's latest run — "" when that run reported none, not
	// an earlier run's. A test that recorded and used no mocks gets a line
	// only for a verdict (a test the replay gated out has one).
	latest := make(map[string]models.ScopeWindow)
	for _, w := range windows {
		if w.Name == "" {
			continue
		}
		if prev, seen := latest[w.Name]; seen && w.End.Before(prev.End) {
			continue
		}
		latest[w.Name] = w
	}
	for name, w := range latest {
		if f, known := byFlow[name]; known {
			f.Outcome = w.Outcome
		} else if w.Outcome != "" {
			get(name).Outcome = w.Outcome
		}
	}
	if len(byFlow) == 0 {
		return nil
	}
	out := make([]FlowMocks, 0, len(byFlow))
	for _, f := range byFlow {
		out = append(out, *f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Flow < out[j].Flow })
	return out
}

// compareCase checks the app's answer against the recorded one, forgiving the case's own noise and volatile headers.
func (m *mockService) compareCase(tc *models.TestCase, actual *models.HTTPResp) (bool, *models.Result) {
	tc = m.withRunIDs(tc)
	return httpMatcher.Match(tc, actual, m.config.Test.GlobalNoise.Global, m.config.Test.IgnoreOrdering, m.config.Test.CompareAll, m.logger, false, httpMatcher.WithAutoHeaderNoise(true))
}

// recordedCases loads the cases each flow recorded, keyed by flow, in recorded order.
func (m *mockService) recordedCases(ctx context.Context, name string) map[string][]*models.TestCase {
	reader, ok := m.mappingDB.(CaseReader)
	if !ok || m.testDB == nil {
		return nil
	}
	byFlow, err := reader.GetCases(ctx, name)
	if err != nil {
		m.logger.Debug("could not read which cases each flow recorded", zap.Error(err))
		return nil
	}
	tcs, err := m.testDB.GetTestCases(ctx, name)
	if err != nil {
		m.logger.Debug("could not read the recorded cases", zap.Error(err))
		return nil
	}
	byName := make(map[string]*models.TestCase, len(tcs))
	for _, tc := range tcs {
		byName[tc.Name] = tc
	}
	out := make(map[string][]*models.TestCase, len(byFlow))
	for flow, names := range byFlow {
		for _, n := range names {
			if tc, ok := byName[n]; ok {
				out[flow] = append(out[flow], tc)
			}
		}
	}
	return out
}

// expectedMocks reads which mocks each flow recorded.
func (m *mockService) expectedMocks(ctx context.Context, name string) map[string][]models.MockEntry {
	if m.mappingDB == nil {
		return nil
	}
	byFlow, _, err := m.mappingDB.Get(ctx, name)
	if err != nil {
		m.logger.Debug("could not read which mocks each flow recorded", zap.Error(err))
		return nil
	}
	return byFlow
}

func (m *mockService) withRunIDs(tc *models.TestCase) *models.TestCase {
	if m.ids == nil || m.ids.Empty() || tc == nil {
		return tc
	}
	out := *tc
	out.HTTPResp.Body = m.ids.Rewrite(tc.HTTPResp.Body)
	if len(tc.HTTPResp.Header) > 0 {
		out.HTTPResp.Header = make(map[string]string, len(tc.HTTPResp.Header))
		for k, v := range tc.HTTPResp.Header {
			out.HTTPResp.Header[k] = m.ids.Rewrite(v)
		}
	}
	return &out
}

// reportMisses prints each call no mock matched once, with the test it was
// made in ("(outside any test)" when none) and how many times, and what the
// matcher said about the first one: how far matching got, the closest mock,
// and the protocol's own next step. In a `keploy mock` run the agent logs
// misses at Debug, so this is where they show.
func (m *mockService) reportMisses(misses []models.UnmatchedCall, byFlow []FlowMocks) {
	m.reportMissesAs(misses, byFlow, "(outside any test)")
}

// reportMissesAs is reportMisses with the label for a miss no test's window
// holds.
func (m *mockService) reportMissesAs(misses []models.UnmatchedCall, byFlow []FlowMocks, unplaced string) {
	type key struct{ test, protocol, call, dest string }
	var order []key
	n := map[key]int{}
	first := map[key]models.UnmatchedCall{}
	add := func(test string, miss models.UnmatchedCall) {
		k := key{test, miss.Protocol, miss.ActualSummary, miss.Destination}
		if n[k] == 0 {
			order = append(order, k)
			first[k] = miss
		}
		n[k]++
	}
	type call struct{ protocol, summary, dest string }
	placed := map[call]int{}
	for _, f := range byFlow {
		for _, miss := range f.Missed {
			add(f.Flow, miss)
			placed[call{miss.Protocol, miss.ActualSummary, miss.Destination}]++
		}
	}
	for _, miss := range misses {
		k := call{miss.Protocol, miss.ActualSummary, miss.Destination}
		if placed[k] > 0 {
			placed[k]--
			continue
		}
		add("", miss)
	}
	for _, k := range order {
		test := k.test
		if test == "" {
			test = unplaced
		}
		fields := []zap.Field{zap.String("test", test), zap.String("protocol", k.protocol), zap.String("call", k.call), zap.String("destination", k.dest)}
		if n[k] > 1 {
			fields = append(fields, zap.Int("times", n[k]))
		}
		miss := first[k]
		if miss.MatchPhase != "" {
			fields = append(fields, zap.String("match_phase", miss.MatchPhase))
		}
		if miss.CandidateCount > 0 {
			fields = append(fields, zap.Int("candidates", miss.CandidateCount))
		}
		if miss.ClosestMock != "" {
			fields = append(fields, zap.String("closest", miss.ClosestMock))
		}
		if miss.DestinationScope != "" && miss.DestinationScope != models.DestinationScopeUnknown {
			fields = append(fields, zap.String("destination_scope", miss.DestinationScope))
		}
		next := miss.NextSteps
		if next == "" {
			next = "record this call with --on-miss record, or re-record the set"
		}
		m.logger.Warn("no recorded mock matched a call", append(fields, zap.String("next_step", next))...)
	}
}

// ranTests lists the top-level tests the replay reached, once each, in the
// order they first appeared. A test run more than once (-count, a retry) is
// listed once, with its latest run's verdict. A test the replay gated out
// (--run-only) is listed with the verdict models.ScopeOutcomeGated: it did not
// run, and a consumer must tell it apart from a test that did. Tests of the
// same name in different folders stay apart.
func ranTests(windows []models.ScopeWindow) []RanTest {
	root := gitTop()
	type key struct{ dir, name string }
	var out []RanTest
	at := map[key]int{}
	ends := map[key]time.Time{}
	for _, w := range windows {
		if w.App || w.Suite || w.Name == "" || parentOf(windows, w.Name) != "" {
			continue
		}
		k := key{w.Dir, w.Name}
		t := RanTest{Name: w.Name, Set: setName(w.Dir, root), Status: w.Outcome}
		i, seen := at[k]
		if !seen {
			at[k], ends[k] = len(out), w.End
			out = append(out, t)
			continue
		}
		if !w.End.Before(ends[k]) {
			out[i], ends[k] = t, w.End
		}
	}
	return out
}

type failScan struct {
	mu     sync.Mutex
	rest   []byte
	failed []string
	// overlong is set while the line being read passed maxFailScanLine.
	overlong bool
}

// maxFailScanLine bounds the partial line failScan keeps between writes.
const maxFailScanLine = 64 << 10

func (f *failScan) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rest = append(f.rest, p...)
	defer func() {
		// A `--- FAIL:` line is short; a runner writing a long line with no
		// newline must not grow this buffer without bound. Drop the partial
		// line and the rest of it, so no text inside it is read as a line.
		if len(f.rest) > maxFailScanLine {
			f.rest, f.overlong = nil, true
		}
	}()
	for {
		i := bytes.IndexByte(f.rest, '\n')
		if i < 0 {
			break
		}
		line := string(f.rest[:i])
		f.rest = f.rest[i+1:]
		if f.overlong {
			f.overlong = false
			continue
		}
		if !strings.HasPrefix(line, "--- FAIL: ") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 3 || strings.Contains(fields[2], "/") || slices.Contains(f.failed, fields[2]) {
			continue
		}
		f.failed = append(f.failed, fields[2])
	}
	return len(p), nil
}

func (f *failScan) list() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.failed...)
}
