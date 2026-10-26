package models

import (
	"crypto/sha256"
	"fmt"
	"math"
	"net/url"
	"sort"
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
// longer than the record gap by a meaningful difference
// (GapDifferenceMeaningful) is what an elapsed-time span that expired at
// replay looks like; keploy cannot see the application's clock, so the note
// names that as the likely cause only then, and never as a certainty.
//
// The mirror is a replay faster than the recording: the recording passed the
// span, so a later test's recording has the call again, and the replay,
// still inside it, never makes that call. Its recording is then unconsumed,
// and RepeatNote on it (UnconsumedMock.RepeatOf) names the earlier identical
// recording that answered at this replay; there a replay gap meaningfully
// shorter than the record gap is the likely sign.

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
	// call came, at this replay; on an unconsumed recording, which this
	// replay never made, how long after it the recording's test began.
	// RecordGapSeconds is how long after Mock's call was recorded the test
	// that made this call began, at record time; RecordGapKnown is false when
	// that test's recorded start is not known, and RecordGapSeconds is then 0.
	// ReplayGapFromStart says ReplayGapSeconds runs to when this call's test
	// began at this replay, as RecordGapSeconds does at record, so the two
	// compare like for like (Anchor); false while it runs to the call (the
	// agent's note, which knows the call, not the test).
	ReplayGapSeconds   float64 `json:"replay_gap_seconds" yaml:"replay_gap_seconds"`
	ReplayGapFromStart bool    `json:"replay_gap_from_start,omitempty" yaml:"replay_gap_from_start,omitempty"`
	RecordGapSeconds   float64 `json:"record_gap_seconds,omitempty" yaml:"record_gap_seconds,omitempty"`
	RecordGapKnown     bool    `json:"record_gap_known,omitempty" yaml:"record_gap_known,omitempty"`
	// RecordedLast is the recording RecordGapSeconds runs from: of the
	// set's per-test recordings identical to Mock, the newest made before
	// this call's test began at record (Mock itself when that is the one).
	// An application that keeps an answer for a span of elapsed time was
	// last given it by that call at record, which another test may have made
	// after Mock's: that test's call, the cache still fresh at this replay,
	// was never made again, so its recording is not the one that answered.
	// Empty while the record gap is not known.
	RecordedLast string `json:"recorded_last,omitempty" yaml:"recorded_last,omitempty"`
}

// NewRepeatNote is the note of a call made at missAt, identical to mock,
// which answered an earlier call at answeredAt and was recorded at
// recordedAt. Its record gap is not known until Anchor measures it.
func NewRepeatNote(mock string, answeredAt, recordedAt, missAt time.Time) *RepeatNote {
	n := &RepeatNote{Mock: mock, AnsweredAt: answeredAt, RecordedAt: recordedAt}
	if !answeredAt.IsZero() && !missAt.IsZero() {
		n.ReplayGapSeconds = gapSeconds(missAt.Sub(answeredAt))
	}
	return n
}

