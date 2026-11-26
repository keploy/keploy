package models

import (
	"testing"
	"time"
)

func TestWindowScheduleOwnerOf(t *testing.T) {
	t0 := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	at := func(d time.Duration) time.Time { return t0.Add(d) }

	t.Run("empty and zero", func(t *testing.T) {
		var nilSched *WindowSchedule
		if idx, inside, ok := nilSched.OwnerOf(at(10*time.Millisecond), MockWindowEdgeSkew); ok || inside || idx != -1 {
			t.Fatalf("nil schedule: got (%d, %v, %v), want (-1, false, false)", idx, inside, ok)
		}
		empty := NewWindowSchedule(nil)
		if idx, inside, ok := empty.OwnerOf(at(10*time.Millisecond), MockWindowEdgeSkew); ok || inside || idx != -1 {
			t.Fatalf("empty schedule: got (%d, %v, %v), want (-1, false, false)", idx, inside, ok)
		}
		one := NewWindowSchedule([]TestWindow{{TestCase: "t1", Start: at(0), End: at(10 * time.Millisecond)}})
		if idx, inside, ok := one.OwnerOf(time.Time{}, MockWindowEdgeSkew); ok || inside || idx != -1 {
			t.Fatalf("zero time: got (%d, %v, %v), want (-1, false, false)", idx, inside, ok)
		}
	})

	t.Run("nested smallest containing window owns", func(t *testing.T) {
		s := NewWindowSchedule([]TestWindow{
			{TestCase: "outer", Start: at(0), End: at(100 * time.Millisecond)},
			{TestCase: "mid", Start: at(5 * time.Millisecond), End: at(50 * time.Millisecond)},
			{TestCase: "inner", Start: at(10 * time.Millisecond), End: at(20 * time.Millisecond)},
		})
		for _, c := range []struct {
			d    time.Duration
			want string
		}{
			{0, "outer"},
			{4 * time.Millisecond, "outer"},
			{5 * time.Millisecond, "mid"},
			{9 * time.Millisecond, "mid"},
			{10 * time.Millisecond, "inner"},
			{12 * time.Millisecond, "inner"},
			{15 * time.Millisecond, "inner"},
			{20 * time.Millisecond, "inner"},
			{25 * time.Millisecond, "mid"},
			{50 * time.Millisecond, "mid"},
			{60 * time.Millisecond, "outer"},
			{100 * time.Millisecond, "outer"},
		} {
			idx, inside, ok := s.OwnerOf(at(c.d), MockWindowEdgeSkew)
			if !ok || !inside {
				t.Fatalf("at %v: got ok=%v inside=%v, want true/true", c.d, ok, inside)
			}
			if got := s.Window(idx).TestCase; got != c.want {
				t.Errorf("at %v: owner=%q, want %q", c.d, got, c.want)
			}
		}
	})

	t.Run("nested sharing start or end edge", func(t *testing.T) {
		sameStart := NewWindowSchedule([]TestWindow{
			{TestCase: "outer", Start: at(0), End: at(30 * time.Millisecond)},
			{TestCase: "inner", Start: at(0), End: at(20 * time.Millisecond)},
		})
		idx, inside, ok := sameStart.OwnerOf(at(15*time.Millisecond), MockWindowEdgeSkew)
		if !ok || !inside || sameStart.Window(idx).TestCase != "inner" {
			t.Fatalf("same-start nested at 15ms: got (%v, %v, %v), want inner", idx, inside, ok)
		}
		sameEnd := NewWindowSchedule([]TestWindow{
			{TestCase: "outer", Start: at(0), End: at(30 * time.Millisecond)},
			{TestCase: "inner", Start: at(10 * time.Millisecond), End: at(30 * time.Millisecond)},
		})
		idx, inside, ok = sameEnd.OwnerOf(at(15*time.Millisecond), MockWindowEdgeSkew)
		if !ok || !inside || sameEnd.Window(idx).TestCase != "inner" {
			t.Fatalf("same-end nested at 15ms: got (%v, %v, %v), want inner", idx, inside, ok)
		}
	})

	t.Run("partly overlapping non-nested ranks by closest start then stable id", func(t *testing.T) {
		// B is wider than A, yet neither encloses the other: closest request
		// start (15-10=5ms < 15-0=15ms) picks B.
		s := NewWindowSchedule([]TestWindow{
			{TestCase: "a", Start: at(0), End: at(20 * time.Millisecond)},
			{TestCase: "b", Start: at(10 * time.Millisecond), End: at(80 * time.Millisecond)},
		})
		idx, inside, ok := s.OwnerOf(at(15*time.Millisecond), MockWindowEdgeSkew)
		if !ok || !inside || s.Window(idx).TestCase != "b" {
			t.Fatalf("partly overlapping at 15ms: got (%v, %v, %v), want b", idx, inside, ok)
		}
		// Identical windows tie-break deterministically by TestCase ID regardless of input order.
		for _, ws := range [][]TestWindow{
			{
				{TestCase: "tc-a", Start: at(10 * time.Millisecond), End: at(20 * time.Millisecond)},
				{TestCase: "tc-b", Start: at(10 * time.Millisecond), End: at(20 * time.Millisecond)},
			},
			{
				{TestCase: "tc-b", Start: at(10 * time.Millisecond), End: at(20 * time.Millisecond)},
				{TestCase: "tc-a", Start: at(10 * time.Millisecond), End: at(20 * time.Millisecond)},
			},
		} {
			eq := NewWindowSchedule(ws)
			idx, inside, ok := eq.OwnerOf(at(15*time.Millisecond), MockWindowEdgeSkew)
			if !ok || !inside || eq.Window(idx).TestCase != "tc-a" {
				t.Fatalf("identical windows: got (%v, %v, %v), want tc-a", idx, inside, ok)
			}
		}
	})

	t.Run("edge skew bounded to nearest edge", func(t *testing.T) {
		s := NewWindowSchedule([]TestWindow{
			{TestCase: "t1", Start: at(10 * time.Millisecond), End: at(20 * time.Millisecond)},
			{TestCase: "t2", Start: at(20*time.Millisecond + 100*time.Microsecond), End: at(30 * time.Millisecond)},
			{TestCase: "t3", Start: at(50 * time.Millisecond), End: at(60 * time.Millisecond)},
		})
		// 30us past t1's end (nearer to t1 than t2's start at +100us).
		idx, inside, ok := s.OwnerOf(at(20*time.Millisecond+30*time.Microsecond), MockWindowEdgeSkew)
		if !ok || inside || s.Window(idx).TestCase != "t1" {
			t.Fatalf("30us past t1: got (%v, inside=%v, ok=%v), want t1 edge", idx, inside, ok)
		}
		// 20us before t2's start (80us past t1's end): nearer to t2.
		idx, inside, ok = s.OwnerOf(at(20*time.Millisecond+80*time.Microsecond), MockWindowEdgeSkew)
		if !ok || inside || s.Window(idx).TestCase != "t2" {
			t.Fatalf("20us before t2: got (%v, inside=%v, ok=%v), want t2 edge", idx, inside, ok)
		}
		// 30us before t1's start.
		idx, inside, ok = s.OwnerOf(at(10*time.Millisecond-30*time.Microsecond), MockWindowEdgeSkew)
		if !ok || inside || s.Window(idx).TestCase != "t1" {
			t.Fatalf("30us before t1: got (%v, inside=%v, ok=%v), want t1 edge", idx, inside, ok)
		}
		// Exact maxSkew boundary is owned; maxSkew+1ns is not.
		idx, inside, ok = s.OwnerOf(at(30*time.Millisecond+MockWindowEdgeSkew), MockWindowEdgeSkew)
		if !ok || inside || s.Window(idx).TestCase != "t2" {
			t.Fatalf("exact maxSkew past t2: got (%v, inside=%v, ok=%v), want t2 edge", idx, inside, ok)
		}
		if idx, inside, ok := s.OwnerOf(at(30*time.Millisecond+MockWindowEdgeSkew+time.Nanosecond), MockWindowEdgeSkew); ok || inside || idx != -1 {
			t.Fatalf("maxSkew+1ns past t2: got (%d, %v, %v), want unowned", idx, inside, ok)
		}
		// Midpoint of large gap (40ms: 10ms from t2 and 10ms from t3) is unowned.
		if idx, inside, ok := s.OwnerOf(at(40*time.Millisecond), MockWindowEdgeSkew); ok || inside || idx != -1 {
			t.Fatalf("large gap 40ms: got (%d, %v, %v), want unowned", idx, inside, ok)
		}
		// maxSkew <= 0 disables edge skew.
		if idx, inside, ok := s.OwnerOf(at(20*time.Millisecond+30*time.Microsecond), 0); ok || inside || idx != -1 {
			t.Fatalf("maxSkew=0: got (%d, %v, %v), want unowned", idx, inside, ok)
		}
		// Equidistant edge tie-break is deterministic (closer Start wins: t2's Start is 50us away vs t1's Start 10.05ms away).
		idx, inside, ok = s.OwnerOf(at(20*time.Millisecond+50*time.Microsecond), MockWindowEdgeSkew)
		if !ok || inside || s.Window(idx).TestCase != "t2" {
			t.Fatalf("equidistant gap 20.05ms: got (%v, inside=%v, ok=%v), want t2", idx, inside, ok)
		}
	})

	t.Run("zero response timestamp keeps its place", func(t *testing.T) {
		// X starts at 0 with no response timestamp (clamped to End=Start=0);
		// Y runs [500ms, 510ms]. X must keep index 0 and must not stretch into Y.
		s := NewWindowSchedule([]TestWindow{
			{TestCase: "x-no-resp", Start: at(0)},
			{TestCase: "y-after", Start: at(500 * time.Millisecond), End: at(510 * time.Millisecond)},
		})
		if got := s.Window(0).TestCase; got != "x-no-resp" {
			t.Fatalf("window 0 = %q, want x-no-resp", got)
		}
		if !s.Window(0).End.Equal(at(0)) {
			t.Fatalf("x-no-resp End = %v, want clamped to Start %v", s.Window(0).End, at(0))
		}
		idx, inside, ok := s.OwnerOf(at(0), MockWindowEdgeSkew)
		if !ok || !inside || s.Window(idx).TestCase != "x-no-resp" {
			t.Fatalf("at 0: got (%v, %v, %v), want x-no-resp", idx, inside, ok)
		}
		idx, inside, ok = s.OwnerOf(at(505*time.Millisecond), MockWindowEdgeSkew)
		if !ok || !inside || s.Window(idx).TestCase != "y-after" {
			t.Fatalf("at 505ms: got (%v, %v, %v), want y-after", idx, inside, ok)
		}
		if idx, inside, ok := s.OwnerOf(at(250*time.Millisecond), MockWindowEdgeSkew); ok || inside || idx != -1 {
			t.Fatalf("at 250ms: got (%d, %v, %v), want unowned (x must not expand to Start+1s)", idx, inside, ok)
		}
	})
}

