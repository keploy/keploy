package models

import (
	"fmt"
	"math"
	"strings"
	"time"
)

// REPEAT OF AN EARLIER RECORDED CALL.
//
// An application that keeps a dependency's answer for a span of ELAPSED time
// (a cache with expire-after-write, a token kept until it nears expiry, a
// refresh window) makes the call once and then answers from memory. keploy's
// time freeze moves the wall clock only, so such a span runs on the real
// monotonic clock. When the recording ran faster than the span, the call was
// recorded once, in the test that filled the cache, and later tests made no
// call. A slower replay passes the span, and the application makes the call
// again in a later test whose recording has none: keploy refuses it, since
// answering with another test's recording could hide a bug, and the test
// fails on a miss that alone says nothing ("no_mocks, candidates 0").
//
// RepeatNote is what the miss then says: the call is identical to a recording
// an earlier test of the set made, which already answered at this replay, and
// how far apart the two calls were at replay and at record. A replay gap
// longer than the record gap is what an elapsed-time span that expired at
// replay looks like; keploy cannot see the application's clock, so the note
// names that as the likely cause, never as a certainty.

// RepeatNote says that a call no mock answered is identical to a recording an
// earlier test of the set made, which had already answered the same call at
// this replay (MockMismatchReport.Repeat, UnmatchedCall.Repeat). Identical is
// the parser's judgement: for HTTP the method, destination, path, query and
// body (and the X-Amz-Target) after the configured and learned noise, other
// headers aside.
type RepeatNote struct {
	// Mock is the earlier recording the call is identical to.
	Mock string `json:"mock" yaml:"mock"`
	// Test is the earlier test of the set whose call Mock answered at this
	// replay, as the replay names it at the end of the set; empty while it is
	// not known (the agent knows the recording, not the test).
	Test string `json:"test,omitempty" yaml:"test,omitempty"`
	// AnsweredAt is when Mock answered that earlier call at this replay;
	// RecordedAt is when Mock's call was recorded.
	AnsweredAt time.Time `json:"answered_at,omitzero" yaml:"answered_at,omitempty"`
	RecordedAt time.Time `json:"recorded_at,omitzero" yaml:"recorded_at,omitempty"`
	// ReplayGapSeconds is how long after Mock answered the earlier call this
	// call came, at this replay. RecordGapSeconds is how long after Mock's
	// call was recorded the test that made this call began, at record time;
	// RecordGapKnown is false when the agent did not know that test's
	// recorded start, and RecordGapSeconds is then 0.
	ReplayGapSeconds float64 `json:"replay_gap_seconds" yaml:"replay_gap_seconds"`
	RecordGapSeconds float64 `json:"record_gap_seconds,omitempty" yaml:"record_gap_seconds,omitempty"`
	RecordGapKnown   bool    `json:"record_gap_known,omitempty" yaml:"record_gap_known,omitempty"`
}

// NewRepeatNote is the note of a call made at missAt, identical to mock,
// which answered an earlier call at answeredAt and was recorded at
// recordedAt, in a test that began at record time at testRecordedStart (zero
// when not known).
func NewRepeatNote(mock string, answeredAt, recordedAt, missAt, testRecordedStart time.Time) *RepeatNote {
	n := &RepeatNote{Mock: mock, AnsweredAt: answeredAt, RecordedAt: recordedAt}
	if !answeredAt.IsZero() && !missAt.IsZero() {
		n.ReplayGapSeconds = gapSeconds(missAt.Sub(answeredAt))
	}
	if !testRecordedStart.IsZero() && !recordedAt.IsZero() {
		n.RecordGapSeconds, n.RecordGapKnown = gapSeconds(testRecordedStart.Sub(recordedAt)), true
	}
	return n
}

// gapSeconds is d in seconds, to the hundredth, never below zero: two clocks
// a few microseconds apart must not print a negative gap.
func gapSeconds(d time.Duration) float64 {
	if d < 0 {
		return 0
	}
	return math.Round(d.Seconds()*100) / 100
}

// Clone is a copy of n; nil for nil.
func (n *RepeatNote) Clone() *RepeatNote {
	if n == nil {
		return nil
	}
	c := *n
	return &c
}

// ElapsedTimeExpiryLikely reports whether the gaps look like a span measured
// in elapsed time that the recording stayed inside and the replay passed: the
// call came later after its earlier twin at replay than its test came after
// that twin at record. Unknown when the record gap is not known.
func (n *RepeatNote) ElapsedTimeExpiryLikely() (likely, known bool) {
	if n == nil || !n.RecordGapKnown {
		return false, false
	}
	return n.ReplayGapSeconds > n.RecordGapSeconds, true
}

// Explain is the note's sentence for a report and the CLI: which recording
// (and, once named, which test) the call repeats, the two gaps, the likely
// cause, and what to do. recordCmd is the command that records the set again.
func (n *RepeatNote) Explain(recordCmd string) string {
	if n == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString("This request is identical to ")
	b.WriteString(n.Mock)
	if n.Test != "" {
		fmt.Fprintf(&b, ", the recording that already answered test %s's call at this replay. ", n.Test)
	} else {
		b.WriteString(", a recording of an earlier test of this set that already answered that test's call at this replay. ")
	}
	fmt.Fprintf(&b, "At replay this call came %s after that one", formatGap(n.ReplayGapSeconds))
	if n.RecordGapKnown {
		fmt.Fprintf(&b, "; at record this test began %s after it", formatGap(n.RecordGapSeconds))
	}
	b.WriteString(", and this test's recording has no such call. ")
	switch likely, known := n.ElapsedTimeExpiryLikely(); {
	case known && likely:
		b.WriteString("The application likely kept the earlier answer for a span of elapsed time (a cache with expire-after-write, a token, a refresh window) that the recording stayed inside and this slower replay passed, so it asked again; keploy's time freeze does not slow that clock. ")
	case known:
		b.WriteString("The gap at replay was not longer than at record, so an expired cache, token or refresh window is a less likely cause: keploy cannot tell from here why the application asked again. ")
	default:
		b.WriteString("If the application keeps that answer for a span of elapsed time (a cache with expire-after-write, a token, a refresh window), a replay slower than the recording passes it and the application asks again; keploy's time freeze does not slow that clock. ")
	}
	fmt.Fprintf(&b, "keploy does not answer it with the earlier test's recording, which could hide a bug. To make the test independent of timing, record it with that cache disabled or its span longer than the replay ('%s'), or make the call in every test that needs it.", recordCmd)
	return b.String()
}

// formatGap renders a gap in seconds for a sentence.
func formatGap(s float64) string {
	return fmt.Sprintf("%.2fs", s)
}
