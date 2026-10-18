package models

import (
	"reflect"
	"testing"
	"time"
)

var wm0 = time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)

func wmAt(ms int) time.Time { return wm0.Add(time.Duration(ms) * time.Millisecond) }

func wmWindow(test string, from, to int) TestWindow {
	return TestWindow{TestCase: test, Start: wmAt(from), End: wmAt(to)}
}

func wmMock(name string, at int) WindowedMock {
	return WindowedMock{Name: name, Kind: HTTP, At: wmAt(at), Res: wmAt(at + 1), TestMock: true}
}

// mappedNames is each test's derived mapping by mock name, in its order.
func mappedNames(m WindowMappings) map[string][]string {
	out := make(map[string][]string, len(m.Tests))
	for _, tc := range m.Tests {
		names := []string{}
		for _, e := range tc.Mocks {
			names = append(names, e.Name)
		}
		out[tc.ID] = names
	}
	return out
}

// Sequential tests: a test owns the test mocks recorded inside its window, in
// recorded order, both ends included; the start-up mocks before the first
// window, the gaps between windows, and mocks a test may not own belong to no
// test. Every window has an entry, an empty one when nothing is inside it.
func TestDeriveWindowMappingsSequentialWindows(t *testing.T) {
	windows := []TestWindow{
		wmWindow("test-2", 2000, 2100),
		wmWindow("test-1", 1000, 1100),
		wmWindow("test-3", 3000, 3100),
	}
	mocks := []WindowedMock{
		wmMock("boot", 500),
		wmMock("m-1b", 1100), // at the window's end
		wmMock("m-1a", 1000), // at its start, recorded first
		wmMock("gap", 1500),
		{Name: "handshake", At: wmAt(2010), TestMock: false},
		wmMock("m-2", 2050),
		wmMock("untimed", 0),
		wmMock("after", 3200),
	}
	mocks[6].At = time.Time{}
	got := DeriveWindowMappings(windows, mocks)
	want := map[string][]string{"test-1": {"m-1a", "m-1b"}, "test-2": {"m-2"}, "test-3": {}}
	if names := mappedNames(got); !reflect.DeepEqual(names, want) {
		t.Fatalf("mapped %v; want %v", names, want)
	}
	if order := []string{got.Tests[0].ID, got.Tests[1].ID, got.Tests[2].ID}; !reflect.DeepEqual(order, []string{"test-1", "test-2", "test-3"}) {
		t.Fatalf("tests in order %v; want window order", order)
	}
	if got.Concurrent != nil {
		t.Fatalf("sequential tests reported as concurrent: %v", got.Concurrent)
	}
	if e := got.Tests[1].Mocks[0]; e.Kind != string(HTTP) || e.ReqTimestampMock != FormatMockTimestamp(wmAt(2050)) ||
		e.ResTimestampMock != FormatMockTimestamp(wmAt(2051)) || e.Timestamp != wmAt(2050).Unix() {
		t.Fatalf("the entry does not carry the mock's kind and recorded times: %+v", e)
	}
}

// Two windows open at once: a mock recorded while both are open goes to the
// one that opened first, and both tests are named as concurrent. A mock
// recorded once only one of them is open goes to that one and makes nobody
// concurrent.
func TestDeriveWindowMappingsOverlappingWindows(t *testing.T) {
	t.Run("nested", func(t *testing.T) {
		got := DeriveWindowMappings(
			[]TestWindow{wmWindow("outer", 1000, 2000), wmWindow("inner", 1300, 1500), wmWindow("later", 3000, 3100)},
			[]WindowedMock{wmMock("both-open", 1400), wmMock("outer-only", 1800), wmMock("later", 3050)},
		)
		want := map[string][]string{"outer": {"both-open", "outer-only"}, "inner": {}, "later": {"later"}}
		if names := mappedNames(got); !reflect.DeepEqual(names, want) {
			t.Fatalf("mapped %v; want %v", names, want)
		}
		if !reflect.DeepEqual(got.Concurrent, []string{"outer", "inner"}) {
			t.Fatalf("concurrent = %v; want [outer inner]", got.Concurrent)
		}
	})
	t.Run("the first one ended", func(t *testing.T) {
		got := DeriveWindowMappings(
			[]TestWindow{wmWindow("a", 1000, 1200), wmWindow("b", 1100, 1500)},
			[]WindowedMock{wmMock("after-a", 1300)},
		)
		want := map[string][]string{"a": {}, "b": {"after-a"}}
		if names := mappedNames(got); !reflect.DeepEqual(names, want) {
			t.Fatalf("mapped %v; want %v", names, want)
		}
		if got.Concurrent != nil {
			t.Fatalf("no mock was recorded while both were open, yet concurrent = %v", got.Concurrent)
		}
	})
	t.Run("an earlier window ended inside a later-started one", func(t *testing.T) {
		// a [1000,1200], b [1100,1900], c [1150,1300]: at 1250 a has ended,
		// and b, opened before c, owns the mock.
		got := DeriveWindowMappings(
			[]TestWindow{wmWindow("a", 1000, 1200), wmWindow("b", 1100, 1900), wmWindow("c", 1150, 1300)},
			[]WindowedMock{wmMock("m", 1250)},
		)
		if names := mappedNames(got); !reflect.DeepEqual(names["b"], []string{"m"}) {
			t.Fatalf("mapped %v; want m to b", names)
		}
		if !reflect.DeepEqual(got.Concurrent, []string{"b", "c"}) {
			t.Fatalf("concurrent = %v; want [b c]", got.Concurrent)
		}
	})
}

