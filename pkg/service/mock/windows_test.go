package mock

import (
	"context"
	"reflect"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

func windowMock(name string, t0 time.Time, req, res int) *models.Mock {
	return &models.Mock{Name: name, Spec: models.MockSpec{
		ReqTimestampMock: t0.Add(time.Duration(req) * time.Second),
		ResTimestampMock: t0.Add(time.Duration(res) * time.Second),
	}}
}

// keploy mock replay sends the agent the recorded window of every test of the
// set, from its mappings: from the test's recorded begin (also where a
// recording it lists was sent before it: in flight into it) to the last place
// (models.PlaceOf: here each request's time) of the recordings it lists. A
// test that lists none spans its begin alone, a test with no begin starts at
// its earliest recording, a test with neither has none, a test listed twice
// begins at its earliest begin, and windows that overlap or touch are one
// window no test names.
func TestRecordedTestWindowsAreTheMappingsTests(t *testing.T) {
	t0 := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	at := func(s int) time.Time { return t0.Add(time.Duration(s) * time.Second) }
	decoded := []*models.Mock{
		windowMock("boot", t0, 0, 1),         // the pool's
		windowMock("a-1", t0, 11, 12),        // TestA's
		windowMock("a-2", t0, 14, 15),        // TestA's
		windowMock("b-inflight", t0, 19, 21), // TestB's, sent before it began
		windowMock("b-1", t0, 22, 23),        // TestB's
		windowMock("end", t0, 90, 91),        // the pool's: after every test
	}
	for _, c := range []struct {
		name  string
		mp    *models.Mapping
		table map[string][]string
		want  []models.TestWindow
	}{
		{
			name:  "one window a test",
			mp:    &models.Mapping{TestCases: []models.MappedTestCase{{ID: "o::TestA", At: at(10)}, {ID: "o::TestB", At: at(20)}}},
			table: map[string][]string{"o::TestA": {"a-1", "a-2"}, "o::TestB": {"b-inflight", "b-1"}},
			want: []models.TestWindow{
				{TestCase: "o::TestA", Start: at(10), End: at(14)},
				{TestCase: "o::TestB", Start: at(20), End: at(22)},
			},
		},
		{
			name: "a test listed twice begins at its earliest begin",
			mp: &models.Mapping{TestCases: []models.MappedTestCase{
				{ID: "o::TestA", At: at(10)}, {ID: "o::TestB", At: at(21)}, {ID: "o::TestB", At: at(20)}, {ID: "o::TestB"},
			}},
			table: map[string][]string{"o::TestA": {"a-1", "a-2"}, "o::TestB": {"b-1"}},
			want: []models.TestWindow{
				{TestCase: "o::TestA", Start: at(10), End: at(14)},
				{TestCase: "o::TestB", Start: at(20), End: at(22)},
			},
		},
		{
			name:  "a test listing no recording spans its begin; one with no begin and nothing timed has none",
			mp:    &models.Mapping{TestCases: []models.MappedTestCase{{ID: "o::TestA", At: at(10)}, {ID: "o::TestC", At: at(30)}, {ID: "o::TestD"}}},
			table: map[string][]string{"o::TestA": {"a-1", "a-2"}, "o::TestC": {}, "o::TestD": {"not-decoded"}},
			want: []models.TestWindow{
				{TestCase: "o::TestA", Start: at(10), End: at(14)},
				{TestCase: "o::TestC", Start: at(30), End: at(30)},
			},
		},
		{
			name:  "a recording the mapping lists of a test the mappings give no begin",
			mp:    nil,
			table: map[string][]string{"o::TestB": {"b-1"}},
			want:  []models.TestWindow{{TestCase: "o::TestB", Start: at(22), End: at(22)}},
		},
		{
			name:  "windows that touch at one instant are one, named by no test",
			mp:    &models.Mapping{TestCases: []models.MappedTestCase{{ID: "o::TestA", At: at(10)}, {ID: "o::TestB", At: at(14)}}},
			table: map[string][]string{"o::TestA": {"a-1", "a-2"}, "o::TestB": {"b-1"}},
			want:  []models.TestWindow{{Start: at(10), End: at(22), Tests: []string{"o::TestA", "o::TestB"}}},
		},
		{
			name:  "three overlapping windows are one, listing each test",
			mp:    &models.Mapping{TestCases: []models.MappedTestCase{{ID: "o::TestA", At: at(10)}, {ID: "o::TestB", At: at(13)}, {ID: "o::TestC", At: at(20)}}},
			table: map[string][]string{"o::TestA": {"a-1", "a-2"}, "o::TestB": {"b-1"}, "o::TestC": {}},
			want:  []models.TestWindow{{Start: at(10), End: at(22), Tests: []string{"o::TestA", "o::TestB", "o::TestC"}}},
		},
		{
			name:  "overlapping windows are one, named by no test",
			mp:    &models.Mapping{TestCases: []models.MappedTestCase{{ID: "o::TestA", At: at(10)}, {ID: "o::TestB", At: at(13)}, {ID: "o::TestC", At: at(30)}}},
			table: map[string][]string{"o::TestA": {"a-1", "a-2"}, "o::TestB": {"b-1"}, "o::TestC": {}},
			want: []models.TestWindow{
				{Start: at(10), End: at(22), Tests: []string{"o::TestA", "o::TestB"}},
				{TestCase: "o::TestC", Start: at(30), End: at(30)},
			},
		},
	} {
		if got := recordedTestWindows(c.mp, c.table, decoded); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: windows\n %v\nwant\n %v", c.name, got, c.want)
		}
	}
}

