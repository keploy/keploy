package models

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

// ComparedShare is how much of a test set, or of a run, keploy compared, and
// why it set the rest aside. A PASSED set compared only its tests that got a
// verdict (passed, failed or obsolete): a run that skipped most of a set (its
// tests quarantined at record, its ignore list) read as green with a small
// share of it compared. A set's report carries it when its run set any test
// aside (TestReport.SetAside), and the run's summary, keploy report and JUnit
// each give its line (Line).
type ComparedShare struct {
	// Compared is how many tests got a verdict: passed, failed or obsolete.
	Compared int `json:"compared" yaml:"compared"`
	// Total is how many tests the set, or the run, has.
	Total int `json:"total" yaml:"total"`
	// SetAside is how many the run skipped (IGNORED), with or without a reason.
	SetAside int `json:"setAside" yaml:"set_aside"`
	// Reasons are why, most tests first: one entry for each reason, by its
	// code (SetAsideReasonKey), with how many tests it set aside. A skip
	// with no reason given is counted under NoSkipReason. A report keeps
	// only the first MaxShareReasons (Capped); the rest are counted in
	// OtherReasons and OtherTests.
	Reasons []SetAsideReason `json:"reasons,omitempty" yaml:"reasons,omitempty"`
	// OtherReasons is how many reasons a capped share left out of Reasons,
	// and OtherTests how many tests they set aside.
	OtherReasons int `json:"otherReasons,omitempty" yaml:"other_reasons,omitempty"`
	OtherTests   int `json:"otherTests,omitempty" yaml:"other_tests,omitempty"`
}

// SetAsideReason is one reason a run set tests aside for, and how many.
type SetAsideReason struct {
	Reason string `json:"reason" yaml:"reason"`
	Tests  int    `json:"tests" yaml:"tests"`
}

// NoSkipReason is the reason entry of tests a run skipped with no reason
// given: a hook that set them IGNORED, or a list its caller gave no reason
// for.
const NoSkipReason = "no reason given"

// IgnoredByConfigReason is the reason of a test the run skips because
// keploy.yml's test.ignoredTests names it, or ignores its whole set: the
// user's own choice, said as such.
const IgnoredByConfigReason = "ignored by keploy.yml"

// KeploySetAside reports whether reason, an IGNORED test's, is one keploy
// gave (a quarantine, a test with no mapping entry): any reason but none and
// IgnoredByConfigReason.
func KeploySetAside(reason string) bool {
	return reason != "" && reason != IgnoredByConfigReason
}

// MaxShareReasons and MaxShareReasonBytes bound the share a report carries
// (Capped): it sits in the report's head, before its tests, where a reader
// of the totals reads a bounded head only, and a set's reasons can be as
// many as its tests, each as long as its skip reason.
const (
	MaxShareReasons     = 5
	MaxShareReasonBytes = 120
)

// skipReasonCode is a quarantine's reason as keploy writes it: its class and
// its code ("keploy/unmocked_call"), maybe followed by why.
var skipReasonCode = regexp.MustCompile(`^[a-z][a-z-]*/[a-z][a-z0-9_]*`)

// SetAsideReasonKey is the entry a skip reason is counted under: a
// quarantine's class and code ("keploy/unmocked_call"), the text before the
// first ": " of any other reason ("manual" for "manual: flaky upstream"), and
// NoSkipReason for none. Tests skipped for the same cause count together,
// however their reasons go on to say which call or test.
func SetAsideReasonKey(reason string) string {
	r := SkipReasonOneLine(reason)
	if r == "" {
		return NoSkipReason
	}
	if code := skipReasonCode.FindString(r); code != "" {
		return code
	}
	if i := strings.Index(r, ": "); i > 0 {
		return r[:i]
	}
	return r
}

