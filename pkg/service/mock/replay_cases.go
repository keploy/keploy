package mock

import (
	"context"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	httpMatcher "go.keploy.io/server/v3/pkg/matcher/http"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// CaseOutcome is one recorded request replayed: what the app answered this run, compared with the recording.
type CaseOutcome struct {
	Flow   string
	Case   *models.TestCase
	Actual *models.HTTPResp // nil when the runner did not make this request
	Passed bool
	Result *models.Result
}

// FlowMocks is what one test used and missed this run, next to what its recording lists.
type FlowMocks struct {
	Flow     string
	Expected []models.MockEntry
	Consumed []models.MockState
	Missed   []models.UnmatchedCall
}

// CaseReader is an optional MappingDB extension: which cases each flow recorded.
type CaseReader interface {
	GetCases(ctx context.Context, testSetID string) (map[string][]string, error)
}

// replayDetail is everything the outcome needs to say what each test did this run.
type replayDetail struct {
	windows  []models.ScopeWindow
	expected map[string][]models.MockEntry
	recorded map[string][]*models.TestCase
	actual   []*models.TestCase
}

// actualCapture keeps the app's incoming requests during a replay, in memory only.
type actualCapture struct {
	done  chan struct{}
	mu    sync.Mutex
	cases []*models.TestCase
}

// watchIncoming starts keeping the app's incoming requests when requests are on; nil when they are off.
func (m *mockService) watchIncoming(ctx context.Context) *actualCapture {
	if !m.config.Mock.RecordRequests {
		return nil
	}
	reader, ok := m.instrumentation.(IncomingReader)
	if !ok {
		return nil
	}
	incoming, err := reader.GetIncoming(ctx, models.IncomingOptions{Filters: m.config.Record.Filters})
	if err != nil {
		m.logger.Warn("the app's responses will not be compared with the recording: could not read its incoming requests", zap.Error(err))
		return nil
	}
	c := &actualCapture{done: make(chan struct{})}
	go func() {
		defer utils.Recover(m.logger)
		defer close(c.done)
		for tc := range incoming {
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

func flowAt(windows []models.ScopeWindow, at time.Time) (string, bool) {
	name := containing(windows, at)
	return name, name != ""
}

// pairCases matches the requests the runner made this run with the cases each flow recorded, in order within the flow.
func pairCases(windows []models.ScopeWindow, recorded map[string][]*models.TestCase, actual []*models.TestCase, compare func(*models.TestCase, *models.HTTPResp) (bool, *models.Result)) []CaseOutcome {
	byFlow := make(map[string][]*models.TestCase)
	for _, a := range actual {
		if flow, ok := flowAt(windows, caseTime(a)); ok {
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
		for _, rc := range recorded[flow] {
			key := requestKey(string(rc.HTTPReq.Method), rc.HTTPReq.URL)
			o := CaseOutcome{Flow: flow, Case: rc}
			for i, a := range seen {
				if used[i] || requestKey(string(a.HTTPReq.Method), a.HTTPReq.URL) != key {
					continue
				}
				used[i] = true
				resp := a.HTTPResp
				o.Actual = &resp
				o.Passed, o.Result = compare(rc, &resp)
				break
			}
			out = append(out, o)
		}
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
		if flow, ok := flowAt(windows, time.Unix(0, c.Timestamp)); ok {
			get(flow).Consumed = append(get(flow).Consumed, c)
		}
	}
	for _, miss := range misses {
		if miss.At.IsZero() {
			continue
		}
		if flow, ok := flowAt(windows, miss.At); ok {
			get(flow).Missed = append(get(flow).Missed, miss)
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
