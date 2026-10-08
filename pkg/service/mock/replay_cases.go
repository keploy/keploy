package mock

import (
	"context"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mocknoise"

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
func pairCases(windows []models.ScopeWindow, recorded map[string][]*models.TestCase, actual []*models.TestCase, compare func(recorded *models.TestCase, sent *models.HTTPReq, answer *models.HTTPResp) (bool, *models.Result)) []CaseOutcome {
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
					outs[i].Passed, outs[i].Result = compare(rc, &a.HTTPReq, &resp)
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
//
// The answer is compared with the recording as it is first, so whatever
// passes without ids being followed passes with it. Only when that fails, and
// the agent bound ids this run, is it compared once more with those ids put
// back (asRecorded).
func (m *mockService) compareCase(tc *models.TestCase, sent *models.HTTPReq, answer *models.HTTPResp) (bool, *models.Result) {
	pass, result := m.matchCase(tc, answer)
	if pass {
		return pass, result
	}
	if recorded, changed := m.asRecorded(tc, sent, answer); changed {
		return m.matchCase(tc, &recorded)
	}
	return pass, result
}

func (m *mockService) matchCase(tc *models.TestCase, answer *models.HTTPResp) (bool, *models.Result) {
	return httpMatcher.Match(tc, answer, m.config.Test.GlobalNoise.Global, m.config.Test.IgnoreOrdering, m.config.Test.CompareAll, m.logger, false, httpMatcher.WithAutoHeaderNoise(true))
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

// asRecorded is the app's answer to a case with the ids this run made mapped
// back to the recorded ones they stand for, in its body and header values,
// and whether that changed anything. The pairs are the agent's (the ids it
// bound where the app first sent them to a dependency); ids are replaced as
// whole words, as the agent replaces them in the mocks' answers.
//
// The case's own request has the say over each pair. Where the recorded
// request names the recorded id, the test chose which entity it asked for, and
// the pair is used only if the request it sent this run names the live id: a
// test that sent Y where the recording has X, while the app made Z, asked for
// one entity and was answered with another, and must fail. A recorded id the
// recorded request does not name (a create's answer) was the app's to make,
// and its live id is always mapped back.
//
// An answer that names the recorded id of a pair beside its live one names one
// entity by two ids — a dependency call the agent answered as recorded handed
// the app the recorded id, and the app's own id stands next to it — and that
// pair is not used: mapped back, the two would read as one and pass.
//
// Nothing is taken from the request alone: without a pair from the agent the
// answer stands as it is.
func (m *mockService) asRecorded(tc *models.TestCase, sent *models.HTTPReq, answer *models.HTTPResp) (models.HTTPResp, bool) {
	if m.ids == nil || m.ids.Empty() {
		return *answer, false
	}
	recorded, live := requestWords(&tc.HTTPReq), requestWords(sent)
	answered := responseWords(answer)
	back := map[string]string{} // this run's id -> the recorded one
	for rec, cur := range m.ids.Pairs() {
		if answered[rec] {
			continue
		}
		if !recorded[rec] || live[cur] {
			back[cur] = rec
		}
	}
	return mocknoise.ReplaceResponseWords(*answer, func(w string) (string, bool) {
		rec, ok := back[w]
		return rec, ok
	})
}

// responseWords are the words of a response an id can be, from its body and
// header values.
func responseWords(resp *models.HTTPResp) map[string]bool {
	out := map[string]bool{}
	mocknoise.EachResponseText(resp, func(s string) {
		mocknoise.ScanWords(s, func(start, end int) { out[s[start:end]] = true })
	})
	return out
}

// requestWords are the words of a request an id can be (mocknoise.ScanWords),
// from every text of it: URL, body, header, query and form values.
func requestWords(req *models.HTTPReq) map[string]bool {
	out := map[string]bool{}
	mocknoise.EachRequestText(req, func(s string) {
		mocknoise.ScanWords(s, func(start, end int) { out[s[start:end]] = true })
	})
	return out
}