// A window with no start opens nothing; one with no end, or an end before its
// start, ends at its start.
func TestDeriveWindowMappingsDegenerateWindows(t *testing.T) {
	noStart := TestWindow{TestCase: "no-start", End: wmAt(5000)}
	noEnd := TestWindow{TestCase: "no-end", Start: wmAt(1000)}
	backwards := TestWindow{TestCase: "backwards", Start: wmAt(2000), End: wmAt(1000)}
	got := DeriveWindowMappings(
		[]TestWindow{noStart, noEnd, backwards},
		[]WindowedMock{wmMock("at-1000", 1000), wmMock("at-1001", 1001), wmMock("at-2000", 2000), wmMock("at-4000", 4000)},
	)
	want := map[string][]string{"no-end": {"at-1000"}, "backwards": {"at-2000"}}
	if names := mappedNames(got); !reflect.DeepEqual(names, want) {
		t.Fatalf("mapped %v; want %v", names, want)
	}
}

// Which recorded mocks a test may own: data-plane mocks, by what was recorded,
// that no async lane serves.
func TestNewWindowedMockTestMocks(t *testing.T) {
	at := wmAt(1000)
	mock := func(kind Kind, tag string) *Mock {
		m := &Mock{Kind: kind, Name: "mock-1"}
		m.Spec.ReqTimestampMock = at
		m.Spec.ResTimestampMock = at.Add(time.Millisecond)
		if tag != "" {
			m.Spec.Metadata = map[string]string{"type": tag}
		}
		return m
	}
	async := mock(HTTP, "")
	async.Spec.Async = &AsyncMeta{Lane: "events", Seq: 3}
	for _, tc := range []struct {
		name string
		mock *Mock
		want bool
	}{
		{"untagged HTTP", mock(HTTP, ""), true},
		{"a recorder's data mock", mock(Postgres, "mocks"), true},
		{"a connection failure", mock(ConnectionFailure, ""), true},
		{"a config mock", mock(Postgres, "config"), false},
		{"a connection mock", mock(MySQL, "connection"), false},
		{"DNS", mock(DNS, ""), false},
		{"an async lane's mock", async, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := NewWindowedMock(tc.mock)
			if w.TestMock != tc.want {
				t.Fatalf("TestMock = %v; want %v", w.TestMock, tc.want)
			}
			if !w.At.Equal(at) || !w.Res.Equal(at.Add(time.Millisecond)) || w.Name != "mock-1" || w.Kind != tc.mock.Kind {
				t.Fatalf("windowed mock = %+v", w)
			}
		})
	}
}

// The window a recorded test case opens: HTTP and gRPC only.
func TestRecordedWindow(t *testing.T) {
	httpTC := &TestCase{Name: "h", Kind: HTTP, HTTPReq: HTTPReq{Timestamp: wmAt(1)}, HTTPResp: HTTPResp{Timestamp: wmAt(2)}}
	grpcTC := &TestCase{Name: "g", Kind: GRPC_EXPORT, GrpcReq: GrpcReq{Timestamp: wmAt(3)}, GrpcResp: GrpcResp{Timestamp: wmAt(4)}}
	other := &TestCase{Name: "o", Kind: Kind("Other"), HTTPReq: HTTPReq{Timestamp: wmAt(5)}}
	if w := RecordedWindow(httpTC); w != (TestWindow{TestCase: "h", Start: wmAt(1), End: wmAt(2)}) {
		t.Fatalf("HTTP window %+v", w)
	}
	if w := RecordedWindow(grpcTC); w != (TestWindow{TestCase: "g", Start: wmAt(3), End: wmAt(4)}) {
		t.Fatalf("gRPC window %+v", w)
	}
	if w := RecordedWindow(other); !w.Start.IsZero() {
		t.Fatalf("a %s test case opened a window: %+v", other.Kind, w)
	}
	if w := RecordedWindow(nil); w != (TestWindow{}) {
		t.Fatalf("nil test case: %+v", w)
	}
}