// What the agent then tells the learn step (integrations.TestWindows, the
// agent's MockManager.InRecordedTestWindow and SameRecordedTestWindow): a
// recording of another test is inside a window that is not the paired
// recording's (so it is that test's, not an earlier place of the paired
// one), one of the same test shares its window, and a pool recording is
// inside none. The set's teardown (a driver's endSessions after the last
// test) is later than every window's end, and no test's recording is.
func TestRecordedTestWindowsTellAnotherTestsRecording(t *testing.T) {
	t0 := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	decoded := []*models.Mock{
		windowMock("boot", t0, 0, 1),
		windowMock("a-1", t0, 11, 12),
		windowMock("a-2", t0, 14, 15),
		windowMock("b-1", t0, 22, 23),
		windowMock("b-2", t0, 24, 25),
		windowMock("endSessions", t0, 90, 91),
	}
	mp := &models.Mapping{TestCases: []models.MappedTestCase{{ID: "o::TestA", At: t0.Add(10 * time.Second)}, {ID: "o::TestB", At: t0.Add(20 * time.Second)}}}
	table := map[string][]string{"o::TestA": {"a-1", "a-2"}, "o::TestB": {"b-1", "b-2"}}
	s := models.NewWindowSchedule(recordedTestWindows(mp, table, decoded))
	byName := map[string]*models.Mock{}
	for _, mk := range decoded {
		byName[mk.Name] = mk
	}
	req := func(name string) time.Time { return byName[name].Spec.ReqTimestampMock }
	same := func(a, b string) bool {
		i := s.IndexOf(req(a))
		return i >= 0 && i == s.IndexOf(req(b))
	}
	paired := "b-2"
	if !s.Contains(req(paired)) {
		t.Fatalf("the paired recording %s is in no window", paired)
	}
	if !s.Contains(req("a-1")) || same("a-1", paired) {
		t.Errorf("TestA's recording: inside %v, same window as %s %v; want inside another test's window", s.Contains(req("a-1")), paired, same("a-1", paired))
	}
	if !same("b-1", paired) {
		t.Errorf("TestB's own earlier recording is not in %s's window", paired)
	}
	for _, pool := range []string{"boot", "endSessions"} {
		if s.Contains(req(pool)) {
			t.Errorf("the pool's %s is inside a test's window", pool)
		}
	}
	var lastEnd time.Time
	for i := 0; i < s.Len(); i++ {
		if e := s.Window(i).End; e.After(lastEnd) {
			lastEnd = e
		}
	}
	if !req("endSessions").After(lastEnd) {
		t.Errorf("the teardown's endSessions (%v) is not after every window's end (%v)", req("endSessions"), lastEnd)
	}
	for _, name := range []string{"a-1", "a-2", "b-1", "b-2"} {
		if req(name).After(lastEnd) {
			t.Errorf("a test's recording %s is after every window's end", name)
		}
	}
}