// Anchor measures n's two gaps start to start: at this replay from when Mock
// answered the earlier call (AnsweredAt) to replayStart, when this call's
// test began; at record from recordedLastAt, when recordedLast was recorded
// (RecordedLast), to recordStart, when the test began then. A zero time
// leaves its gap as it was.
func (n *RepeatNote) Anchor(replayStart, recordStart time.Time, recordedLast string, recordedLastAt time.Time) {
	if n == nil {
		return
	}
	if !replayStart.IsZero() && !n.AnsweredAt.IsZero() {
		n.ReplayGapSeconds, n.ReplayGapFromStart = gapSeconds(replayStart.Sub(n.AnsweredAt)), true
	}
	if !recordStart.IsZero() && !recordedLastAt.IsZero() && recordedLast != "" {
		n.RecordGapSeconds, n.RecordGapKnown, n.RecordedLast = gapSeconds(recordStart.Sub(recordedLastAt)), true, recordedLast
	}
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

// MinGapDifferenceSeconds and MinGapDifferenceShare are how far apart a
// note's two gaps must be before they speak for or against a span of elapsed
// time (GapDifferenceMeaningful): at least a second, and more than a tenth of
// the longer gap. Closer than that, the difference is what a replay's timing
// gives any repeated call, a poller's included, and says nothing of a cache.
const (
	MinGapDifferenceSeconds = 1.0
	MinGapDifferenceShare   = 0.1
)

// GapDifferenceMeaningful reports whether a replay gap and a record gap, in
// seconds, are far enough apart to judge a likely cause by: at least
// MinGapDifferenceSeconds, and more than MinGapDifferenceShare of the longer.
func GapDifferenceMeaningful(replay, record float64) bool {
	d := math.Abs(replay - record)
	return d >= MinGapDifferenceSeconds && d > MinGapDifferenceShare*math.Max(replay, record)
}

// ElapsedTimeExpiryLikely reports whether the gaps look like a span measured
// in elapsed time that the recording stayed inside and the replay passed:
// the test came later after its earlier twin's call at replay than after the
// newest identical call before it at record (RecordedLast), by a meaningful
// difference (GapDifferenceMeaningful). Unknown when the record gap is not
// known, or the replay gap runs to the call, not to the test's start
// (ReplayGapFromStart): unlike gaps say nothing.
func (n *RepeatNote) ElapsedTimeExpiryLikely() (likely, known bool) {
	if n == nil || !n.RecordGapKnown || !n.ReplayGapFromStart {
		return false, false
	}
	return n.ReplayGapSeconds > n.RecordGapSeconds && GapDifferenceMeaningful(n.ReplayGapSeconds, n.RecordGapSeconds), true
}

// GapsTooClose reports whether n's gaps compare like for like but are too
// close to judge a cause by (GapDifferenceMeaningful): then a sentence states
// the gaps and claims neither cause.
func (n *RepeatNote) GapsTooClose() bool {
	if n == nil || !n.RecordGapKnown || !n.ReplayGapFromStart {
		return false
	}
	return !GapDifferenceMeaningful(n.ReplayGapSeconds, n.RecordGapSeconds)
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
	if n.ReplayGapFromStart {
		fmt.Fprintf(&b, "At replay this test began %s after that call", formatGap(n.ReplayGapSeconds))
	} else {
		fmt.Fprintf(&b, "At replay this call came %s after that one", formatGap(n.ReplayGapSeconds))
	}
	if n.RecordGapKnown {
		fmt.Fprintf(&b, "; at record this test began %s after %s", formatGap(n.RecordGapSeconds), n.recordedLastPhrase())
	}
	b.WriteString(", and this test's recording has no such call. ")
	switch likely, known := n.ElapsedTimeExpiryLikely(); {
	case known && likely:
		b.WriteString("The application likely kept the earlier answer for a span of elapsed time (a cache with expire-after-write, a token, a refresh window) that the recording stayed inside and this slower replay passed, so it asked again; keploy's time freeze does not slow that clock. ")
	case n.GapsTooClose():
		b.WriteString(gapsTooClose(true))
	case known:
		b.WriteString("The gap at replay was not longer than at record, so an expired cache, token or refresh window is a less likely cause: keploy cannot tell from here why the application asked again. ")
	default:
		b.WriteString("If the application keeps that answer for a span of elapsed time (a cache with expire-after-write, a token, a refresh window), a replay slower than the recording passes it and the application asks again; keploy's time freeze does not slow that clock. ")
	}
	fmt.Fprintf(&b, "keploy does not answer it with the earlier test's recording, which could hide a bug. To make the test independent of timing, record it with that cache disabled or its span longer than the replay ('%s'), or make the call in every test that needs it.", recordCmd)
	return b.String()
}

// ExplainMiss is the next step of a miss the note is on: Explain, unless the
// call is also closest, a recording keploy made where this replay did not
// make it (notOwned: another test's, named by recordedIn when the replay
// knows it, or one made between tests). Both explanations hold then, and the
// sentence names both: the call is that recording, which exact serving does
// not give this test, and it repeats the earlier answer, which may say why
// the application asked again here. Neither replaces the other: the
// recording made elsewhere is the reason the call missed, and the repeat a
// possible cause, which Explain judges by its gaps and claims only when
// they support it. keploy record's judging treats it as any call made
// elsewhere: it moves the recording to the test set's worker scope, and its
// confirming round still compares the test's response. The repeat is named
// for information only; it changes neither.
func (n *RepeatNote) ExplainMiss(notOwned bool, closest, recordedIn, recordCmd string) string {
	if n == nil {
		return ""
	}
	if !notOwned || closest == "" {
		return n.Explain(recordCmd)
	}
	where := "a recording keploy made between tests or in another test's window"
	if recordedIn != "" {
		where = "a recording keploy made in test " + recordedIn + "'s window"
	}
	return fmt.Sprintf("This request is %s, %s, where this replay did not make it: exact serving answers a test only with its own recordings. %s keploy record's judging treats this miss as any call made elsewhere: it moves %s to the test set's worker scope (mappings.yaml worker:), and its confirming round still compares this test's response. The repeat is named here for information only.", closest, where, n.Explain(recordCmd), closest)
}

// recordedLastPhrase names, in a sentence, the recording the record gap runs
// from (RecordedLast): "it" when that is Mock.
func (n *RepeatNote) recordedLastPhrase() string {
	if n.RecordedLast == "" || n.RecordedLast == n.Mock {
		return "it"
	}
	return n.RecordedLast + ", the newest identical recording made before it"
}

// formatGap renders a gap in seconds for a sentence.
func formatGap(s float64) string {
	return fmt.Sprintf("%.2fs", s)
}

// gapsTooClose is the cause sentence of a note whose gaps are too close to
// judge by (GapsTooClose), for a call the application made (asked) or a
// recorded call it did not make: it claims neither cause.
func gapsTooClose(asked bool) string {
	why := "why the application asked again"
	if !asked {
		why = "why the application did not ask again"
	}
	return "The two gaps are too close to tell a span of elapsed time (a cache, a token, a refresh window) from the replay's own timing: keploy cannot tell from here " + why + ". "
}

// ElapsedTimeFreshLikely is ElapsedTimeExpiryLikely's mirror, for a note on
// an unconsumed recording (ExplainUnmade): the test came sooner after the
// earlier twin at replay than at record, by a meaningful difference
// (GapDifferenceMeaningful), as a span the recording passed and the replay
// stayed inside does. Unknown when the record gap is not known.
func (n *RepeatNote) ElapsedTimeFreshLikely() (likely, known bool) {
	if n == nil || !n.RecordGapKnown || !n.ReplayGapFromStart {
		return false, false
	}
	return n.ReplayGapSeconds < n.RecordGapSeconds && GapDifferenceMeaningful(n.ReplayGapSeconds, n.RecordGapSeconds), true
}

// ExplainUnmade is Explain's sentence for a note on an unconsumed recording:
// a call this test recorded that the replay never made, identical to Mock,
// which answered an earlier test's call at this replay.
func (n *RepeatNote) ExplainUnmade(recordCmd string) string {
	if n == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString("This test's recording has a call identical to ")
	b.WriteString(n.Mock)
	if n.Test != "" {
		fmt.Fprintf(&b, ", the recording that answered test %s's call at this replay, and this replay never made it. ", n.Test)
	} else {
		b.WriteString(", a recording of an earlier test of this set that answered that test's call at this replay, and this replay never made it. ")
	}
	fmt.Fprintf(&b, "At replay this test began %s after that call", formatGap(n.ReplayGapSeconds))
	if n.RecordGapKnown {
		fmt.Fprintf(&b, "; at record it began %s after %s", formatGap(n.RecordGapSeconds), n.recordedLastPhrase())
	}
	b.WriteString(". ")
	switch likely, known := n.ElapsedTimeFreshLikely(); {
	case known && likely:
		b.WriteString("The application likely still held the earlier answer for a span of elapsed time (a cache with expire-after-write, a token, a refresh window) that this faster replay stayed inside and the recording passed, so it did not ask again; keploy's time freeze does not change that clock. ")
	case n.GapsTooClose():
		b.WriteString(gapsTooClose(false))
	case known:
		b.WriteString("The gap at replay was not shorter than at record, so a cache, token or refresh window still fresh is a less likely cause: keploy cannot tell from here why the application did not ask again. ")
	default:
		b.WriteString("If the application keeps that answer for a span of elapsed time (a cache with expire-after-write, a token, a refresh window), a replay faster than the recording stays inside it and the application does not ask again; keploy's time freeze does not change that clock. ")
	}
	fmt.Fprintf(&b, "To make the test independent of timing, record it with that cache disabled ('%s'), or make the call in every test that needs it.", recordCmd)
	return b.String()
}

// Brief is the note in a clause, for a one-line summary such as a JUnit
// failure's: the earlier recording, the test it answered once named, the two
// gaps, and the likely cause when the gaps support it. unmade is true for a
// note on an unconsumed recording (ExplainUnmade), false for a miss's.
func (n *RepeatNote) Brief(unmade bool) string {
	if n == nil {
		return ""
	}
	s := "identical to mock " + n.Mock
	if n.Test != "" {
		s += ", which answered test " + n.Test + "'s call"
	} else {
		s += ", which answered an earlier test's call"
	}
	s += " " + formatGap(n.ReplayGapSeconds) + " earlier at replay"
	if n.RecordGapKnown {
		s += " and " + formatGap(n.RecordGapSeconds) + " earlier at record"
		if n.RecordedLast != "" && n.RecordedLast != n.Mock {
			s += " (from " + n.RecordedLast + ")"
		}
	}
	likely, _ := n.ElapsedTimeExpiryLikely()
	if unmade {
		likely, _ = n.ElapsedTimeFreshLikely()
	}
	if likely {
		s += ": likely an answer the application keeps for a span of elapsed time"
	}
	return s
}

// RequestFingerprint tells recordings of one identical request apart from
// the rest (UnconsumedMock.RepeatOf): for an HTTP mock a digest of its
// method, its URL with the query in sorted order, its X-Amz-Target and its
// body, other headers aside; "" for a mock of another kind. It applies no
// noise, so it is stricter than a miss's comparison: two recordings it calls
// identical sent the same request. A mock whose payload is on disk keeps the
// one worked out before its payload was left there.
func (m *Mock) RequestFingerprint() string {
	if m == nil {
		return ""
	}
	if m.onDisk != nil {
		return m.onDisk.fingerprint
	}
	return requestFingerprint(m)
}

// requestFingerprint is RequestFingerprint of a mock whose payload is in
// memory.
func requestFingerprint(m *Mock) string {
	r := m.Spec.HTTPReq
	if m.Kind != HTTP || r == nil {
		return ""
	}
	// The target as the parser's request keys read it (recordedRequestKeys):
	// of the header's spellings, the first in sorted order, so a mock that
	// carries two spellings has one digest, not one per map order.
	names := make([]string, 0, len(r.Header))
	for k := range r.Header {
		names = append(names, k)
	}
	sort.Strings(names)
	target := ""
	for _, k := range names {
		if strings.EqualFold(k, "X-Amz-Target") {
			target = r.Header[k]
			break
		}
	}
	h := sha256.New()
	for _, part := range []string{string(r.Method), sortedQueryURL(r.URL), target, r.Body, r.Binary, r.BodyRef.Path} {
		// Each part with its length first, so no two splits of the same
		// bytes give one digest.
		fmt.Fprintf(h, "%d:%s|", len(part), part)
	}
	return string(h.Sum(nil))
}

// sortedQueryURL is raw with its query parameters in sorted order, so two
// requests that differ only in the order of their parameters are one; raw
// itself when it does not parse.
func sortedQueryURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.RawQuery == "" {
		return raw
	}
	q, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return raw
	}
	u.RawQuery = q.Encode()
	return u.String()
}
