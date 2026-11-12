package mock

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"

	"go.keploy.io/server/v3/pkg/models"
)

// recordMoveLabel names, on a RULE USED line, the series keploy record moved
// as trailing a test on its own, with no declared rule (seriesDue.Rule
// empty), as RuleNotMadeLine names them.
const recordMoveLabel = "(keploy record's trail move)"

// seriesRepeatReader is an optional Instrumentation extension, which
// pkg/platform/http.AgentClient has: the agent's non-draining mock stats,
// whose SeriesRepeats the run's RULE USED lines read.
type seriesRepeatReader interface {
	GetMockStats(ctx context.Context) (models.MockStats, error)
}

// seriesRepeats is what the run read of how many calls in tests each series
// answered past its recorded count (models.MockStats.SeriesRepeats): by
// series, or, when none was read, why not (unread).
type seriesRepeats struct {
	bySeries map[string]int64
	unread   string
}

// readSeriesRepeats reads, from an agent that counts them, how many calls in
// tests each series answered past its recorded count. A client that cannot
// ask, an agent that does not count them, or a failed read is no count: its
// unread says which, and the RULE USED lines say the repeats were not
// counted. It never fails the run: whether a series was made is read from
// what the run consumed, not from this count.
func readSeriesRepeats(ctx context.Context, instr Instrumentation) seriesRepeats {
	r, ok := instr.(seriesRepeatReader)
	if !ok {
		return seriesRepeats{unread: "this keploy's agent client cannot ask for them"}
	}
	stats, err := r.GetMockStats(ctx)
	if err != nil {
		return seriesRepeats{unread: "the read failed: " + err.Error()}
	}
	if !stats.SeriesCounted {
		return seriesRepeats{unread: "this keploy agent does not count them"}
	}
	return seriesRepeats{bySeries: stats.SeriesRepeats}
}

// ruleUse is what one rule's series did in a run: the run's RULE USED line
// for the rule (RuleUsedLine).
type ruleUse struct {
	// Rule is the rule, "<id>@<first 8 hex of its digest>"; empty for the
	// series keploy record moved on its own.
	Rule string
	// Series is how many series the rule placed in the set's tests.
	Series int
	// Known is whether what the run consumed was read: without it nothing
	// below but Series is said.
	Known bool
	// Answered is how many calls of the run its series answered: each
	// recording used once, plus Repeats.
	Answered int64
	// Repeats is how many calls in tests the last recording of a series
	// answered past the series' recorded count; -1 when not counted
	// (RepeatsUnread says why).
	Repeats       int64
	RepeatsUnread string
	// Misses is how many of the run's misses carried the value of a test a
	// series of the rule trails (models.UnmatchedCall.TrailsOf); -1 when the
	// misses were not read.
	Misses int
	// Required, Made, NotMade and NotDue count the series required once per
	// run (seriesDue.write): made, due and not made (RULE NOT MADE), or not
	// due (their test did not run). ServedOnly counts the rest: series of
	// reads, or of writes that changed nothing when recorded. NotYetDue
	// counts, of Required, the series not made that the run did not require
	// because the application exited before their rule's timer bound
	// (seriesDue.early, RULE NOT DUE).
	Required, Made, NotMade, NotDue, ServedOnly, NotYetDue int
}

// ruleUses is, by rule in rule order (keploy record's own trail moves
// first), what each rule's series of placed did in a run whose tests are
// tests, whose agent consumed consumed (known false: that read failed),
// counted repeats, and missed misses (missesKnown false: that read failed).
// nil when placed holds no series.
func ruleUses(placed map[string]seriesDue, tests []TestReceipt, consumed []models.MockState, known bool, repeats seriesRepeats, misses []models.UnmatchedCall, missesKnown bool) []ruleUse {
	if len(placed) == 0 {
		return nil
	}
	ran := ranTests(tests)
	used := usedNames(consumed)
	by := map[string]*ruleUse{}
	trailed := map[string]map[string]bool{}
	for _, id := range slices.Sorted(maps.Keys(placed)) {
		s := placed[id]
		u := by[s.Rule]
		if u == nil {
			u = &ruleUse{Rule: s.Rule, Known: known}
			if repeats.unread != "" {
				u.Repeats, u.RepeatsUnread = -1, repeats.unread
			}
			by[s.Rule] = u
			trailed[s.Rule] = map[string]bool{}
		}
		u.Series++
		trailed[s.Rule][s.Trails] = true
		if !known {
			continue
		}
		for _, n := range s.Names {
			if used[n] {
				u.Answered++
			}
		}
		// No count read (Repeats -1) has no series in bySeries: it adds 0.
		u.Repeats += repeats.bySeries[id]
		u.Answered += repeats.bySeries[id]
		switch {
		case !s.write:
			u.ServedOnly++
		case !s.dueIn(ran):
			u.Required++
			u.NotDue++
		case s.madeIn(used):
			u.Required++
			u.Made++
		case s.early:
			u.Required++
			u.NotYetDue++
		default:
			u.Required++
			u.NotMade++
		}
	}
	out := make([]ruleUse, 0, len(by))
	for _, rule := range slices.Sorted(maps.Keys(by)) {
		u := by[rule]
		if !missesKnown {
			u.Misses = -1
		} else {
			// A background series trails no test (""): no miss names it.
			for _, c := range misses {
				if c.TrailsOf != "" && trailed[rule][c.TrailsOf] {
					u.Misses++
				}
			}
		}
		out = append(out, *u)
	}
	return out
}

