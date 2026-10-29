package models

import (
	"regexp"
	"slices"
	"sort"
	"strings"
)

// Sanitized is m as evidence keeps it: the protocol, the
// destination, how far the matcher got, the closest recorded mock's name,
// the matcher's next step, when it happened, why it missed (the learn step's
// reason, in record's full round), how an answered call was answered and the
// process that made the call (UnmatchedCall.Answer, Source: names,
// no value), and the call's shape
// (MissShape): for HTTP and gRPC the method and the target's shape,
// for SQL the statement with its literals written "?". No body, no bound
// parameter, no SQL literal, no query value, and no path segment that looks
// like a value (PathShape says which do) or that a test variable's
// place on the call names (VariablePlaces: one the miss names, one the
// closest mock or a mock on the call's route declares, or one that holds a
// value the test bound). The next step is the matcher's advice, or the
// learn step's reason, with each field path it names in its shape
// (SanitizedNextStep). ChargedTo stays: it names a test, no value.
func (m UnmatchedCall) Sanitized() UnmatchedCall {
	out := UnmatchedCall{Protocol: m.Protocol, Destination: m.Destination, LiveDestination: m.LiveDestination, MatchPhase: m.MatchPhase,
		ClosestMock: m.ClosestMock, NextSteps: m.SanitizedNextStep(), At: m.At, ActualSummary: MissShape(m), Differs: FieldShapes(m.Differs), FieldReasons: FieldReasonShapes(m.FieldReasons),
		Kind: m.Kind, Recorded: m.Recorded, Write: m.Write, NotOwned: m.NotOwned, Reason: m.Reason, Concurrent: m.Concurrent.Clone(),
		// An answered call stays one (UnmatchedCall.Answer): how it was
		// answered and by which process, no value.
		Answer: m.Answer, Drift: m.Drift, Reused: m.Reused, WriteKind: m.WriteKind, Source: m.Source, RecordedIn: m.RecordedIn, ChargedTo: m.ChargedTo,
		Repeat: m.Repeat.Clone(), Background: m.Background, Clock: m.Clock}
	// A variable's path places name segments, not values: kept, so a shape
	// of this miss (MissShape) still writes them {}. Any other place
	// can name a JSON key that is a value.
	for _, p := range m.VariablePlaces {
		if IsPathPlace(p) {
			out.VariablePlaces = append(out.VariablePlaces, p)
		}
	}
	if m.MatchPhase == MatchPhaseVariable {
		// A test variable did not hold, or the closest mock's variables could
		// not be followed. The part of the call it is in is kept: a URL path
		// segment by its position, a header or query parameter by its name.
		// A body place is kept as "body" alone, since a JSON key can be a
		// value the app sent (a map keyed by an email). No value is kept.
		for _, d := range m.FieldDiffs {
			path := d.Path
			if strings.HasPrefix(path, "body.") || strings.HasPrefix(path, "body[") {
				path = "body"
			}
			out.FieldDiffs = append(out.FieldDiffs, MockFieldDiff{Path: path, Kind: d.Kind})
		}
		out.NextSteps = "a test variable did not hold in " + m.ClosestMock + " (the call sent another value than the one the test bound, a value of another type, or a value before the call that binds it), or that mock's variables cannot be followed: fix the application, or re-record the test with 'keploy mock record' if that is intended"
	}
	return out
}

// SanitizedNextStep is m's next step as evidence keeps it: the matcher's
// advice, or the learn step's reason in record's full round, with every
// field path it names written in its shape (FieldShape). A next step
// names the fields that differed, and others (where a value was recorded),
// and a map's keys can be values (an email, a user ID): each of the miss's
// field paths, whole or without its "body." (as a learn step's reason may
// name it), and every body path the text names (bodyPlace) keeps its shape,
// so a key that may be a value is {}. The rest of the text stays: it is the
// matcher's or the learn step's own words, and holds no value of the call.
// (It was once replaced whole by a count of the fields, which hid the learn
// step's reason from every in-test miss of the round.) Why a replay refused
// a field for a reason of its own (a learned read of the application's
// clock the call does not carry: FieldReasons) is added to it, as the
// miss's reason says it, so the run's summary, which shows the next step,
// says it too.
func (m UnmatchedCall) SanitizedNextStep() string {
	next := m.shapedNextStep()
	var whys []string
	for _, r := range FieldReasonShapes(m.FieldReasons) {
		if !strings.Contains(next, r.Why) {
			whys = append(whys, r.Why)
		}
	}
	switch {
	case len(whys) == 0:
		return next
	case next == "":
		return strings.Join(whys, "; ")
	}
	return next + " (" + strings.Join(whys, "; ") + ")"
}

// shapedNextStep is m's own next step with every field path it names in its
// shape (SanitizedNextStep).
func (m UnmatchedCall) shapedNextStep() string {
	text := m.NextSteps
	if text == "" {
		return ""
	}
	var paths []string
	for _, d := range m.FieldDiffs {
		for _, p := range []string{d.Path, strings.TrimPrefix(d.Path, "body.")} {
			if p != "" && FieldShape(p) != p && !slices.Contains(paths, p) {
				paths = append(paths, p)
			}
		}
	}
	if len(paths) > 0 {
		// Longest first: a path is shaped whole, never inside a longer one
		// it begins; the replacer reads the text once, so no shape is
		// shaped again.
		sort.SliceStable(paths, func(i, j int) bool { return len(paths[i]) > len(paths[j]) })
		args := make([]string, 0, 2*len(paths))
		for _, p := range paths {
			args = append(args, p, FieldShape(p))
		}
		text = strings.NewReplacer(args...).Replace(text)
	}
	return bodyPlace.ReplaceAllStringFunc(text, func(p string) string {
		// A sentence's full stop or a list's separator is not the path's.
		trimmed := strings.TrimRight(p, ".,;:")
		return FieldShape(trimmed) + p[len(trimmed):]
	})
}

// bodyPlace is a body field path as a matcher's or a learn step's text names
// one (body.cmd.filter.appId, body.docs.msgs.0.q.entity_id, body[0].id): the
// root every protocol's compared places share.
var bodyPlace = regexp.MustCompile(`\bbody[.\[][^\s,;()'"]*`)