// A test's window starts at its begin, as keploy test's starts at its test
// case's request, even where a recording it lists was sent before it: the
// agent gives that in-flight call to the test through the gap rule
// (models.WindowSchedule.InFlightInto), and a pool recording made in the gap
// while no test was open stays outside every window, so the learn step reads
// it as the pool's, not as the next test's.
func TestAPoolRecordingInTheInFlightGapIsOutsideEveryWindow(t *testing.T) {
	t0 := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	at := func(s int) time.Time { return t0.Add(time.Duration(s) * time.Second) }
	decoded := []*models.Mock{
		windowMock("a-1", t0, 11, 12),        // TestA's
		windowMock("b-inflight", t0, 15, 21), // TestB's: sent in the gap, answered inside TestB
		windowMock("gap-pool", t0, 17, 18),   // the pool's: made in the gap, no test open
		windowMock("b-1", t0, 22, 23),        // TestB's
	}
	mp := &models.Mapping{TestCases: []models.MappedTestCase{{ID: "o::TestA", At: at(10)}, {ID: "o::TestB", At: at(20)}}}
	table := map[string][]string{"o::TestA": {"a-1"}, "o::TestB": {"b-inflight", "b-1"}}
	ws := recordedTestWindows(mp, table, decoded)
	if want := []models.TestWindow{{TestCase: "o::TestA", Start: at(10), End: at(11)}, {TestCase: "o::TestB", Start: at(20), End: at(22)}}; !reflect.DeepEqual(ws, want) {
		t.Fatalf("windows\n %v\nwant\n %v", ws, want)
	}
	s := models.NewWindowSchedule(ws)
	if s.Contains(at(17)) {
		t.Errorf("the pool's gap recording (sent at +17s, while no test was open) is inside a window: %v", ws)
	}
	if !s.InFlightInto(at(15), at(21), at(20)) {
		t.Errorf("TestB's in-flight recording (sent +15s, answered +21s) is not in flight into TestB's window: %v", ws)
	}
	if s.InFlightInto(at(17), at(18), at(20)) {
		t.Errorf("the pool's gap recording, answered before TestB began, reads as in flight into it")
	}
}

// A response's recorded time is when the relay's write of it to the
// application returned, and the application can act on the bytes before
// that: end its test and send its next call, which the other forwarder
// reads and stamps first. keploy mock record charges a call to a test by
// its place (models.PlaceOf), its request time, so the test's window
// derived for the replay ends at its recordings' last place, never at an
// answer stamped after it: else a pool recording made after the test
// ended, but read before that answer's write returned, falls inside the
// test's window, and the learn step reads the replayed call as one made in
// another window than its recording.
//
// The timestamps are a Mongo set's, recorded on a loaded host: the last
// test's delete (last-delete) was answered 2.16 ms after it was sent, and
// the write of that answer to the application returned 270 µs after keploy
// had read the driver's endSessions, sent as its client disconnected after
// the last test. The replay then missed that endSessions, outside every
// test, in both judging rounds, and the record failed with every test
// passing.
func TestAnAnswerStampedAfterTheNextCallDoesNotStretchItsTestsWindow(t *testing.T) {
	at := func(s string) time.Time {
		ts, err := time.Parse(time.RFC3339Nano, s)
		if err != nil {
			t.Fatal(err)
		}
		return ts
	}
	mk := func(name, req, res string) *models.Mock {
		return &models.Mock{Name: name, Spec: models.MockSpec{ReqTimestampMock: at(req), ResTimestampMock: at(res)}}
	}
	decoded := []*models.Mock{
		mk("last-find", "2026-11-02T21:56:15.612330437Z", "2026-11-02T21:56:15.613136419Z"),
		mk("last-delete", "2026-11-02T21:56:15.613347922Z", "2026-11-02T21:56:15.615503334Z"),
		mk("endSessions", "2026-11-02T21:56:15.61523347Z", "2026-11-02T21:56:15.616645021Z"), // the pool's: after the test ended
	}
	begin := at("2026-11-02T21:56:15.611679758Z")
	mp := &models.Mapping{TestCases: []models.MappedTestCase{{ID: "items::TestCrossTestFind", At: begin}}}
	table := map[string][]string{"items::TestCrossTestFind": {"last-find", "last-delete"}}
	ws := recordedTestWindows(mp, table, decoded)
	if want := []models.TestWindow{{TestCase: "items::TestCrossTestFind", Start: begin, End: at("2026-11-02T21:56:15.613347922Z")}}; !reflect.DeepEqual(ws, want) {
		t.Errorf("windows\n %v\nwant\n %v", ws, want)
	}
	if s := models.NewWindowSchedule(ws); s.Contains(decoded[2].Spec.ReqTimestampMock) {
		t.Errorf("the pool's endSessions, sent after the test ended, is inside the test's window %v", ws)
	}
}