// A call sent in the gap before a test and answered at or after its start (a
// consumer's long poll) was in flight when that test began, and is its own,
// first in its list; one sent and answered in the gap, one sent before the
// first test, and one the in-flight rule does not apply to (a carry-over or
// window-bound mock, a session mock: WindowedMock.InFlight) belong to no test.
func TestDeriveWindowMappingsGivesAnInFlightGapCallToItsTest(t *testing.T) {
	windows := []TestWindow{wmWindow("test-1", 1000, 1100), wmWindow("test-2", 2000, 2100)}
	inFlight := WindowedMock{Name: "fetch-o2", Kind: HTTP, At: wmAt(1500), Res: wmAt(2050), TestMock: true, InFlight: true}
	lateAnswer := WindowedMock{Name: "fetch-late", Kind: HTTP, At: wmAt(1600), Res: wmAt(2500), TestMock: true, InFlight: true}
	atStart := WindowedMock{Name: "answered-at-start", Kind: HTTP, At: wmAt(1650), Res: wmAt(2000), TestMock: true, InFlight: true}
	atGapStart := WindowedMock{Name: "sent-at-gap-start", Kind: HTTP, At: wmAt(1100), Res: wmAt(2050), TestMock: true, InFlight: true}
	beforeStart := WindowedMock{Name: "answered-before-start", Kind: HTTP, At: wmAt(1660), Res: wmAt(1999), TestMock: true, InFlight: true}
	bootInFlight := WindowedMock{Name: "boot-fetch", Kind: HTTP, At: wmAt(500), Res: wmAt(1050), TestMock: true, InFlight: true}
	notEligible := WindowedMock{Name: "carry-or-failure", Kind: ConnectionFailure, At: wmAt(1550), Res: wmAt(2050), TestMock: true}
	got := DeriveWindowMappings(windows, []WindowedMock{
		wmMock("own-1", 1050), atGapStart, inFlight, notEligible, lateAnswer, atStart, beforeStart,
		wmMock("gap", 1700), bootInFlight, wmMock("own-2", 2010),
	})
	want := map[string][]string{
		"test-1": {"own-1", "sent-at-gap-start"},
		"test-2": {"fetch-o2", "fetch-late", "answered-at-start", "own-2"},
	}
	if names := mappedNames(got); !reflect.DeepEqual(names, want) {
		t.Fatalf("mapped %v; want %v", names, want)
	}
}

// Two tests that start at the same instant: which of them a call in flight
// then belongs to cannot be told by the start the agent knows a window by, so
// neither gets it (WindowSchedule.GapBefore), as the mock manager serves it to
// neither.
func TestDeriveWindowMappingsGivesAnInFlightCallToNeitherOfTwoTestsThatStartTogether(t *testing.T) {
	windows := []TestWindow{wmWindow("a", 1000, 1100), wmWindow("b", 2000, 2100), wmWindow("c", 2000, 2200)}
	inFlight := WindowedMock{Name: "fetch", Kind: HTTP, At: wmAt(1500), Res: wmAt(2050), TestMock: true, InFlight: true}
	got := DeriveWindowMappings(windows, []WindowedMock{inFlight, wmMock("own-b", 2010)})
	want := map[string][]string{"a": {}, "b": {"own-b"}, "c": {}}
	if names := mappedNames(got); !reflect.DeepEqual(names, want) {
		t.Fatalf("mapped %v; want %v", names, want)
	}
	if from, ok := NewWindowSchedule(windows).GapBefore(wmAt(2000)); ok {
		t.Fatalf("GapBefore(the shared start) = %v, true; want no gap", from)
	}
}

