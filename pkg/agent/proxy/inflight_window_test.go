package proxy

import (
	"net/http"
	"sort"
	"testing"
	"time"

	httpint "go.keploy.io/server/v3/pkg/agent/proxy/integrations/http"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// inFlightMock is a per-test HTTP mock whose request was recorded at req ms
// and its response at res ms (pms).
func inFlightMock(name string, req, res int) *models.Mock {
	return &models.Mock{Name: name, Kind: models.HTTP, Spec: models.MockSpec{
		Metadata:         map[string]string{"type": "mocks"},
		ReqTimestampMock: pms(req), ResTimestampMock: pms(res)},
		TestModeInfo: models.TestModeInfo{Lifetime: models.LifetimePerTest, LifetimeDerived: true}}
}

// A consumer keeps a fetch in flight between tests: the one sent in the gap
// before test-2 (after test-1's window ended) and answered inside test-2's
// window carries what test-2 waits for. Staged for test-2, it is in test-2's
// per-test tier, as is one answered after test-2 ended; one sent and
// answered in the gap, and one sent inside test-1's window, are in no tier.
// Without recorded windows nothing changes.
func TestTheManagerGivesACallInFlightWhenATestBeginsToThatTest(t *testing.T) {
	recording := func() []*models.Mock {
		return []*models.Mock{
			inFlightMock("own-2", 2010, 2011),
			inFlightMock("fetch-o2", 1500, 2050),   // sent in the gap, answered in test-2
			inFlightMock("fetch-late", 1600, 2500), // answered after test-2 ended
			inFlightMock("gap-only", 1700, 1800),   // sent and answered in the gap
			inFlightMock("t1-long", 1050, 2050),    // sent inside test-1's window
		}
	}
	perTestNames := func(mm *MockManager) []string {
		got, _ := mm.GetFilteredMocks()
		var names []string
		for _, mk := range got {
			names = append(names, mk.Name)
		}
		sort.Strings(names)
		return names
	}
	for _, seeded := range []bool{true, false} {
		mm := NewMockManager(nil, nil, zap.NewNop())
		mm.ResetForReplaySession()
		mm.SeedStartupCutoff(winStart(1))
		if seeded {
			mm.SeedRecordedWindows(windowsOf("test-1", "test-2"))
		}
		mm.SetMocksWithWindow(nil, nil, models.BaseTime, time.Now())
		mm.SetMocksWithWindow(recording(), nil, winStart(2), winEnd(2))
		want := []string{"fetch-late", "fetch-o2", "own-2"}
		if !seeded {
			want = []string{"own-2"}
		}
		if got := perTestNames(mm); !equalStrings(got, want) {
			t.Fatalf("seeded=%v: test-2's per-test tier holds %v; want %v", seeded, got, want)
		}
		if from, ok := mm.InFlightFrom(winStart(2)); seeded && (!ok || !from.Equal(winEnd(1))) {
			t.Fatalf("InFlightFrom(test-2) = %v, %v; want test-1's end", from, ok)
		}
		mm.Close()
	}
}

// The HTTP integration reads a test's per-test mocks through the windowed
// accessor (GetPerTestMocksInWindow), which checks the window again: the call
// in flight when test-2 began, which the staging filed in test-2's tier, is
// served to test-2 there too, beside a call of test-2's own window.
func TestHTTPServesACallInFlightWhenItsTestBegins(t *testing.T) {
	httpMock := func(name string, req, res int) *models.Mock {
		return &models.Mock{Version: "api.keploy.io/v1beta1", Name: name, Kind: models.HTTP, Spec: models.MockSpec{
			Metadata: map[string]string{"type": "mocks"},
			HTTPReq:  &models.HTTPReq{Method: "GET", URL: "http://deps/" + name, ProtoMajor: 1, ProtoMinor: 1},
			// As recorded: the serializer recomputes a recorded Content-Length.
			HTTPResp:         &models.HTTPResp{StatusCode: 200, Body: name, Header: map[string]string{"Content-Length": "0"}},
			ReqTimestampMock: pms(req), ResTimestampMock: pms(res)},
			TestModeInfo: models.TestModeInfo{Lifetime: models.LifetimePerTest, LifetimeDerived: true}}
	}
	mm := NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(mm.Close)
	mm.ResetForReplaySession()
	mm.SeedStartupCutoff(winStart(1))
	mm.SeedRecordedWindows(windowsOf("test-1", "test-2"))
	mm.SetMocksWithWindow(nil, nil, models.BaseTime, time.Now())
	mm.SetMocksWithWindow([]*models.Mock{httpMock("own-2", 2010, 2011), httpMock("fetch-o2", 1500, 2050)}, nil, winStart(2), winEnd(2))

	inWindow, err := mm.GetPerTestMocksInWindow()
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, mk := range inWindow {
		names = append(names, mk.Name)
	}
	sort.Strings(names)
	if !equalStrings(names, []string{"fetch-o2", "own-2"}) {
		t.Fatalf("GetPerTestMocksInWindow during test-2 = %v; want the call in flight and test-2's own", names)
	}
	h := httpint.New(zap.NewNop())
	for _, name := range []string{"own-2", "fetch-o2"} {
		resp, body, err := serveOne(t, h, mm, "GET /"+name+" HTTP/1.1\r\nHost: deps\r\n\r\n")
		if resp == nil {
			t.Fatalf("GET /%s: no response; the integration returned %v", name, err)
		}
		if resp.StatusCode != http.StatusOK || string(body) != name {
			t.Fatalf("GET /%s answered %d %q; want 200 %q", name, resp.StatusCode, body, name)
		}
	}
}

// The in-flight rule is for a test's own calls only. A carry-over mock (a
// Pulsar SEND's receipt) sent in the gap after test-1 and answered once test-2
// began is test-1's by the release rule: the staging files it in the
// carry-over tier, not test-2's per-test tier, and the windowed read does not
// list it either. Nor does a session mock that reached the per-test slice.
func TestACarryOverMockInFlightStaysOnTheCarryOverTier(t *testing.T) {
	registerSends(t)
	send := brokerMockAt("send-gap", pulsarKind, "SEND", 1500)
	send.Spec.ResTimestampMock = pms(2050)
	session := inFlightMock("session-gap", 1550, 2050)
	session.TestModeInfo.Lifetime = models.LifetimeSession
	mm := NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(mm.Close)
	mm.ResetForReplaySession()
	mm.SeedStartupCutoff(winStart(1))
	mm.SeedRecordedWindows(windowsOf("test-1", "test-2"))
	mm.SetMocksWithWindow(nil, nil, models.BaseTime, time.Now())
	mm.SetMocksWithWindow([]*models.Mock{inFlightMock("own-2", 2010, 2011), send, session}, nil, winStart(2), winEnd(2))

	for name, read := range map[string]func() ([]*models.Mock, error){
		"GetFilteredMocks":        mm.GetFilteredMocks,
		"GetPerTestMocksInWindow": mm.GetPerTestMocksInWindow,
	} {
		got, _ := read()
		var names []string
		for _, mk := range got {
			names = append(names, mk.Name)
		}
		if !equalStrings(names, []string{"own-2"}) {
			t.Fatalf("%s during test-2 = %v; want test-2's own only", name, names)
		}
	}
	carry, _ := mm.GetCarryOverMocks()
	if len(carry) != 1 || carry[0].Name != "send-gap" {
		t.Fatalf("the carry-over tier holds %v; want the SEND", carry)
	}
}

// The agent's disk store loads, for a window, the calls in flight at its
// start: sent after the gap's start, answered at or after the window's start.
func TestDiskMocksLoadInFlight(t *testing.T) {
	d, err := NewDiskMocks(zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.Close() }()
	failure := inFlightMock("failure-gap", 1550, 2050)
	failure.Kind = models.ConnectionFailure
	for _, mk := range []*models.Mock{
		inFlightMock("t1-long", 1050, 2050),
		inFlightMock("sent-at-gap-start", 1100, 2050),
		inFlightMock("fetch-o2", 1500, 2050),
		failure,
		inFlightMock("gap-only", 1700, 1800),
		inFlightMock("answered-just-before", 1750, 1999),
		inFlightMock("fetch-at-start", 1800, 2000),
		inFlightMock("own-2", 2010, 2011),
	} {
		if err := d.Add(mk); err != nil {
			t.Fatal(err)
		}
	}
	d.Finalize()
	got, err := d.LoadInFlight(winEnd(1), winStart(2))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, mk := range got {
		names = append(names, mk.Name)
	}
	if !equalStrings(names, []string{"fetch-o2", "fetch-at-start"}) {
		t.Fatalf("loaded %v; want the two sent in the gap and answered from test-2's start", names)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// A kind registered carry-over marks a per-test mock of it consumed through
// MarkMockAsUsed outside the current window CarryOver (a server push delivered
// late), and the replay maps it by its release window. A call in flight when
// the current window's test began is that window's: its tier holds it, and
// it is mapped to that test whichever door consumes it. One consumed outside
// the window and not in flight is still flagged.
func TestACallInFlightConsumedThroughMarkMockAsUsedIsTheWindowsOwn(t *testing.T) {
	registerSends(t)
	lookup := func(name string, req, res int) *models.Mock {
		m := brokerMockAt(name, pulsarKind, "LOOKUP", req)
		m.Spec.ResTimestampMock = pms(res)
		return m
	}
	inFlight, stale := lookup("lookup-in-flight", 1500, 2050), lookup("lookup-stale", 1050, 1060)
	if !models.InFlightEligible(inFlight) || !models.MarksOutOfWindow(inFlight) {
		t.Fatalf("the fixture: in-flight eligible %v, marks out of window %v; want both", models.InFlightEligible(inFlight), models.MarksOutOfWindow(inFlight))
	}
	mm := NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(mm.Close)
	mm.ResetForReplaySession()
	mm.SeedStartupCutoff(winStart(1))
	mm.SeedRecordedWindows(windowsOf("test-1", "test-2"))
	mm.SetMocksWithWindow(nil, nil, models.BaseTime, time.Now())
	mm.SetMocksWithWindow([]*models.Mock{inFlight, stale}, nil, winStart(2), winEnd(2))
	mm.GetConsumedMocks()
	if !mm.MarkMockAsUsed(*inFlight) {
		t.Fatal("the call in flight was refused")
	}
	mm.MarkMockAsUsed(*stale)
	flagged := map[string]bool{}
	for _, st := range mm.GetConsumedMocks() {
		flagged[st.Name] = st.CarryOver
	}
	if c, ok := flagged["lookup-in-flight"]; !ok || c {
		t.Fatalf("the call in flight consumed in test-2: recorded %v, flagged CarryOver %v; want recorded, not flagged", ok, c)
	}
	if c, ok := flagged["lookup-stale"]; ok && !c {
		t.Fatal("a mock of test-1's window consumed in test-2 was not flagged CarryOver")
	}
}