func TestWindowScheduleOwnerOfSpan(t *testing.T) {
	t0 := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	at := func(d time.Duration) time.Time { return t0.Add(d) }

	s := NewWindowSchedule([]TestWindow{
		{TestCase: "outer", Start: at(0), End: at(100 * time.Millisecond)},
		{TestCase: "u1", Start: at(8 * time.Millisecond), End: at(15 * time.Millisecond)},
		{TestCase: "inner", Start: at(10 * time.Millisecond), End: at(20 * time.Millisecond)},
		{TestCase: "u2", Start: at(80 * time.Millisecond), End: at(90 * time.Millisecond)},
	})

	// Outer's transaction span [12ms, 85ms] crosses u1 and u2, so neither short
	// window contains it; only outer does.
	idx, ok := s.OwnerOfSpan(at(12*time.Millisecond), at(85*time.Millisecond))
	if !ok || s.Window(idx).TestCase != "outer" {
		t.Fatalf("span [12ms,85ms]: got (%d, %v), want outer", idx, ok)
	}
	// Inner's transaction span [11ms, 19ms] fits inside inner (not u1).
	idx, ok = s.OwnerOfSpan(at(11*time.Millisecond), at(19*time.Millisecond))
	if !ok || s.Window(idx).TestCase != "inner" {
		t.Fatalf("span [11ms,19ms]: got (%d, %v), want inner", idx, ok)
	}
	// Span extending past every window has no owner.
	if idx, ok := s.OwnerOfSpan(at(50*time.Millisecond), at(120*time.Millisecond)); ok || idx != -1 {
		t.Fatalf("span [50ms,120ms]: got (%d, %v), want (-1, false)", idx, ok)
	}
	// Zero end or end < start falls back to start.
	idx, ok = s.OwnerOfSpan(at(12*time.Millisecond), time.Time{})
	if !ok || s.Window(idx).TestCase != "inner" {
		t.Fatalf("zero end at 12ms: got (%d, %v), want inner", idx, ok)
	}
	idx, ok = s.OwnerOfSpan(at(12*time.Millisecond), at(5*time.Millisecond))
	if !ok || s.Window(idx).TestCase != "inner" {
		t.Fatalf("end < start at 12ms: got (%d, %v), want inner", idx, ok)
	}
	// Empty/zero guard.
	var nilSched *WindowSchedule
	if idx, ok := nilSched.OwnerOfSpan(at(10*time.Millisecond), at(20*time.Millisecond)); ok || idx != -1 {
		t.Fatalf("nil schedule: got (%d, %v), want (-1, false)", idx, ok)
	}
	if idx, ok := s.OwnerOfSpan(time.Time{}, at(20*time.Millisecond)); ok || idx != -1 {
		t.Fatalf("zero start: got (%d, %v), want (-1, false)", idx, ok)
	}
}

