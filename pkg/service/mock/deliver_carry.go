package mock

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"go.keploy.io/server/v3/pkg"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// PriorDelivery is a deliver: entry of the set's previous recording
// (models.Delivery) with the calls its names stood for there: its case's
// request (CaseCall, "" when that case is not a recorded HTTP request) and
// its after: mock's (AfterCall, "" when that mock is not a recorded HTTP
// call). A re-record names its cases and mocks anew, so a DeliveryCarrier
// finds the entry's case and mock again by these. Lost says why the previous
// recording's cases or mocks could not be read ("" when they were): its
// calls are then unknown.
type PriorDelivery struct {
	models.Delivery
	CaseCall  string
	AfterCall string
	Lost      string
}

// NamedCall is a case or mock of a recording, by name, with the call it is
// (caseCall, mockCall; "" for one that is not an HTTP request or call).
type NamedCall struct {
	Name string
	Call string
}

// CarryTest is a test the new recording ran, with deliver: entries in the
// previous one: its cases and its mocks, in the recording's order.
type CarryTest struct {
	Name  string
	Cases []NamedCall
	Mocks []NamedCall
}

// CarryLine is what keploy record says of one deliver: entry of the
// previous recording: carried into the new one (Kept, said at INFO), or
// dropped (said at WARN).
type CarryLine struct {
	Kept bool
	Msg  string
}

// DeliveryCarrier carries a set's deliver: entries across a re-record. A
// downstream build registers one (RegisterDeliveryCarrier); without one a
// re-record drops every entry of the tests it ran, each said at WARN with
// how to add it back: an entry naming the old recording's case and mock
// would send another request, or wait for another call, in the new one.
type DeliveryCarrier interface {
	// Carry returns, by test, the entries each test of tests keeps in its
	// new recording, named as that recording names their case and after:
	// mock, and one line for each entry of prior that tests holds a test
	// for. prior is by test; tests holds only tests prior has entries for.
	Carry(prior map[string][]PriorDelivery, tests []CarryTest) (map[string][]models.Delivery, []CarryLine)
}

// deliveryCarrier is the registered DeliveryCarrier, nil when none.
var deliveryCarrier DeliveryCarrier

// RegisterDeliveryCarrier registers c as keploy record's DeliveryCarrier. A
// nil c unregisters it.
func RegisterDeliveryCarrier(c DeliveryCarrier) {
	deliveryCarrier = c
}

// caseCall is the recorded HTTP request a deliver: entry's case stands for,
// as a re-record finds it again: its method and path ("POST /webhooks/pay").
// "" for a case that is not an HTTP request.
func caseCall(tc *models.TestCase) string {
	if tc == nil || tc.Kind != models.HTTP {
		return ""
	}
	return string(tc.HTTPReq.Method) + " " + callPath(tc.HTTPReq.URL, false)
}

// mockCall is the HTTP call a deliver: entry's after: mock stands for, as a
// re-record finds it again: its method, host and path ("POST
// pay.example:443/v1/charges"). "" for a mock that is not an HTTP call.
func mockCall(mk *models.Mock) string {
	if mk == nil || mk.Kind != models.HTTP || mk.Spec.HTTPReq == nil {
		return ""
	}
	return string(mk.Spec.HTTPReq.Method) + " " + callPath(mk.Spec.HTTPReq.URL, true)
}

// callPath is raw's path, after its host when withHost; raw without its
// query when it is not a URL.
func callPath(raw string, withHost bool) string {
	u, err := url.Parse(raw)
	if err != nil {
		path, _, _ := strings.Cut(raw, "?")
		return path
	}
	if withHost {
		return u.Host + u.Path
	}
	return u.Path
}

// priorDeliveries reads, by test, the deliver: entries of the set's previous
// recording, with the calls their names stood for there, before a re-record
// deletes its mappings: carryDeliveries hands them to the DeliveryCarrier,
// which keeps each in the new recording when it finds its calls there. Nil without the proven switch (deliver: is
// read only under it), when the set has none, or when its mappings cannot be
// read, which is said.
func (m *mockService) priorDeliveries(ctx context.Context, name string) map[string][]PriorDelivery {
	if !provenswitch.On() {
		return nil
	}
	reader, ok := m.mappingDB.(MappingReader)
	if !ok {
		return nil
	}
	mp, err := reader.GetMapping(ctx, name)
	if err != nil {
		m.logger.Warn("keploy could not read the set's mappings.yaml before recording it again, so it carries none of its deliver: entries into this recording",
			zap.String("mock-set", name), zap.Error(err),
			zap.String("next_step", "add each test's deliver: entries back in mappings.yaml, with the names its callback case and trigger call have in this recording"))
		return nil
	}
	if mp == nil {
		return nil
	}
	byTest := make(map[string][]PriorDelivery)
	mocks := map[string]bool{}
	for _, tc := range mp.TestCases {
		for _, d := range tc.Deliver {
			byTest[tc.ID] = append(byTest[tc.ID], PriorDelivery{Delivery: d})
			mocks[d.After] = true
		}
	}
	if len(byTest) == 0 {
		return nil
	}
	caseCalls, casesLost := m.priorCaseCalls(ctx, name)
	mockCalls, mocksLost := m.priorMockCalls(ctx, name, mocks)
	for _, ds := range byTest {
		for i := range ds {
			d := &ds[i]
			d.CaseCall, d.AfterCall = caseCalls[d.Case], mockCalls[d.After]
			d.Lost = casesLost
			if d.Lost == "" {
				d.Lost = mocksLost
			}
		}
	}
	return byTest
}