// ranTests is the set of the tests of tests that ran (ranOutcome).
func ranTests(tests []TestReceipt) map[string]bool {
	ran := make(map[string]bool, len(tests))
	for _, t := range tests {
		if ranOutcome(t.Outcome) {
			ran[t.Name] = true
		}
	}
	return ran
}

// plural is n and word, with an s (es after an s) unless n is 1.
func plural(n int64, word string) string {
	switch {
	case n == 1:
		return fmt.Sprintf("%d %s", n, word)
	case strings.HasSuffix(word, "s"):
		return fmt.Sprintf("%d %ses", n, word)
	}
	return fmt.Sprintf("%d %ss", n, word)
}

// RuleUsedLine is the run's RULE USED line for u: how many series the rule
// placed, the calls the run answered under it, the misses that carried the
// value of a test it trails, and its series required once per run (made,
// not made, not due), with the line's fields as JSON at its end
// (" rule={...}", keys sorted), as keploy's other RULE lines end.
func RuleUsedLine(u ruleUse) string {
	label, fields := u.Rule, map[string]any{"event": "used", "series": u.Series}
	if id, _, _ := strings.Cut(u.Rule, "@"); u.Rule != "" {
		fields["id"] = id
	} else {
		label, fields["by"] = recordMoveLabel, "record"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "RULE USED %s: %d series", label, u.Series)
	if !u.Known {
		fields["unread"] = true
		b.WriteString("; what this run's agent served could not be read, so this line says nothing of what the run answered or made")
		return finishRuleLine(b.String(), fields)
	}
	fmt.Fprintf(&b, "; this run answered %s", plural(u.Answered, "call"))
	if u.Repeats < 0 {
		fmt.Fprintf(&b, " (repeats past the recorded count not counted: %s)", u.RepeatsUnread)
	} else {
		fmt.Fprintf(&b, " (%s in tests past the recorded count, by the last of their series)", plural(u.Repeats, "repeat"))
	}
	fields["answered"], fields["repeats"] = u.Answered, u.Repeats
	if u.Misses < 0 {
		b.WriteString("; misses not read")
	} else {
		fmt.Fprintf(&b, "; %s carried the value of a test it trails", plural(int64(u.Misses), "miss"))
	}
	fields["misses"] = u.Misses
	fmt.Fprintf(&b, "; required %d: made %d, not made %d, not due %d (their test did not run)", u.Required, u.Made, u.NotMade, u.NotDue)
	if u.NotYetDue > 0 {
		fmt.Fprintf(&b, ", not yet due %d (the app exited before their timer bound)", u.NotYetDue)
		fields["not_yet_due"] = u.NotYetDue
	}
	fmt.Fprintf(&b, "; served, not required %d (reads, or writes that changed nothing when recorded)", u.ServedOnly)
	fields["required"], fields["made"], fields["not_made"], fields["not_due"], fields["served_only"] = u.Required, u.Made, u.NotMade, u.NotDue, u.ServedOnly
	return finishRuleLine(b.String(), fields)
}

// RulesLine is the run's RULES line: the rules of uses together, as the
// RULE USED lines count them one by one.
func RulesLine(uses []ruleUse) string {
	var rules, series, notMade int
	var answered, repeats int64
	var misses int
	known, counted, missesRead := true, true, true
	for _, u := range uses {
		if u.Rule != "" {
			rules++
		}
		series += u.Series
		known = known && u.Known
		answered += u.Answered
		notMade += u.NotMade
		if u.Repeats < 0 {
			counted = false
		} else {
			repeats += u.Repeats
		}
		if u.Misses < 0 {
			missesRead = false
		} else {
			misses += u.Misses
		}
	}
	fields := map[string]any{"event": "rules", "rules": rules, "series": series}
	var b strings.Builder
	fmt.Fprintf(&b, "RULES: %s placed %d series on this set", plural(int64(rules), "declared rule"), series)
	if len(uses) > rules {
		b.WriteString(", keploy record's own trail moves among them")
		fields["record_moves"] = true
	}
	if !known {
		fields["unread"] = true
		b.WriteString("; what this run's agent served could not be read")
		return finishRuleLine(b.String(), fields)
	}
	fmt.Fprintf(&b, "; this run answered %s under them", plural(answered, "call"))
	fields["answered"] = answered
	if counted {
		fmt.Fprintf(&b, " (%s past the recorded count)", plural(repeats, "repeat"))
		fields["repeats"] = repeats
	} else {
		b.WriteString(" (repeats past the recorded count not counted)")
		fields["repeats"] = -1
	}
	if missesRead {
		fmt.Fprintf(&b, "; %s carried the value of a test they trail", plural(int64(misses), "miss"))
		fields["misses"] = misses
	} else {
		b.WriteString("; misses not read")
		fields["misses"] = -1
	}
	fmt.Fprintf(&b, "; required series not made: %d", notMade)
	fields["not_made"] = notMade
	return finishRuleLine(b.String(), fields)
}

// finishRuleLine is line with ". rule=" and fields as JSON (keys sorted).
func finishRuleLine(line string, fields map[string]any) string {
	b, err := json.Marshal(fields)
	if err != nil {
		b = []byte(`{"event":"unwritable"}`)
	}
	return line + ". rule=" + string(b)
}