// A recording placed at its answer (models.IsAnswerPlaced: a consumer's long
// poll) is charged to the test whose scope holds its answer, so that answer
// is its place in the test's window: the window ends at it, and a test the
// mappings give no begin starts at it, never at its request, sent while the
// test before ran.
func TestAWindowEndsAtARecordingPlacedAtItsAnswer(t *testing.T) {
	t.Cleanup(models.RegisterAnswerPlaced(models.HTTP, func(m *models.Mock) bool { return m.Spec.Metadata["poll"] == "yes" }))
	t0 := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	at := func(s int) time.Time { return t0.Add(time.Duration(s) * time.Second) }
	poll := &models.Mock{Name: "b-poll", Kind: models.HTTP,
		Spec:         models.MockSpec{Metadata: map[string]string{"poll": "yes"}, ReqTimestampMock: at(12), ResTimestampMock: at(24)},
		TestModeInfo: models.TestModeInfo{Lifetime: models.LifetimePerTest, LifetimeDerived: true}}
	if !models.IsAnswerPlaced(poll) {
		t.Fatal("the poll is not placed at its answer")
	}
	decoded := []*models.Mock{windowMock("a-1", t0, 11, 12), windowMock("b-1", t0, 21, 22), poll}
	for _, c := range []struct {
		name string
		mp   *models.Mapping
		want []models.TestWindow
	}{
		{
			name: "begun",
			mp:   &models.Mapping{TestCases: []models.MappedTestCase{{ID: "o::TestA", At: at(10)}, {ID: "o::TestB", At: at(20)}}},
			want: []models.TestWindow{{TestCase: "o::TestA", Start: at(10), End: at(11)}, {TestCase: "o::TestB", Start: at(20), End: at(24)}},
		},
		{
			name: "no begins",
			want: []models.TestWindow{{TestCase: "o::TestA", Start: at(11), End: at(11)}, {TestCase: "o::TestB", Start: at(21), End: at(24)}},
		},
	} {
		table := map[string][]string{"o::TestA": {"a-1"}, "o::TestB": {"b-poll", "b-1"}}
		if got := recordedTestWindows(c.mp, table, decoded); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: windows\n %v\nwant\n %v", c.name, got, c.want)
		}
	}
}

// A mapping can list a recording the set did not decode (a mock file cut
// short, or one with no request time): it has no place, and it moves no
// test's window, wherever it sits in the test's list. Read as a zero place,
// it would start a test with no begin at its next recording, or give it no
// window at all.
func TestARecordingWithNoPlaceMovesNoWindow(t *testing.T) {
	t0 := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	at := func(s int) time.Time { return t0.Add(time.Duration(s) * time.Second) }
	unstamped := &models.Mock{Name: "a-unstamped", Kind: models.HTTP}
	decoded := []*models.Mock{windowMock("a-1", t0, 11, 12), windowMock("a-2", t0, 13, 14), unstamped}
	want := []models.TestWindow{{TestCase: "o::TestA", Start: at(11), End: at(13)}}
	for _, list := range [][]string{
		{"a-1", "a-gone", "a-2"},
		{"a-1", "a-2", "a-gone"},
		{"a-1", "a-unstamped", "a-2"},
		{"a-1", "a-2", "a-unstamped"},
	} {
		table := map[string][]string{"o::TestA": list}
		if got := recordedTestWindows(nil, table, decoded); !reflect.DeepEqual(got, want) {
			t.Errorf("%v: windows\n %v\nwant\n %v", list, got, want)
		}
	}
}

// scopeTableDB is a set's mappings, read both ways.
type scopeTableDB struct {
	MappingDB
	mapping *models.Mapping
}

func (d scopeTableDB) Get(context.Context, string) (map[string][]models.MockEntry, bool, error) {
	out := map[string][]models.MockEntry{}
	for _, tc := range d.mapping.TestCases {
		out[tc.ID] = tc.Mocks
	}
	return out, true, nil
}

func (d scopeTableDB) GetMapping(context.Context, string) (*models.Mapping, error) {
	return d.mapping, nil
}