// NewWindowedMock applies the in-flight rule to a per-test data mock only: not
// to a connection failure, nor to a session mock, whether its lifetime was
// derived already or is read from its recording.
func TestNewWindowedMockSaysWhetherTheInFlightRuleApplies(t *testing.T) {
	stated := func(kind Kind, lifetime string, derived bool) *Mock {
		m := &Mock{Name: "m", Kind: kind, Spec: MockSpec{Metadata: map[string]string{MetadataLifetimeKey: lifetime, "type": "mocks"},
			ReqTimestampMock: wmAt(1), ResTimestampMock: wmAt(2)}}
		if derived {
			m.DeriveLifetime()
		}
		return m
	}
	for _, c := range []struct {
		m    *Mock
		want bool
	}{
		{stated(HTTP, MetadataLifetimePerTest, false), true},
		{stated(HTTP, MetadataLifetimePerTest, true), true},
		{stated(HTTP, MetadataLifetimeSession, false), false},
		{stated(HTTP, MetadataLifetimeSession, true), false},
		{stated(ConnectionFailure, MetadataLifetimePerTest, false), false},
	} {
		derivedBefore := c.m.TestModeInfo.LifetimeDerived
		if got := NewWindowedMock(c.m).InFlight; got != c.want {
			t.Errorf("%s %v (derived %v): InFlight %v, want %v", c.m.Kind, c.m.Spec.Metadata, derivedBefore, got, c.want)
		}
		if c.m.TestModeInfo.LifetimeDerived != derivedBefore {
			t.Errorf("NewWindowedMock derived the mock's own lifetime")
		}
	}
}

// workerPlaces is the worker scope's mocks, each "name@place", in order.
func workerPlaces(m WindowMappings) []string {
	out := []string{}
	for _, e := range m.Worker {
		out = append(out, e.Name+"@"+e.Place)
	}
	return out
}

// A test mock recorded while no window was open, which the in-flight rule
// gives to no test, is the set's worker scope (D2), in recorded order, each
// placed after the window that began last before it, or at the set's start
// before every window: the app's start, the gap between two tests, and what
// came after the last test. A mock no test may own (a handshake) and one with
// no recorded time are not in it, and a gap call the in-flight rule gives to
// a test is that test's alone.
func TestDeriveWindowMappingsPlacesWhatNoTestOwnsInTheWorkerScope(t *testing.T) {
	windows := []TestWindow{wmWindow("test-2", 2000, 2100), wmWindow("test-1", 1000, 1100), wmWindow("test-3", 3000, 3100)}
	inFlight := WindowedMock{Name: "fetch-in-flight", Kind: HTTP, At: wmAt(1500), Res: wmAt(2050), TestMock: true, InFlight: true}
	answeredInGap := WindowedMock{Name: "poll-in-gap", Kind: HTTP, At: wmAt(1600), Res: wmAt(1900), TestMock: true, InFlight: true}
	untimed := wmMock("untimed", 0)
	untimed.At = time.Time{}
	got := DeriveWindowMappings(windows, []WindowedMock{
		wmMock("boot", 500),
		wmMock("m-1", 1050),
		wmMock("gap-1", 1400),
		inFlight,
		answeredInGap,
		{Name: "handshake", At: wmAt(1700), TestMock: false},
		wmMock("m-2", 2050),
		wmMock("gap-2", 2500),
		untimed,
		wmMock("after", 3200),
	})
	want := map[string][]string{"test-1": {"m-1"}, "test-2": {"fetch-in-flight", "m-2"}, "test-3": {}}
	if names := mappedNames(got); !reflect.DeepEqual(names, want) {
		t.Fatalf("mapped %v; want %v", names, want)
	}
	wantWorker := []string{"boot@", "gap-1@test-1", "poll-in-gap@test-1", "gap-2@test-2", "after@test-3"}
	if w := workerPlaces(got); !reflect.DeepEqual(w, wantWorker) {
		t.Fatalf("worker scope %v; want %v", w, wantWorker)
	}
	if e := got.Worker[1]; e.Kind != string(HTTP) || e.ReqTimestampMock != FormatMockTimestamp(wmAt(1400)) {
		t.Fatalf("a worker entry does not carry the mock's kind and recorded time: %+v", e)
	}
	// No window at all: everything is the scope's, at the set's start.
	if w := workerPlaces(DeriveWindowMappings(nil, []WindowedMock{wmMock("a", 10), wmMock("b", 20)})); !reflect.DeepEqual(w, []string{"a@", "b@"}) {
		t.Fatalf("with no window the worker scope is %v; want [a@ b@]", w)
	}
}
