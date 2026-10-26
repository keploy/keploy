package mock

import (
	"slices"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

func windowMock(name string, t0 time.Time, req, res int) *models.Mock {
	return &models.Mock{Name: name, Spec: models.MockSpec{
		ReqTimestampMock: t0.Add(time.Duration(req) * time.Second),
		ResTimestampMock: t0.Add(time.Duration(res) * time.Second),
	}}
}

// keploy mock replay sends the agent the recorded window of every test of the
// set, from its mappings: from the test's recorded begin (earlier where a
// recording it lists was sent before it) to the last request or answer of
// the recordings it lists. A test that lists none spans its begin alone, a
// test with no begin and no timed recording has none, and windows that
// overlap are one window no test names.
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
				{TestCase: "o::TestA", Start: at(10), End: at(15)},
				{TestCase: "o::TestB", Start: at(19), End: at(23)},
			},
		},
		{
			name:  "a test listing no recording spans its begin; one with no begin and nothing timed has none",
			mp:    &models.Mapping{TestCases: []models.MappedTestCase{{ID: "o::TestA", At: at(10)}, {ID: "o::TestC", At: at(30)}, {ID: "o::TestD"}}},
			table: map[string][]string{"o::TestA": {"a-1", "a-2"}, "o::TestC": {}, "o::TestD": {"not-decoded"}},
			want: []models.TestWindow{
				{TestCase: "o::TestA", Start: at(10), End: at(15)},
				{TestCase: "o::TestC", Start: at(30), End: at(30)},
			},
		},
		{
			name:  "a recording the mapping lists of a test the mappings give no begin",
			mp:    nil,
			table: map[string][]string{"o::TestB": {"b-1"}},
			want:  []models.TestWindow{{TestCase: "o::TestB", Start: at(22), End: at(23)}},
		},
		{
			name:  "overlapping windows are one, named by no test",
			mp:    &models.Mapping{TestCases: []models.MappedTestCase{{ID: "o::TestA", At: at(10)}, {ID: "o::TestB", At: at(13)}, {ID: "o::TestC", At: at(30)}}},
			table: map[string][]string{"o::TestA": {"a-1", "a-2"}, "o::TestB": {"b-1"}, "o::TestC": {}},
			want: []models.TestWindow{
				{Start: at(10), End: at(23)},
				{TestCase: "o::TestC", Start: at(30), End: at(30)},
			},
		},
	} {
		if got := recordedTestWindows(c.mp, c.table, decoded); !slices.Equal(got, c.want) {
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
