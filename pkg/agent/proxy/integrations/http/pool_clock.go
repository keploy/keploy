package http

import (
	"context"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// A TIMER'S CALL IN A TEST'S WINDOW, ANSWERED BY A START-UP OR
// BETWEEN-TESTS RECORDING WHOSE CLOCK READS MOVED WITH THE CALL'S CLOCK
// (under the proven switch, exact serving; the Mongo parser does the same,
// keploy/integrations pkg/mongo/v2/pool_clock.go).
//
// An application's timer can carry its clock in what it sends: a poll's
// "?since=<now - 60 s>", a heartbeat's {"at": <now>}, a lease that expires
// "now + 5 minutes". Its recordings made while no test was open (the set's
// start-up and between-tests recordings, the pool a test's window is offered
// last: integrations.FromPool) learned no clock read, as the learn step
// pairs nothing onto them while a test's view is shown, so a tick the replay
// makes inside a test's window is the same request made at another instant,
// and no recording is this very call (strictDiff): it missed, and charged the
// test with a call it never made.
//
// Such a call is answered by the first of those recordings, in recorded
// order, that it is but for reads of the clock (clockMoved): every compared
// field as the strict comparison reads it (keploy.yml's noise, the static
// header list, the recording's learned noise and redacted values aside), and
// every field that differs one instant in both (a whole number of seconds,
// ten digits, a fraction allowed, or of milliseconds, thirteen, since the
// epoch, from 2000 to 2100: a JSON number or string, a query value, a form
// value, a header value, a path segment) whose instant moved, between the
// recording and the call, by one and the same amount, the gap between the
// two calls' clocks: each field keeps the recording's own offset from its
// read ("since" stays sixty seconds before the call's clock, an expiry five
// minutes after it), and the call's read is no earlier than its span on the
// application's clock (learn.ClockSpanFrom) allows, given that the
// recording's read came no later than the recorded call. Every such field
// must also lie near the clock on each side: the recording's value within T
// of the recorded call's time, the call's within T of its span on the
// application's clock, T being the set's recorded range (from its first
// recorded request to its last recorded answer, which the agent reads once
// as it stages the set: learn.Span's Recorded) plus the call's span. So a
// counter or an id in the epoch range that happens to move by the shared
// amount is never read as the clock (an HTTP field is a number or a text,
// so the bound reads every one); where the set's range is not known, no
// field is read as the clock. It is served as the exact pool copy is: unchecked, never required, once (the store's
// consumption ledger), never to a call one more than the test's own
// recordings (repeatsOwn reads the same rule), and named on the test's
// evidence as the set's background. A recording that declares variable
// places is not read by this rule.
//
// The rule reads no provenance of a field (whether the app made it from its
// clock or carried it from an earlier answer): a value the app carries from
// a recorded answer is served back unchanged at replay, so it never differs,
// and a field that differs must move by the one amount every other field
// moved by, near both clocks.
//
// Stated limits. An offset larger than T (a "since an hour ago" bound in a
// two-minute set) is not read as the clock: that tick misses, as before this
// rule (a set-aside, never a false green). The recording's read has no
// earlier bound here (a replay
// does not hold the set's previous recorded call), so an offset that grew by
// the same amount in every clock field at once passes; one field's offset
// changed against another's does not. A field read whole as the call's clock
// in text (an RFC 3339 date) is not read by this rule. A field the recording
// learned as a clock read (MockSpec.Clocks) is checked as learned.

// movedPlace is one place of a request, in the learn step's dialect
// (body.at, url.query.since, header.x-since, url.path.2), that holds one
// instant in the recording (rec) and in the call (live), written at one
// resolution (res), and differs.
type movedPlace struct {
	rec, live time.Time
	res       time.Duration
	// compared: the strict comparison reached the place and let it change
	// (strictKnown.learnedAt). A place it never compares (keploy.yml's
	// noise, the static header list, a learned or redacted value) moved
	// nothing that counts.
	compared bool
}

// clockCall is a live request as clockMoved reads it, read once for every
// recording it is compared with: where it went, its leaves in the learn
// step's dialect, and its span on the application's clock (known false
// where keploy cannot tell it).
type clockCall struct {
	input  *req
	live   destination
	leaves map[string]learn.Leaf
	span   learn.Span
	known  bool
}

// clockCallOf reads input (clockCall); ok is false with the switch off and
// for a request with no URL.
func clockCallOf(ctx context.Context, input *req) (clockCall, bool) {
	if !provenswitch.On() || input == nil || input.url == nil {
		return clockCall{}, false
	}
	span, known := learn.ClockSpanFrom(ctx, input.arrived)
	return clockCall{input: input, live: liveDestination(ctx, input), leaves: learnNoise{}.shape(liveParts(ctx, input), "").Leaves, span: span, known: known}, true
}

// clockMoved reports whether the live request (c) is m's recording but for
// fields that moved with the call's clock (see above), and those fields by
// their shape (clockField), sorted. ok is false where any other compared
// field differs, where no compared field moved (the call is then the
// recording itself, which the strict comparison answers), and where the
// call's span on the application's clock or the recording's time is
// unknown: a field checked against no clock would pass whatever it holds.
// n is the run's noise.
func clockMoved(ctx context.Context, c clockCall, m *models.Mock, n strictNoise) ([]string, bool) {
	if !c.known || m == nil || m.Spec.HTTPReq == nil || m.Spec.ReqTimestampMock.IsZero() || hasVariables(m) {
		return nil, false
	}
	moved := movedPlaces(learnNoise{}.shape(recordedParts(m), "").Leaves, c.leaves)
	if len(moved) == 0 || len(strictDiffMoved(ctx, c.input, c.live, m, n, moved)) != 0 {
		return nil, false
	}
	var drifts []movedPlace
	var fields []string
	for p, mv := range moved {
		if mv.compared {
			drifts = append(drifts, *mv)
			fields = append(fields, clockField(p).shape)
		}
	}
	if len(drifts) == 0 || !movedTogether(drifts, c.span.From.Sub(m.Spec.ReqTimestampMock)) || !nearTheClocks(drifts, m.Spec.ReqTimestampMock, c.span) {
		return nil, false
	}
	sort.Strings(fields)
	return fields, true
}

// movedPlaces are the leaves recorded and live both hold, of one type, whose
// values differ and are each one instant at one resolution (epochInstant).
func movedPlaces(recorded, live map[string]learn.Leaf) map[string]*movedPlace {
	var out map[string]*movedPlace
	for p, rl := range recorded {
		ll, ok := live[p]
		if !ok || rl.Unnamed || ll.Type != rl.Type || ll.Value == rl.Value {
			continue
		}
		rt, rres, ok := epochInstant(rl.Value)
		if !ok {
			continue
		}
		lt, lres, ok := epochInstant(ll.Value)
		if !ok || lres != rres {
			continue
		}
		if out == nil {
			out = map[string]*movedPlace{}
		}
		out[p] = &movedPlace{rec: rt, live: lt, res: rres}
	}
	return out
}

// movedTogether reports whether every drift moved by one amount, the gap
// between the two calls' reads of their clocks: a value cut to its
// resolution in both runs (a floor, a round, alike) moves by the gap to
// less than one cut, so one gap lies less than one cut from every drift;
// and that gap is no less than least, the earliest the call's read (no
// earlier than its span's start) can be after the recording's (no later
// than its call).
func movedTogether(drifts []movedPlace, least time.Duration) bool {
	first := true
	var lo, hi time.Duration
	for _, d := range drifts {
		moved := d.live.Sub(d.rec)
		if first || moved-d.res > lo {
			lo = moved - d.res
		}
		if first || moved+d.res < hi {
			hi = moved + d.res
		}
		first = false
	}
	return !first && lo < hi && hi > least
}

// nearTheClocks reports whether every drift lies near the clock on each
// side: its recorded instant within T of recorded (the recorded call's
// time), its live one within T of the call's span on the application's
// clock (span.From to span.To), T being the set's recorded range plus the
// span (Span.Recorded). False where the set's range is not known: a value
// bounded by no range would pass as the clock wherever it lies.
func nearTheClocks(drifts []movedPlace, recorded time.Time, span learn.Span) bool {
	if !span.RecordedKnown || span.To.Before(span.From) {
		return false
	}
	tol := span.Recorded + span.To.Sub(span.From)
	for _, d := range drifts {
		if gap := d.rec.Sub(recorded); gap > tol || gap < -tol {
			return false
		}
		if d.live.Before(span.From.Add(-tol)) || d.live.After(span.To.Add(tol)) {
			return false
		}
	}
	return true
}

// epochInstant reads s as seconds (ten digits, with or without a fraction,
// to the fraction's resolution) or milliseconds (thirteen digits) since the
// epoch, from 2000 up to 2100, the instants the Mongo parser's rule reads
// too. ok is false for any other text.
func epochInstant(s string) (time.Time, time.Duration, bool) {
	whole, frac, dotted := strings.Cut(s, ".")
	if whole == "" || strings.Trim(whole, "0123456789") != "" || (dotted && (frac == "" || strings.Trim(frac, "0123456789") != "")) {
		return time.Time{}, 0, false
	}
	n, err := strconv.ParseInt(whole, 10, 64)
	if err != nil {
		return time.Time{}, 0, false
	}
	var t time.Time
	var res time.Duration
	switch {
	case len(whole) == 10 && dotted:
		ns, _ := strconv.ParseInt((frac + "000000000")[:9], 10, 64)
		res = time.Second
		for i := 0; i < min(len(frac), 9); i++ {
			res /= 10
		}
		t = time.Unix(n, ns).UTC()
	case len(whole) == 10:
		t, res = time.Unix(n, 0).UTC(), time.Second
	case len(whole) == 13 && !dotted:
		t, res = time.UnixMilli(n).UTC(), time.Millisecond
	default:
		return time.Time{}, 0, false
	}
	if t.Before(epochFrom) || !t.Before(epochTo) {
		return time.Time{}, 0, false
	}
	return t, res, true
}

var (
	epochFrom = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	epochTo   = time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC)
)

// clockMovedRecording is a recording a call is but for its clock
// (clockMoved), and the fields that moved.
type clockMovedRecording struct {
	m      *models.Mock
	fields []string
}

// clockMovedRecordings are those of mocks, in their order, that the request
// is but for fields that moved with its clock (clockMoved).
func clockMovedRecordings(ctx context.Context, input *req, mocks []*models.Mock, n strictNoise) []clockMovedRecording {
	if len(mocks) == 0 {
		return nil
	}
	c, ok := clockCallOf(ctx, input)
	if !ok || !c.known {
		return nil
	}
	var out []clockMovedRecording
	for _, m := range mocks {
		if fields, ok := clockMoved(ctx, c, m, n); ok {
			out = append(out, clockMovedRecording{m: m, fields: fields})
		}
	}
	return out
}