// scopeTablePusher keeps what the replay pushed (ScopePusher).
type scopeTablePusher struct {
	Instrumentation
	table   map[string][]string
	windows []models.TestWindow
}

func (p *scopeTablePusher) PushScopeTable(_ context.Context, table map[string][]string, windows []models.TestWindow) error {
	p.table, p.windows = table, windows
	return nil
}

// With the proven switch off, keploy mock replay sends the agent every test's
// recorded window with the per-test table, derived from the set's mappings
// as under the switch (recordedTestWindows): the run's place an answer
// recorded in a later window than its call is written at. A recording the
// agent never holds still places its test's window. A test's recorded begin
// is read only under the switch, so each window starts at its test's first
// recording.
func TestScopeTableCarriesTheRecordedWindowsWithTheSwitchOff(t *testing.T) {
	t.Setenv("KEPLOY_PROVEN", "")
	t0 := time.Date(2026, 11, 6, 0, 0, 0, 0, time.UTC)
	at := func(s int) time.Time { return t0.Add(time.Duration(s) * time.Second) }
	decoded := []*models.Mock{windowMock("a-1", t0, 1, 2), windowMock("b-1", t0, 21, 22), windowMock("b-2", t0, 25, 26)}
	p := &scopeTablePusher{}
	m := &mockService{logger: zap.NewNop(), instrumentation: p, mappingDB: scopeTableDB{mapping: &models.Mapping{TestCases: []models.MappedTestCase{
		{ID: "o::TestA", At: at(0), Mocks: []models.MockEntry{{Name: "a-1"}}},
		{ID: "o::TestB", At: at(20), Mocks: []models.MockEntry{{Name: "b-1"}, {Name: "b-2"}}},
	}}}}
	if err := m.pushScopeTable(context.Background(), "rec", decoded[:2], nil, decoded, nil); err != nil {
		t.Fatal(err)
	}
	want := []models.TestWindow{{TestCase: "o::TestA", Start: at(1), End: at(1)}, {TestCase: "o::TestB", Start: at(21), End: at(25)}}
	if !reflect.DeepEqual(p.windows, want) {
		t.Fatalf("windows\n %v\nwant\n %v", p.windows, want)
	}
	if len(p.table) != 2 {
		t.Fatalf("table %v", p.table)
	}
}

// The run's place reads a set's windows so that a test's own answers lie in
// its own window, the last test's too: built from what recordedTestWindows
// gives, under the switch (each test's recorded begin) and with it off (its
// first recording). An answer recorded in a later test lies in that test's.
func TestRunWindowsHoldEachAnswerInTheTestOfItsMoment(t *testing.T) {
	t0 := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	decoded := []*models.Mock{
		windowMock("a-1", t0, 11, 12),
		windowMock("a-2", t0, 14, 15),
		windowMock("a-late", t0, 13, 24),
		windowMock("b-1", t0, 22, 23),
		windowMock("c-1", t0, 32, 33),
		windowMock("c-2", t0, 34, 36),
	}
	table := map[string][]string{"o::TestA": {"a-1", "a-2", "a-late"}, "o::TestB": {"b-1"}, "o::TestC": {"c-1", "c-2"}}
	begun := &models.Mapping{TestCases: []models.MappedTestCase{
		{ID: "o::TestA", At: t0.Add(10 * time.Second)}, {ID: "o::TestB", At: t0.Add(20 * time.Second)}, {ID: "o::TestC", At: t0.Add(30 * time.Second)},
	}}
	for name, mp := range map[string]*models.Mapping{"switch on": begun, "switch off": nil} {
		t.Run(name, func(t *testing.T) {
			s := models.NewWindowSchedule(models.RunWindows(recordedTestWindows(mp, table, decoded)))
			for _, c := range []struct {
				mock string
				test string
			}{
				{"a-1", "o::TestA"}, {"a-2", "o::TestA"}, {"a-late", "o::TestB"}, {"b-1", "o::TestB"}, {"c-1", "o::TestC"}, {"c-2", "o::TestC"},
			} {
				var res time.Time
				for _, m := range decoded {
					if m.Name == c.mock {
						res = m.Spec.ResTimestampMock
					}
				}
				want, ok := s.IndexOfTest(c.test)
				if got := s.IndexOf(res); !ok || got != want {
					t.Errorf("the answer of %s lies in window %d, want %s's, %d", c.mock, got, c.test, want)
				}
			}
		})
	}
}