// ComparedShareOf is a set's share, from its report: nil when the run set
// no test of it aside, so a set that compared all it ran says nothing more.
// Each IGNORED row counts under its reason; ignored tests with no row (a set
// skipped whole) count under NoSkipReason.
func ComparedShareOf(r *TestReport) *ComparedShare {
	if r == nil {
		return nil
	}
	counts := map[string]int{}
	for _, t := range r.Tests {
		if t.Status == TestStatusIgnored {
			counts[SetAsideReasonKey(t.SkipReason)]++
		}
	}
	return NewComparedShare(r.Success+r.Failure+r.Obsolete, r.Total, r.Ignored, counts)
}

// NewComparedShare is the share of compared tests of total, setAside of them
// skipped, for the reasons counted in counts (by SetAsideReasonKey): nil when
// none was set aside. Set-aside tests no reason counts are counted under
// NoSkipReason.
func NewComparedShare(compared, total, setAside int, counts map[string]int) *ComparedShare {
	if setAside <= 0 {
		return nil
	}
	merged := make(map[string]int, len(counts)+1)
	counted := 0
	for r, n := range counts {
		if n > 0 {
			merged[r] += n
			counted += n
		}
	}
	if counted < setAside {
		merged[NoSkipReason] += setAside - counted
	}
	out := make([]SetAsideReason, 0, len(merged))
	for r, n := range merged {
		out = append(out, SetAsideReason{Reason: r, Tests: n})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Tests != out[j].Tests {
			return out[i].Tests > out[j].Tests
		}
		return out[i].Reason < out[j].Reason
	})
	return &ComparedShare{Compared: compared, Total: total, SetAside: setAside, Reasons: out}
}

// ReasonCounts is how many tests each reason of the share set aside, its
// left-out reasons not included.
func (s *ComparedShare) ReasonCounts() map[string]int {
	if s == nil {
		return nil
	}
	out := make(map[string]int, len(s.Reasons))
	for _, r := range s.Reasons {
		out[r.Reason] += r.Tests
	}
	return out
}

// Capped is the share as a report carries it: its first MaxShareReasons
// reasons, each cut to MaxShareReasonBytes, and the rest counted in
// OtherReasons and OtherTests. Its Line reads as the whole share's.
func (s *ComparedShare) Capped() *ComparedShare {
	if s == nil {
		return nil
	}
	c := *s
	keep := s.Reasons
	if len(keep) > MaxShareReasons {
		for _, r := range keep[MaxShareReasons:] {
			c.OtherReasons++
			c.OtherTests += r.Tests
		}
		keep = keep[:MaxShareReasons]
	}
	c.Reasons = make([]SetAsideReason, len(keep))
	for i, r := range keep {
		c.Reasons[i] = SetAsideReason{Reason: cutUTF8(r.Reason, MaxShareReasonBytes), Tests: r.Tests}
	}
	return &c
}

// Line is the share on one line, the compared share first, then how many were
// set aside and the top reason: "6 of 307 compared; 301 set aside:
// 301 keploy/unmocked_call". A second reason or more is counted after it
// ("12 for 2 other reasons"), and tests that neither got a verdict nor were
// set aside (a set that stopped before them) close it ("; 3 not run").
func (s *ComparedShare) Line() string {
	if s == nil {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d of %d compared; %d set aside", s.Compared, s.Total, s.SetAside)
	if len(s.Reasons) > 0 {
		top := s.Reasons[0]
		fmt.Fprintf(&b, ": %d %s", top.Tests, top.Reason)
		reasons, tests := s.OtherReasons, s.OtherTests
		for _, r := range s.Reasons[1:] {
			reasons++
			tests += r.Tests
		}
		if reasons > 0 {
			word := "reasons"
			if reasons == 1 {
				word = "reason"
			}
			fmt.Fprintf(&b, ", %d for %d other %s", tests, reasons, word)
		}
	}
	if notRun := s.Total - s.Compared - s.SetAside; notRun > 0 {
		fmt.Fprintf(&b, "; %d not run", notRun)
	}
	return b.String()
}

// cutUTF8 is s cut to at most n bytes, on a rune boundary, marked "..." when
// cut.
func cutUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	end := n - len("...")
	for end > 0 && !utf8.RuneStart(s[end]) {
		end--
	}
	return s[:end] + "..."
}