// edgeCarryKind is a kind TestWindowScheduleEdgeOwner registers carry-over.
const edgeCarryKind Kind = "TestEdgeOwnerCarry"

// The edge rule (EdgeOwner) gives a window only a plain per-test timed mock
// placed outside every window within MockWindowEdgeSkew of its edge, and one
// no other rule places: not one placed inside a window (the smallest holding
// it owns it), before the first window (startup traffic), or in flight into
// the next window (the in-flight rule gives it to that window), and not a
// mock of another shape: placed at its answer, carry-over, of a carry-over
// kind, window-bound, staged with the set's startup, or not per-test.
// EdgeOwnedBy asks it of one window.
func TestWindowScheduleEdgeOwner(t *testing.T) {
	answerPlacedPoll(t)
	t.Cleanup(RegisterStagingNeed(HTTP, StagingNeed{Use: StagingWithStartup, Match: func(m *Mock) bool { return m.Spec.Metadata["push"] == "yes" }}))
	us := func(ms int, d int) time.Time { return wmAt(ms).Add(time.Duration(d) * time.Microsecond) }
	s := NewWindowSchedule([]TestWindow{
		{TestCase: "A", Start: wmAt(1000), End: wmAt(1100)},
		{TestCase: "B", Start: wmAt(1200), End: wmAt(1300)},
	})
	mk := func(name string, req, res time.Time, meta ...string) *Mock {
		m := placeMock(name, 0, 0, false)
		m.Spec.ReqTimestampMock, m.Spec.ResTimestampMock = req, res
		for i := 0; i+1 < len(meta); i += 2 {
			m.Spec.Metadata[meta[i]] = meta[i+1]
		}
		return m
	}
	session := mk("session", us(1100, 30), us(1100, 40))
	session.TestModeInfo.Lifetime = LifetimeSession
	refused := mk("refused", us(1100, 30), us(1100, 40))
	refused.Kind = ConnectionFailure
	// A mock of a carry-over kind its predicate rejects: its consume outside
	// its window is still flagged (MarksOutOfWindow), and the replay maps it
	// by the release rule.
	t.Cleanup(RegisterCarryOver(edgeCarryKind, func(m *Mock) bool { return m.Spec.Metadata["send"] == "yes" }))
	carryKind := mk("carry kind, not carry-over", us(1100, 30), us(1100, 40))
	carryKind.Kind = edgeCarryKind
	if IsCarryOver(carryKind) || !MarksOutOfWindow(carryKind) {
		t.Fatal("precondition: the mock is of a carry-over kind its predicate rejects")
	}
	carried := mk("carry-over", us(1200, -30), us(1200, -20), "send", "yes")
	carried.Kind = edgeCarryKind
	if !IsCarryOver(carried) {
		t.Fatal("precondition: the carry-over mock is carry-over")
	}
	for _, c := range []struct {
		m    *Mock
		want string
	}{
		{mk("past A's end", us(1100, 30), us(1100, 40)), "A"},
		{mk("before B's start", us(1200, -30), us(1200, -20)), "B"},
		{mk("beyond the skew", us(1100, 150), us(1100, 160)), ""},
		{mk("inside A", wmAt(1050), wmAt(1060)), ""},
		{mk("before the first window", us(1000, -30), us(1000, -20)), ""},
		{mk("in flight into B", us(1100, 30), wmAt(1250)), ""},
		{mk("placed at its answer in B", us(1100, 30), wmAt(1250), "poll", "yes"), ""},
		{mk("placed at its answer past A", us(1100, 20), us(1100, 30), "poll", "yes"), ""},
		{carried, ""},
		{carryKind, ""},
		{mk("staged with the startup", us(1100, 30), us(1100, 40), "push", "yes"), ""},
		{refused, ""},
		{session, ""},
	} {
		idx, ok := s.EdgeOwner(c.m)
		got := ""
		if ok {
			got = s.Window(idx).TestCase
		}
		if got != c.want {
			t.Errorf("%s: the edge rule gives it to %q; want %q", c.m.Name, got, c.want)
		}
	}
	var none *WindowSchedule
	if _, ok := none.EdgeOwner(mk("no windows", us(1100, 30), us(1100, 40))); ok {
		t.Error("no schedule: the edge rule gave a mock to a window")
	}
	// The call in flight into B is the one the edge rule leaves to the
	// in-flight rule, from A; a plain edge mock is given; one beyond the skew
	// is neither.
	for _, c := range []struct {
		m    *Mock
		want EdgeOutcome
		win  string
	}{
		{mk("in flight", us(1100, 30), wmAt(1250)), EdgeLeftInFlight, "A"},
		{mk("past A's end", us(1100, 30), us(1100, 40)), EdgeOwned, "A"},
		{mk("beyond", us(1100, 150), us(1100, 160)), EdgeNone, ""},
	} {
		idx, got := s.EdgeDecision(c.m)
		win := ""
		if idx >= 0 {
			win = s.Window(idx).TestCase
		}
		if got != c.want || win != c.win {
			t.Errorf("%s: EdgeDecision = (%q, %d); want (%q, %d)", c.m.Name, win, got, c.win, c.want)
		}
	}
	// EdgeOwnedBy, of one window: the edge mock past A's end is A's and not
	// B's, the one before B's start B's and not A's; the call in flight into
	// B, a mock inside A and one beyond the skew are neither's.
	a, b := s.Window(0), s.Window(1)
	for _, c := range []struct {
		m   *Mock
		ofA bool
		ofB bool
	}{
		{mk("past A's end", us(1100, 30), us(1100, 40)), true, false},
		{mk("100 us past A's end", us(1100, 100), us(1100, 110)), true, false},
		{mk("before B's start", us(1200, -30), us(1200, -20)), false, true},
		{mk("in flight into B", us(1100, 30), wmAt(1250)), false, false},
		{mk("inside A", wmAt(1050), wmAt(1060)), false, false},
		{mk("beyond the skew", us(1100, 150), us(1100, 160)), false, false},
	} {
		if got := s.EdgeOwnedBy(c.m, a.Start, a.End); got != c.ofA {
			t.Errorf("%s: EdgeOwnedBy(A) = %v; want %v", c.m.Name, got, c.ofA)
		}
		if got := s.EdgeOwnedBy(c.m, b.Start, b.End); got != c.ofB {
			t.Errorf("%s: EdgeOwnedBy(B) = %v; want %v", c.m.Name, got, c.ofB)
		}
	}
	if none.EdgeOwnedBy(mk("no windows", us(1100, 30), us(1100, 40)), a.Start, a.End) {
		t.Error("no schedule: EdgeOwnedBy gave a mock to a window")
	}
	// Two windows 150 us apart: a mock 60 us past the first one's end is
	// within 100 us of both edges, and the nearer, the first, owns it.
	near := NewWindowSchedule([]TestWindow{
		{TestCase: "C", Start: wmAt(2000), End: wmAt(2100)},
		{TestCase: "D", Start: us(2100, 150), End: wmAt(2200)},
	})
	between := mk("between C and D", us(2100, 60), us(2100, 70))
	if c, d := near.Window(0), near.Window(1); !near.EdgeOwnedBy(between, c.Start, c.End) || near.EdgeOwnedBy(between, d.Start, d.End) {
		t.Errorf("a mock 60 us past C's end and 90 us before D's start: EdgeOwnedBy(C) = %v, EdgeOwnedBy(D) = %v; want true, false",
			near.EdgeOwnedBy(between, c.Start, c.End), near.EdgeOwnedBy(between, d.Start, d.End))
	}
}