// priorCaseCalls is, by name, each case of the set's previous recording's
// call (caseCall); the string says why none could be read.
func (m *mockService) priorCaseCalls(ctx context.Context, name string) (map[string]string, string) {
	if m.testDB == nil {
		return nil, "this keploy keeps no recorded cases (mock.recordRequests is false in keploy.yml)"
	}
	tcs, err := m.testDB.GetTestCases(ctx, name)
	if err != nil {
		return nil, fmt.Sprintf("the previous recording's cases could not be read: %v", err)
	}
	calls := make(map[string]string, len(tcs))
	for _, tc := range tcs {
		if tc != nil {
			calls[tc.Name] = caseCall(tc)
		}
	}
	return calls, ""
}

// priorMockCalls is, for each of the named mocks of the set's previous
// recording, its call (mockCall), read one mock at a time and kept for the
// named ones only: a set's mocks are never held whole. The string says why
// none could be read.
func (m *mockService) priorMockCalls(ctx context.Context, name string, want map[string]bool) (map[string]string, string) {
	walker, ok := m.mockDB.(pkg.TestSetMockWalker)
	if !ok {
		return nil, "this keploy's mock store cannot read the previous recording's mocks one by one"
	}
	calls := make(map[string]string, len(want))
	err := walker.WalkTestSetMocks(ctx, name, func(mk *models.Mock, _, _ bool) {
		if want[mk.Name] {
			calls[mk.Name] = mockCall(mk)
		}
	})
	if err != nil {
		return nil, fmt.Sprintf("the previous recording's mocks could not be read: %v", err)
	}
	return calls, ""
}

// carryDeliveries hands the deliver: entries of the set's previous recording
// (prior) of each test this recording ran (windows) to the DeliveryCarrier
// with the test's new cases and mocks (byCase, byTest; cases and mocks: the
// new recording's calls by name), puts the entries it keeps in byCase, and
// says each entry: carried at INFO, dropped at WARN. A test this recording
// did not run is left as it is. With no carrier every entry of a test that
// ran is dropped, said.
func (m *mockService) carryDeliveries(name string, prior map[string][]PriorDelivery, windows []models.ScopeWindow, byCase map[string]models.MappedTestCase, byTest map[string][]models.MockEntry, cases, mocks []capturedMock) {
	var ran []string
	seen := make(map[string]bool)
	for _, w := range windows {
		if len(prior[w.Name]) == 0 || seen[w.Name] {
			continue
		}
		seen[w.Name] = true
		ran = append(ran, w.Name)
	}
	if len(ran) == 0 {
		return
	}
	if deliveryCarrier == nil {
		for _, test := range ran {
			for _, d := range prior[test] {
				m.logger.Warn(fmt.Sprintf("test %q: the deliver: entry {case: %s, after: %s} of the previous recording is dropped: this keploy cannot find an entry's case and after: call again in a new recording; to deliver its callback again, add the entry back under the test's deliver: in mappings.yaml with the names its case and its after: call have in this recording", test, d.Case, d.After),
					zap.String("mock-set", name))
			}
		}
		return
	}
	callOf := make(map[string]string, len(cases)+len(mocks))
	for _, c := range cases {
		callOf[c.name] = c.call
	}
	for _, mk := range mocks {
		callOf[mk.name] = mk.call
	}
	named := func(names []string) []NamedCall {
		out := make([]NamedCall, 0, len(names))
		for _, n := range names {
			out = append(out, NamedCall{Name: n, Call: callOf[n]})
		}
		return out
	}
	tests := make([]CarryTest, 0, len(ran))
	for _, test := range ran {
		var calls []string
		for _, e := range byTest[test] {
			calls = append(calls, e.Name)
		}
		tests = append(tests, CarryTest{Name: test, Cases: named(byCase[test].Cases), Mocks: named(calls)})
	}
	kept, lines := deliveryCarrier.Carry(prior, tests)
	for _, t := range tests {
		if len(kept[t.Name]) == 0 {
			continue
		}
		tc := byCase[t.Name]
		tc.Deliver = append(tc.Deliver, kept[t.Name]...)
		byCase[t.Name] = tc
	}
	for _, l := range lines {
		if l.Kept {
			m.logger.Info(l.Msg, zap.String("mock-set", name))
		} else {
			m.logger.Warn(l.Msg, zap.String("mock-set", name))
		}
	}
}
