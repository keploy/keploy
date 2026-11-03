package models

import (
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestSetAsideReasonKey(t *testing.T) {
	for _, c := range []struct{ reason, want string }{
		{"", NoSkipReason},
		{"  \n ", NoSkipReason},
		{"keploy/unmocked_call: Pulsar SEND to 127.0.0.1:6650 (body_mismatch) had no recorded answer", "keploy/unmocked_call"},
		{"keploy/unmocked_call", "keploy/unmocked_call"},
		{"flaky/flipped: body.settled differ", "flaky/flipped"},
		{"keploy/unmocked_call\nhad no answer", "keploy/unmocked_call"},
		{"manual: waits on\nPAY-12", "manual"},
		{"Manual: waits", "Manual"},
		{"no mapping entry", "no mapping entry"},
		{": starts with a colon", ": starts with a colon"},
		{"a/B code: x", "a/B code"},
		{"manual: see keploy/unmocked_call", "manual"},
	} {
		if got := SetAsideReasonKey(c.reason); got != c.want {
			t.Errorf("SetAsideReasonKey(%q) = %q, want %q", c.reason, got, c.want)
		}
	}
}

// A set's share counts each IGNORED row under its reason, and ignored tests
// with no row under NoSkipReason; a set that set none aside has none.
func TestComparedShareOf(t *testing.T) {
	unmocked := "keploy/unmocked_call: Pulsar SEND had no recorded answer"
	rep := &TestReport{
		Success: 2, Failure: 1, Obsolete: 1, Ignored: 6, Total: 11,
		Tests: []TestResult{
			{TestCaseID: "a", Status: TestStatusIgnored, SkipReason: unmocked},
			{TestCaseID: "b", Status: TestStatusIgnored, SkipReason: "keploy/unmocked_call: Redis GET had no recorded answer"},
			{TestCaseID: "c", Status: TestStatusIgnored, SkipReason: "manual: flaky upstream"},
			{TestCaseID: "d", Status: TestStatusIgnored},
			{TestCaseID: "e", Status: TestStatusPassed, SkipReason: unmocked},
			{TestCaseID: "f", Status: TestStatusFailed},
		},
	}
	want := &ComparedShare{Compared: 4, Total: 11, SetAside: 6, Reasons: []SetAsideReason{
		{Reason: NoSkipReason, Tests: 3}, // d, and two ignored with no row
		{Reason: "keploy/unmocked_call", Tests: 2},
		{Reason: "manual", Tests: 1},
	}}
	if got := ComparedShareOf(rep); !reflect.DeepEqual(got, want) {
		t.Fatalf("ComparedShareOf = %+v, want %+v", got, want)
	}
	if got := ComparedShareOf(&TestReport{Success: 3, Total: 3}); got != nil {
		t.Fatalf("a set that set none aside: %+v, want nil", got)
	}
	if got := ComparedShareOf(nil); got != nil {
		t.Fatalf("no report: %+v, want nil", got)
	}
}

// Reasons are sorted most tests first, then by name; counts of none are
// dropped, and tests no reason counts fall under NoSkipReason.
func TestNewComparedShare(t *testing.T) {
	got := NewComparedShare(1, 9, 8, map[string]int{"b": 3, "a": 3, "zero": 0, "neg": -2, "c": 1})
	want := &ComparedShare{Compared: 1, Total: 9, SetAside: 8, Reasons: []SetAsideReason{
		{Reason: "a", Tests: 3}, {Reason: "b", Tests: 3}, {Reason: "c", Tests: 1}, {Reason: NoSkipReason, Tests: 1},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("NewComparedShare = %+v, want %+v", got, want)
	}
	if got := NewComparedShare(3, 3, 0, map[string]int{"a": 1}); got != nil {
		t.Fatalf("none set aside: %+v, want nil", got)
	}
	exact := NewComparedShare(0, 2, 2, map[string]int{"a": 2})
	if len(exact.Reasons) != 1 || exact.Reasons[0] != (SetAsideReason{Reason: "a", Tests: 2}) {
		t.Fatalf("every test counted: %+v, want only a", exact.Reasons)
	}
}

func TestComparedShareLine(t *testing.T) {
	for _, c := range []struct {
		name  string
		share *ComparedShare
		want  string
	}{
		{"nil", nil, ""},
		{"one reason", NewComparedShare(6, 307, 301, map[string]int{"keploy/unmocked_call": 301}),
			"6 of 307 compared; 301 set aside: 301 keploy/unmocked_call"},
		{"one other reason", NewComparedShare(3, 307, 304, map[string]int{"keploy/unmocked_call": 303, "flaky/flipped": 1}),
			"3 of 307 compared; 304 set aside: 303 keploy/unmocked_call, 1 for 1 other reason"},
		{"other reasons", NewComparedShare(1, 9, 8, map[string]int{"a": 5, "b": 2, "c": 1}),
			"1 of 9 compared; 8 set aside: 5 a, 3 for 2 other reasons"},
		{"not run", NewComparedShare(2, 10, 5, map[string]int{"a": 5}),
			"2 of 10 compared; 5 set aside: 5 a; 3 not run"},
		{"capped reads as whole", NewComparedShare(0, 8, 8, map[string]int{"a": 2, "b": 1, "c": 1, "d": 1, "e": 1, "f": 1, "g": 1}).Capped(),
			"0 of 8 compared; 8 set aside: 2 a, 6 for 6 other reasons"},
		{"no reasons listed", &ComparedShare{Compared: 1, Total: 2, SetAside: 1},
			"1 of 2 compared; 1 set aside"},
	} {
		if got := c.share.Line(); got != c.want {
			t.Errorf("%s: Line() = %q, want %q", c.name, got, c.want)
		}
	}
}

// A report's share keeps MaxShareReasons reasons, each at most
// MaxShareReasonBytes, and counts the rest; the share it is cut from keeps
// all of its own.
func TestComparedShareCapped(t *testing.T) {
	if (*ComparedShare)(nil).Capped() != nil {
		t.Fatal("nil.Capped() is not nil")
	}
	long := strings.Repeat("é", MaxShareReasonBytes) // two bytes each
	counts := map[string]int{long: 9, "b": 5, "c": 4, "d": 3, "e": 2, "f": 1, "g": 1}
	whole := NewComparedShare(0, 25, 25, counts)
	c := whole.Capped()
	if len(c.Reasons) != MaxShareReasons || c.OtherReasons != 2 || c.OtherTests != 2 {
		t.Fatalf("capped: %d reasons, %d other reasons, %d other tests; want %d, 2, 2", len(c.Reasons), c.OtherReasons, c.OtherTests, MaxShareReasons)
	}
	// Cut at the last rune boundary that leaves room for "...".
	if top, want := c.Reasons[0].Reason, long[:MaxShareReasonBytes-4]+"..."; top != want || !utf8.ValidString(top) {
		t.Fatalf("the long reason was cut to %d bytes %q; want %q", len(top), top, want)
	}
	if len(whole.Reasons) != 7 || whole.Reasons[0].Reason != long || whole.OtherReasons != 0 {
		t.Fatalf("Capped changed the share it was cut from: %+v", whole)
	}
	five := NewComparedShare(0, 5, 5, map[string]int{"a": 1, "b": 1, "c": 1, "d": 1, "e": 1}).Capped()
	if len(five.Reasons) != 5 || five.OtherReasons != 0 || five.OtherTests != 0 {
		t.Fatalf("five reasons capped: %+v, want all five kept", five)
	}
	// One reason over the cap: MaxShareReasons kept, the one left counted.
	six := NewComparedShare(0, 6, 6, map[string]int{"a": 1, "b": 1, "c": 1, "d": 1, "e": 1, "f": 1}).Capped()
	if len(six.Reasons) != MaxShareReasons || six.OtherReasons != 1 || six.OtherTests != 1 || six.Reasons[MaxShareReasons-1].Reason != "e" {
		t.Fatalf("six reasons capped: %+v, want a to e kept and f counted", six)
	}
	if got := cutUTF8("abc", 3); got != "abc" {
		t.Fatalf("cutUTF8 of a fitting string = %q", got)
	}
	if got := cutUTF8("abcd", 3); got != "..." {
		t.Fatalf("cutUTF8(abcd, 3) = %q, want ...", got)
	}
	// A reason that is not UTF-8 is cut without reading before its start.
	if got := cutUTF8("\x80\x80\x80\x80\x80", 4); got != "..." {
		t.Fatalf("cutUTF8 of continuation bytes = %q, want ...", got)
	}
}

// ReasonCounts gives each listed reason's count.
func TestComparedShareReasonCounts(t *testing.T) {
	if (*ComparedShare)(nil).ReasonCounts() != nil {
		t.Fatal("nil share has reason counts")
	}
	got := NewComparedShare(0, 4, 4, map[string]int{"a": 3, "b": 1}).ReasonCounts()
	if !reflect.DeepEqual(got, map[string]int{"a": 3, "b": 1}) {
		t.Fatalf("ReasonCounts = %v", got)
	}
}
