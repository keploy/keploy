// Package mismatch is the shared framework protocol parsers use to report a
// mock miss. It guarantees a uniform vocabulary across protocols so the same
// failure renders the same way in the CLI mismatch table, `keploy report`,
// the test-report yaml (FailureInfo.UnmatchedCalls) and the platform UI.
//
// Field-diff paths use the SAME grammar as the noise configuration
// ("body.<dotted.path>", "header.<name>", "query.<name>", "method", "path")
// so a user can copy a reported path directly into test.globalNoise or a
// testcase's spec.assertions.noise.
//
// A parser that detects a miss should:
//
//	report := mismatch.NewReport(mismatch.ProtocolHTTP, "GET /orders/42").
//	    WithPhase(models.MatchPhaseExhausted, candidateCount).
//	    WithClosest(closest.Name, fieldDiffs).
//	    WithNextSteps("...").Build()
//	errCh <- models.NewMockMismatchError(baseErr, report)
//
// proxy.go extracts the report from the error chain and the replayer attaches
// it to the failing test's FailureInfo.UnmatchedCalls.
package mismatch

import (
	"fmt"
	"net"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"go.keploy.io/server/v3/pkg/matcher"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// Canonical protocol names. Parsers must use these so report consumers can
// group and filter without normalizing strings.
const (
	ProtocolHTTP     = "HTTP"
	ProtocolHTTP2    = "HTTP/2"
	ProtocolGRPC     = "gRPC"
	ProtocolMySQL    = "MySQL"
	ProtocolPostgres = "PostgreSQL"
	ProtocolMongo    = "MongoDB"
	ProtocolRedis    = "Redis"
	ProtocolGeneric  = "Generic"
	ProtocolDNS      = "DNS"
	ProtocolHBase    = "HBase"
	ProtocolPulsar   = "Pulsar"
)

// maxFieldDiffValueLen bounds recorded/live values embedded in reports so a
// large payload can't bloat the report yaml or the CLI table.
const maxFieldDiffValueLen = 256

// maxFieldDiffs bounds how many per-field diffs a single report carries, and
// so how many fields its hint names.
const maxFieldDiffs = 25

// Builder assembles a models.MockMismatchReport with consistent rendering.
type Builder struct {
	report models.MockMismatchReport
	// comparedDests is the set of upstreams the mocks this miss was actually
	// compared against were recorded for — evidence for the destination-scope
	// check in Build(). nil means "the question is undecidable" — see
	// WithComparedDestinations.
	comparedDests []string
	// diffs are every field diff WithClosest was given, each path and kind
	// once (foldDiffs). The report carries at most maxFieldDiffs of them;
	// the hint reads them all, so that a difference past the cut still
	// decides what it offers.
	diffs []models.MockFieldDiff
	// noise is what the parser knows of keploy.yml's noise for the
	// report's fields (WithGuarded, WithoutNoise).
	noise noiseFacts
}

// noiseFacts is what a parser knows of keploy.yml's noise for a report's
// fields that its field diffs do not carry.
type noiseFacts struct {
	// guarded: fields keploy.yml's noise declares with patterns that a
	// side's value is outside of, each once, in the order first named,
	// every naming of it folded in (WithPatternMisses); guardedAt indexes
	// them by field (fieldKey).
	guarded   []PatternMiss
	guardedAt map[string]int
	// unreachable: fields no keploy.yml noise is offered for, each by its
	// report path.
	unreachable []string
}

// miss is the PatternMiss of the field at path, and whether there is one.
func (n noiseFacts) miss(path string) (PatternMiss, bool) {
	i, ok := n.guardedAt[fieldKey(path)]
	if !ok {
		return PatternMiss{}, false
	}
	return n.guarded[i], true
}

// PatternMiss is a field of a miss that keploy.yml's noise lets vary only
// within patterns, and that the matcher compared because a value of a side
// is outside all of them.
type PatternMiss struct {
	// Path is the field's report path, as its diff names it ("body.qty",
	// "header.x-request-id").
	Path string
	// Entry is keploy.yml's entry that holds the patterns, named by the run
	// of whole fields of Path it matches, so in the API's own case
	// ("requestId" for "body.requestId", "qty" for "body.order.qty", "ids"
	// for "body.ids[]"): keploy reads keploy.yml's keys without case, and
	// lowercased, so the key it holds would name a key keploy.yml does not
	// spell so. "" names the entry by Path.
	Entry string
	// Patterns are the entry's patterns, as keploy.yml writes them.
	Patterns []string
	// Live and Recorded name the values of that side that no pattern
	// matches, by shape (ValueShapes). Both name values where neither
	// side's is in the patterns: widened to match one side's alone, they
	// would leave the call a miss.
	Live, Recorded ValueShapes
	// Container: a side's value is an array or an object. A pattern
	// matches a single value, never a container, so no widening of the
	// patterns makes the call match: the hint offers recording again.
	Container bool
}

// Fold names o's values too, as values of m's field: o is another naming
// of it (an array's elements share a path) or another field m's entry
// guards. m keeps its own path, entry and patterns.
func (m *PatternMiss) Fold(o PatternMiss) {
	m.Live.merge(o.Live)
	m.Recorded.merge(o.Recorded)
	m.Container = m.Container || o.Container
}

// Why is why the matcher compares m's field, named field as a reason names
// it: keploy.yml's noise lets it vary only within m's patterns, and m's
// values of each side are outside them, each side's by its few shapes
// (ValueShapes.String), so a reason is as small as ValueShapes keeps each
// side however many values m names.
func (m PatternMiss) Why(field string) string {
	sides := strings.Join(m.sides(), " and ")
	if sides == "" {
		sides = "a value"
	}
	return matcher.PatternValuesWhy(field, m.Patterns, sides, m.Live.Len()+m.Recorded.Len() > 1)
}

// sides names m's values of each side that has some (ValueShapes.of), live
// first.
func (m PatternMiss) sides() []string {
	return slices.DeleteFunc([]string{m.Live.of("live"), m.Recorded.of("recorded")}, func(s string) bool { return s == "" })
}

// maxNamedShapes is how many distinct shapes of one side's values a hint
// names at most; it counts the values past them.
const maxNamedShapes = 3

// ValueShapes names values of one side of a field that no pattern matches,
// each by its kind and length (matcher.StringShape, matcher.NumberShape),
// never its text: a value a noise pattern guards may be a secret. It is
// bounded by construction: it holds each distinct shape once, the first
// maxNamedShapes of them, each with how many values have it, and counts
// the values past them, so the hint that names it stays small however many
// values miss. The zero value names none.
type ValueShapes struct {
	shapes [maxNamedShapes]string
	counts [maxNamedShapes]int
	// named: shapes[:named] are named, each with counts[i] values.
	named int
	// more: values counted past the named shapes' (Add, merge).
	more int
}

// ShapesOf is the ValueShapes of one value of each of shapes.
func ShapesOf(shapes ...string) ValueShapes {
	var s ValueShapes
	for _, shape := range shapes {
		s.Add(shape)
	}
	return s
}

// Add names one more value, of shape.
func (s *ValueShapes) Add(shape string) { s.add(shape, 1) }

// add names n more values, of shape.
func (s *ValueShapes) add(shape string, n int) {
	for i := 0; i < s.named; i++ {
		if s.shapes[i] == shape {
			s.counts[i] += n
			return
		}
	}
	if s.named < maxNamedShapes {
		s.shapes[s.named], s.counts[s.named] = shape, n
		s.named++
		return
	}
	s.more += n
}

// merge names o's values too.
func (s *ValueShapes) merge(o ValueShapes) {
	for i := 0; i < o.named; i++ {
		s.add(o.shapes[i], o.counts[i])
	}
	s.more += o.more
}

// Len is how many values s names.
func (s ValueShapes) Len() int {
	n := s.more
	for i := 0; i < s.named; i++ {
		n += s.counts[i]
	}
	return n
}

// Shapes are the distinct shapes s names, in the order first named: at
// most maxNamedShapes.
func (s ValueShapes) Shapes() []string { return slices.Clone(s.shapes[:s.named]) }

// String is how a hint names s's values: each named shape once, then how
// many values there are past them ("a string of 4 bytes, a string of 5
// bytes and 2 more").
func (s ValueShapes) String() string {
	out := strings.Join(s.shapes[:s.named], ", ")
	if s.more > 0 {
		out += " and " + strconv.Itoa(s.more) + " more"
	}
	return out
}

// of is how a hint or a reason names s's values as those of side (live,
// recorded): "the live value (a string of 4 bytes)", "the recorded values
// (a string of 2 bytes and 3 more)"; "" for none.
func (s ValueShapes) of(side string) string {
	switch s.Len() {
	case 0:
		return ""
	case 1:
		return "the " + side + " value (" + s.String() + ")"
	}
	return "the " + side + " values (" + s.String() + ")"
}

// NewReport starts a report for one missed call. actualSummary should be the
// shortest string that identifies the live call ("POST /orders", "SELECT
// (prepared stmt 12)", "find users").
func NewReport(protocol, actualSummary string) *Builder {
	return &Builder{report: models.MockMismatchReport{
		At:            time.Now(),
		Protocol:      protocol,
		ActualSummary: actualSummary,
	}}
}

// WithDestination records the outgoing call's destination/domain (e.g. the
// HTTP Host authority, or a "host:port") so the report and its log line say
// WHICH upstream missed — method+path alone collide across hosts when an app
// calls several services.
func (b *Builder) WithDestination(dest string) *Builder {
	b.report.Destination = dest
	return b
}

// WithComparedDestinations supplies the upstream authority of every mock this
// miss was actually compared against, so Build() can tell a call whose
// upstream is absent from that set apart from a drifted request.
//
// WHY THIS EXISTS — the sibling-container scope asymmetry (KUBERNETES; the
// check itself is mode-neutral, and so is the guidance it selects — see
// models.OutOfScopeDestinationCauses, which names the pod model as a
// Kubernetes specific because native and docker runs have no pod):
//
// keploy's RECORD path arms exactly ONE container: the one the user named in
// the record request (RecordRequest.Container). The DS agent honours it
// precisely — sibling containers in the same pod log "matchedSession": false
// and nothing they send is ever captured. The REPLAY path has no such
// narrowing: the sandbox pod's keploy-agent intercepts the WHOLE pod network
// namespace, so a sibling container's egress IS intercepted, finds no mock
// (there was never one to find), and gets reported to the user as a mock
// mismatch.
//
// That is the shape the check was built from, not the limit of what it
// catches: a destination absent from the compared set is equally the
// signature of endpoint/config drift between the recording and replay
// environments, and of a per-test mock window that excluded the very mock
// this call needed — both of which happen in every execution mode.
//
// Before this check, such a miss fell through to the generic "Request
// structure changed since recording. Re-record the test set..." hint, which
// is false twice over: nothing changed, and the closest mock it pointed at
// was on a different upstream entirely. In one reported recording that was 23 of
// 28 unmatched calls, each diffed against a mock for another host — noise
// that buried the 5 misses that were genuinely the application's and cost two
// engineers real debugging time.
//
// WHAT THE VERDICT MAY AND MAY NOT SAY. It speaks for the compared set and
// nothing wider. It must never be rendered as "this destination was never
// recorded": that is a claim about the whole recording, and local state
// cannot support it — protocols that consume mocks on match (HTTP deletes
// per-test mocks via DeleteFilteredMock, and the agent then strips them from
// every later pool) lose a host from their pools the moment its last mock is
// served, so "absent from what survives" and "never recorded" are different
// facts. Every attempt to assert the stronger one produced a false claim
// about a real upstream. The weaker claim is always true, and it still
// removes both things that misled the reader: the "re-record, the request
// structure changed" advice, and leading with a diff against another host.
//
// The interception itself is deliberately NOT changed here: narrowing it
// needs cgroup-scoped BPF (the excluded_pids map is keyed by ROOT-namespace
// TGIDs, which the replay agent — living in the pod's PID namespace — cannot
// enumerate from /proc), and it is harmless anyway, because k8s-proxy
// attaches a deny-all-egress NetworkPolicy to every replay pod, so the
// sibling's call cannot reach its upstream either way. Only the REPORT was
// harmful, so only the REPORT is fixed. This is diagnostic-only by
// construction: it can change what a miss SAYS, never whether a call matches.
//
// Callers must pass nil whenever they cannot read a destination off EVERY
// mock they compared — nil means undecidable, and an undecidable check leaves
// today's message exactly as it was. One unreadable mock is enough: it could
// be the very mock that targeted the live call.
//
// Protocol reach: the mechanism is protocol-agnostic — any parser that can
// name the upstreams of its compared mocks may feed it — but HTTP is the only
// caller today, because it is the only protocol whose recorded mocks carry a
// destination at all (Mongo/MySQL/Postgres/Generic store wire payloads with
// no authority in them). Every other protocol therefore leaves the verdict at
// models.DestinationScopeUnknown, and their reports read exactly as they did
// before this check existed.
func (b *Builder) WithComparedDestinations(dests []string) *Builder {
	b.comparedDests = dests
	return b
}

// WithPhase records how far the match cascade got and how many candidate
// mocks were considered. Use the models.MatchPhase* constants.
func (b *Builder) WithPhase(phase string, candidateCount int) *Builder {
	b.report.MatchPhase = phase
	b.report.CandidateCount = candidateCount
	return b
}

// WithClosest attaches the nearest candidate and its field-level diffs,
// each path and kind once (foldDiffs): the places of one field a parser
// names alike (an array's elements, by the array's shape) are one diff, so
// they count once against the cut and the hint names the field once. The
// report carries at most maxFieldDiffs of them: when it cuts them, those that
// can be why the call missed (causes) first, each in its order. The hint
// reads them all.
func (b *Builder) WithClosest(mockName string, diffs []models.MockFieldDiff) *Builder {
	b.report.ClosestMock = mockName
	diffs = foldDiffs(diffs)
	b.diffs = diffs
	if len(diffs) > maxFieldDiffs {
		// A copy, so the report does not keep every diff alive.
		diffs = slices.Clone(causesFirst(b.report.Protocol, diffs)[:maxFieldDiffs])
	}
	b.report.FieldDiffs = diffs
	return b
}

// foldDiffs is diffs with each path and kind once, at its first place, in
// diffs' order: a diff of a path and kind an earlier one has is dropped.
func foldDiffs(diffs []models.MockFieldDiff) []models.MockFieldDiff {
	type name struct {
		path string
		kind models.MockFieldDiffKind
	}
	seen := make(map[name]bool, len(diffs))
	out := diffs[:0:0]
	for _, d := range diffs {
		if n := (name{d.Path, d.Kind}); !seen[n] {
			seen[n] = true
			out = append(out, d)
		}
	}
	return out
}

// WithGuarded names the fields of the diffs that keploy.yml's noise already
// declares with patterns, a side's value outside them (a header by its name
// in any case), when the parser cannot say which patterns and which side:
// the hint tells the user to widen the entry's patterns so they match the
// recorded and the live value, never to add the field or to empty its
// entry. A parser that can say uses WithPatternMisses.
func (b *Builder) WithGuarded(paths ...string) *Builder {
	for _, p := range paths {
		b.WithPatternMisses(PatternMiss{Path: p})
	}
	return b
}

// WithPatternMisses names the fields of the diffs that keploy.yml's noise
// already declares with patterns, each with its entry, those patterns and
// the values of each side outside them (a header by its name in any case).
// The hint names the entry, the patterns and those values' shapes, and
// tells the user to widen the patterns to match each: an entry emptied to
// a list with no patterns would stop the field being compared at all. A
// field named more than once (an array's elements share a path) is held
// once, under its first naming's entry and patterns, and is to match the
// values of every naming: the facts the builder holds, and the hint, stay
// as small as ValueShapes keeps each side, however many namings there are.
func (b *Builder) WithPatternMisses(misses ...PatternMiss) *Builder {
	for _, m := range misses {
		k := fieldKey(m.Path)
		if i, seen := b.noise.guardedAt[k]; seen {
			b.noise.guarded[i].Fold(m)
			continue
		}
		if b.noise.guardedAt == nil {
			b.noise.guardedAt = map[string]int{}
		}
		b.noise.guardedAt[k] = len(b.noise.guarded)
		b.noise.guarded = append(b.noise.guarded, m)
	}
	return b
}

// WithoutNoise names the fields of the diffs that no keploy.yml noise is
// offered for (a header by its name in any case): the parser compares them
// whatever the noise says, or declaring them would stop a comparison keploy
// keeps. A miss that differs in one of them is offered only recording again.
func (b *Builder) WithoutNoise(paths ...string) *Builder {
	b.noise.unreachable = append(b.noise.unreachable, paths...)
	return b
}

// WithDiff sets an explicit human-readable diff, overriding the rendered
// FieldDiffs summary. Prefer WithClosest + field diffs; use this only for
// protocols where field decomposition is impossible (e.g. opaque binary).
func (b *Builder) WithDiff(diff string) *Builder {
	b.report.Diff = diff
	return b
}

// WithNextSteps sets the remediation hint shown to the user.
func (b *Builder) WithNextSteps(steps string) *Builder {
	b.report.NextSteps = steps
	return b
}

// WithRenderedRequests attaches the FULL rendered requests for the CLI
// side-by-side whole-mock diff: mockReq is the closest recorded mock's
// request, receivedReq is the live request the app sent. Both must already be
// human-rendered (one field per line, JSON pretty-printed, sensitive values
// redacted) by the parser. FieldDiffs remain the machine-readable companion;
// these drive the highlighted whole-request view.
func (b *Builder) WithRenderedRequests(mockReq, receivedReq string) *Builder {
	b.report.ClosestMockReq = mockReq
	b.report.ReceivedReq = receivedReq
	return b
}

// Build renders the Diff string from FieldDiffs (when not explicitly set)
// and returns the finished report.
func (b *Builder) Build() *models.MockMismatchReport {
	r := b.report
	// Destination scope is decided BEFORE the next-steps default so the
	// verdict and the guidance can never disagree.
	//
	// It is written to its OWN field, never over MatchPhase. The phase is the
	// cascade's stopping point (no_schema_candidates / body_mismatch /
	// strict_noise_reject), which is what tells a reader how far matching got
	// — triage information that an earlier cut of this code destroyed by
	// overwriting it, leaving the CLI printing "[match stopped at:
	// destination_not_recorded]", a phase that never ran.
	//
	// MatchPhaseNoMocks is left unscored: "there were no mocks at all" has
	// its own accurate message, and there is no compared set to speak about,
	// so the question is not even asked.
	if r.MatchPhase != models.MatchPhaseNoMocks {
		r.DestinationScope = destinationScope(r.Destination, b.comparedDests)
	}
	if r.Diff == "" {
		r.Diff = RenderFieldDiffs(r.FieldDiffs)
	}
	if r.NextSteps == "" {
		r.NextSteps = defaultNextSteps(&r, b.diffs, b.noise)
	}
	return &r
}

// RenderFieldDiffs renders field diffs as a compact single-string summary for
// surfaces that can't show structured data (legacy table cells, logs).
func RenderFieldDiffs(diffs []models.MockFieldDiff) string {
	if len(diffs) == 0 {
		return ""
	}
	parts := make([]string, 0, len(diffs))
	for _, d := range diffs {
		switch d.Kind {
		case models.DiffKindMissingInLive:
			parts = append(parts, fmt.Sprintf("%s: recorded %q, absent in live call", d.Path, d.Expected))
		case models.DiffKindMissingInMock:
			parts = append(parts, fmt.Sprintf("%s: live %q, absent in recording", d.Path, d.Actual))
		case models.DiffKindTypeChanged:
			parts = append(parts, fmt.Sprintf("%s: type changed (recorded %s, live %s)", d.Path, d.Expected, d.Actual))
		default:
			if d.Expected == "" && d.Actual == "" {
				// A diff that carries no values (a form field, an array
				// element under the strict comparison) names the field
				// alone: two empty values would read as values it had.
				parts = append(parts, fmt.Sprintf("%s: differs (values not shown)", d.Path))
				continue
			}
			parts = append(parts, fmt.Sprintf("%s: recorded %q, live %q", d.Path, d.Expected, d.Actual))
		}
	}
	return strings.Join(parts, "; ")
}

// mockMode is on when the agent serves a `keploy mock record|replay` session.
// Such a run is recorded again with `keploy mock record`, not `keploy record`,
// so the hints below name that command; every other run keeps the old text.
//
// It is one switch for the whole process, as the agent runs in one mode. Tests
// in this package that set it restore it with t.Cleanup and must not call
// t.Parallel: a parallel test would read the other's mode and fail on hint
// text it never asked for.
var mockMode atomic.Bool

// SetMockMode is called by the agent at setup with its --mock-mode flag.
func SetMockMode(on bool) { mockMode.Store(on) }

// MockMode reports whether the agent serves a `keploy mock` session. A
// protocol that writes its own hint with WithNextSteps uses it to name how a
// mock run is recorded again (a mock set, RecordCommand) and to leave out
// advice a mock run cannot act on, such as keploy test's
// --update-test-mapping. keploy.yml's test.globalNoise is not such advice:
// `keploy mock replay` hands it to the agent as keploy test does (its
// MockOutgoing options in pkg/service/mock/replay.go).
func MockMode() bool { return mockMode.Load() }

// RecordCommand is the command that records this run again. A protocol that
// writes its own hint with WithNextSteps names the command through it, so a
// mock run never points at `keploy record`.
func RecordCommand() string {
	if mockMode.Load() {
		return "keploy mock record"
	}
	return "keploy record"
}

// defaultNextSteps derives an actionable hint from the report shape. The
// wording deliberately never references commands that don't exist; the two
// real remedies are keploy.yml noise (for drifted values and headers that a
// noise bucket reaches: matchingNoise) and re-recording (for everything, and
// alone for anything else). Only the diffs that can be why the call missed
// are read (causes).
//
// It decides from every field diff (all), not only those the report carries,
// so a difference past the report's cut still keeps noise that would leave
// the call a miss out of the hint. It names only the fields the report
// carries (named), as the miss's sanitizer shapes those names
// (models.UnmatchedCall.SanitizedNextStep), and counts the rest (more). So
// it offers noise only when the report carries every cause: a remedy it
// could not name whole would leave the call a miss.
func defaultNextSteps(r *models.MockMismatchReport, all []models.MockFieldDiff, n noiseFacts) string {
	diffs := causes(r.Protocol, all)
	named := causes(r.Protocol, r.FieldDiffs)
	more := len(diffs) - len(named)
	onlyValueDrift := len(diffs) > 0
	for _, d := range diffs {
		if d.Kind != models.DiffKindValueChanged {
			onlyValueDrift = false
			break
		}
	}
	var noise string
	var noiseOK bool
	if more == 0 {
		// The report carries every cause: named is diffs.
		noise, noiseOK = matchingNoise(r.Protocol, named, n)
	}
	switch {
	case r.MatchPhase == models.MatchPhaseNoMocks:
		if mockMode.Load() {
			return "No recorded mocks were available to match against for this protocol in the selected mock set. Re-record the mock set with '" + RecordCommand() + "'."
		}
		return "No recorded mocks were available to match against for this protocol in the selected test set. Re-record the test set with '" + RecordCommand() + "'."
	case r.DestinationScope == models.DestinationScopeNotInComparedSet:
		// Ahead of the value-drift branch on purpose: when nothing in the
		// compared set targets this upstream, the closest mock belongs to a
		// DIFFERENT one, so its field diffs describe a comparison that was
		// never meaningful. Telling the user to noise those fields would be a
		// second misdirection stacked on the first.
		scope := "recorded mock"
		if r.Protocol != "" {
			scope = "recorded " + r.Protocol + " mock"
		}
		// ONE sentence — the only part of this guidance that differs between
		// two out-of-scope misses is WHICH upstream they went to. The likely
		// causes and the caveat are identical for every one of them and live
		// in models.OutOfScopeDestinationCauses, which renderers emit once
		// per test. An out-of-scope container produces one miss per outgoing
		// call it makes (23 of 28 unmatched calls in the recording this was
		// built from); the paragraph that used to be inlined here was ~1 KB,
		// repeated per miss, in the report AND in the agent's per-miss
		// next_step log field.
		lead := fmt.Sprintf("No %s in the compared set targets %s.", scope, r.Destination)
		if r.ClosestMock != "" {
			// Only claimed when a closest mock was actually picked; the
			// no-candidate branch of the HTTP builder renders no diff at all.
			lead += " The closest mock is on a different upstream, so its differences are not evidence about this call."
		}
		return lead
	case onlyValueDrift:
		paths := make([]string, 0, len(named))
		for _, d := range named {
			paths = append(paths, d.Path)
		}
		drifted := strings.Join(paths, ", ")
		if more > 0 {
			drifted += fmt.Sprintf(", and %d more", more)
		}
		// keploy test and keploy mock replay alike hand the agent
		// keploy.yml's test.globalNoise, so the noise is a remedy in both
		// where it reaches every drifted field; a mock run is only recorded
		// again another way (rerecordRemedy).
		if noiseOK {
			return fmt.Sprintf("Only values drifted (%s). If these are dynamic (timestamps, ids, tokens), %s; otherwise %s.", drifted, noise, rerecordRemedy(r))
		}
		if held := containerMisses(named, n); len(held) > 0 {
			// Widening the patterns would leave the call a miss: say why
			// none is offered, rather than leave the user to try.
			return fmt.Sprintf("Only values drifted (%s). keploy.yml's noise lets %s, and a pattern matches a single value, never an array or an object. If the change is expected, %s.", drifted, strings.Join(held, " and "), rerecordRemedy(r))
		}
		if unread := unreadFields(r.Protocol, named); len(unread) > 0 {
			// Say why no noise is offered where the protocol's matcher
			// reads none: an entry the user adds there would change nothing.
			return fmt.Sprintf("Only values drifted (%s). keploy's %s mock matcher reads no keploy.yml noise for %s. If the change is expected, %s.", drifted, r.Protocol, strings.Join(unread, ", "), rerecordRemedy(r))
		}
		return fmt.Sprintf("Only values drifted (%s). If the change is expected, %s.", drifted, rerecordRemedy(r))
	case noiseOK:
		// Not only value drift, and still in the noise's reach: an HTTP call
		// that does not carry recorded headers, or (under the proven switch)
		// carries headers its recording does not, beside any value drift the
		// noise reaches (matchingNoise reaches no other missing field). The
		// HTTP matcher compares those headers unless keploy.yml declares them
		// noise, so that declaration makes the call match.
		var missing, carried, drifted []string
		for _, d := range named {
			switch d.Kind {
			case models.DiffKindMissingInLive:
				missing = append(missing, d.Path)
			case models.DiffKindMissingInMock:
				carried = append(carried, d.Path)
			default:
				drifted = append(drifted, d.Path)
			}
		}
		var parts []string
		if len(missing) > 0 {
			parts = append(parts, "Recorded headers are missing from this call ("+strings.Join(missing, ", ")+")")
		}
		if len(carried) > 0 {
			lead := "This call carries headers its recording does not"
			if len(parts) > 0 {
				lead = "it carries headers its recording does not"
			}
			parts = append(parts, lead+" ("+strings.Join(carried, ", ")+")")
		}
		cond := "they are optional"
		if len(drifted) > 0 {
			parts = append(parts, "values drifted ("+strings.Join(drifted, ", ")+")")
			cond = "these are optional or dynamic (timestamps, ids, tokens)"
		}
		lead := parts[len(parts)-1]
		if len(parts) > 1 {
			lead = strings.Join(parts[:len(parts)-1], ", ") + " and " + lead
		}
		return fmt.Sprintf("%s. If %s, %s; otherwise %s.", lead, cond, noise, rerecordRemedy(r))
	default:
		// Not --update-test-mapping alone: it updates only a passing test's
		// mapping, so a test this miss failed keeps its recorded one. With
		// --disable-mapping the test is served its recorded window, and mapped
		// if it passes (both are `keploy test` flags a mock run does not have).
		if mockMode.Load() {
			return "Request structure changed since recording. Re-record the mock set with '" + RecordCommand() + "'."
		}
		return "Request structure changed since recording. Re-record the test set with '" + RecordCommand() + "', or, if you edited its mocks, remap it with --disable-mapping --update-test-mapping."
	}
}

// causes are the field diffs, of a protocol's miss, that can be why its call
// missed. Without the proven switch the HTTP matcher lets a call carry
// headers its recording does not (HeadersContainKeys), so such a header is
// reported and is no cause; under the switch every header is compared except
// host, content-length and keploy* headers and those keploy's own or
// keploy.yml's header noise names (the strict comparison's headerDiffs), so
// one only the call carries is a cause.
func causes(protocol string, diffs []models.MockFieldDiff) []models.MockFieldDiff {
	if !carriedHeadersPass(protocol) {
		return diffs
	}
	return slices.DeleteFunc(slices.Clone(diffs), carriedHeader)
}

// causesFirst is diffs with its causes ahead of the rest, each in its order,
// so a report cut to maxFieldDiffs names why its call missed.
func causesFirst(protocol string, diffs []models.MockFieldDiff) []models.MockFieldDiff {
	if !carriedHeadersPass(protocol) {
		return diffs
	}
	out := make([]models.MockFieldDiff, 0, len(diffs))
	var carried []models.MockFieldDiff
	for _, d := range diffs {
		if carriedHeader(d) {
			carried = append(carried, d)
		} else {
			out = append(out, d)
		}
	}
	return append(out, carried...)
}

// carriedHeadersPass reports whether a protocol's matcher lets a call carry
// headers its recording does not: HTTP's, without the proven switch.
func carriedHeadersPass(protocol string) bool {
	return protocol == ProtocolHTTP && !provenswitch.On()
}

// carriedHeader reports whether d is a header only the live call carries.
func carriedHeader(d models.MockFieldDiff) bool {
	return d.Kind == models.DiffKindMissingInMock && strings.HasPrefix(d.Path, "header.")
}

// matchingNoise is the keploy.yml noise that makes a call that differs in
// diffs match, as the remedy the hint names (its verb included) by the
// bucket the matcher reads for each field, and whether there is any.
//   - A request-body field's value drift is named under the bucket the
//     protocol's matcher reads for it (requestBodyNoiseKey) by its
//     root-relative key: the "body." prefix is the report's. A protocol
//     whose matcher reads none for it is offered no noise.
//   - On HTTP, a header whose value drifted, that was recorded and the call
//     does not carry, or that the call carries and its recording does not
//     (a cause only under the proven switch: causes), is named under header
//     by its lowercased name, once. Nothing else is offered there. A
//     method, host or container drift has no noise remedy. A path or query
//     drift has none named here: the url bucket the matcher reads holds
//     regexes of the part of a path or query value that changes, which a
//     field diff does not say, so it is offered only recording again. No
//     noise is offered for an Authorization of any scheme, or a part of a
//     SigV4 one ("header.authorization.region"): authorizationHeader. Nor
//     for a dotted header name with a part that may be a value
//     (models.FieldShape writes it otherwise): the miss's sanitizer would
//     not shape it in the noise's example, and a name that holds a value
//     differs call by call.
//   - Outside HTTP, nothing but a request-body field is in a bucket's
//     reach: no other protocol's matcher reads keploy.yml's noise for any
//     other field but gRPC's and HTTP/2's for their headers, and neither
//     reports a header's diff.
//
// A field the parser named WithoutNoise is in no bucket's reach. A field it
// named WithGuarded or WithPatternMisses is already declared with patterns,
// so its entry's patterns are to be widened to match every value outside
// them, of each side (widenRemedy), never the field added or its entry
// emptied: an entry with no patterns would stop the field being compared at
// all. A pattern matches no absent value and no array or object, so such a
// field missing on a side, or holding a container, is in no bucket's
// reach. A call with any field out of every bucket's reach is offered no
// noise: noise would leave that field's difference, so recording again is
// the only remedy that makes the call match.
func matchingNoise(protocol string, diffs []models.MockFieldDiff, n noiseFacts) (string, bool) {
	isHTTP := protocol == ProtocolHTTP
	bodyKey := requestBodyNoiseKey(protocol)
	unreachable := fieldSet(n.unreachable)
	type bucket struct {
		add     []string
		added   map[string]bool
		widen   []*PatternMiss // by the entry each names, in the diffs' order
		widened map[string]*PatternMiss
	}
	body := bucket{added: map[string]bool{}, widened: map[string]*PatternMiss{}}
	header := bucket{added: map[string]bool{}, widened: map[string]*PatternMiss{}}
	folded := map[string]bool{}
	for _, d := range diffs {
		if unreachable[fieldKey(d.Path)] {
			return "", false
		}
		name, isHeader := strings.CutPrefix(d.Path, "header.")
		key, isBody := strings.CutPrefix(d.Path, "body.")
		var b *bucket
		switch {
		case isBody && bodyKey != "" && d.Kind == models.DiffKindValueChanged:
			b = &body
		case isHTTP && isHeader && !authorizationHeader(name) && models.FieldShape(d.Path) == d.Path &&
			(d.Kind == models.DiffKindValueChanged || d.Kind == models.DiffKindMissingInLive || d.Kind == models.DiffKindMissingInMock):
			key, b = strings.ToLower(name), &header
		default:
			return "", false
		}
		g, isGuarded := n.miss(d.Path)
		if isGuarded && (d.Kind != models.DiffKindValueChanged || g.Container) {
			return "", false
		}
		switch {
		case !isGuarded:
			if !b.added[key] {
				b.added[key], b.add = true, append(b.add, key)
			}
		case folded[fieldKey(d.Path)]:
			// The facts hold every value of the field: a field the diffs
			// name twice (an array's elements) is folded in once.
		default:
			folded[fieldKey(d.Path)] = true
			if g.Entry != "" {
				key = g.Entry
			}
			// keploy.yml's keys are read without case, so an entry two
			// fields name in two cases (id, ID) is one entry: it is named
			// once, by its first naming's run.
			if w := b.widened[strings.ToLower(key)]; w != nil {
				// Another field the same entry guards: its patterns are to
				// match that field's values too.
				w.Fold(g)
				continue
			}
			w := g
			b.widened[strings.ToLower(key)], b.widen = &w, append(b.widen, &w)
		}
	}
	var add, remedy []string
	switch {
	case len(body.add) == 0:
	case bodyKey == "body":
		// Pulsar's matcher reads its message payload's noise from the
		// bucket keploy test excuses HTTP response-body fields by: say so,
		// as a field added there is no longer asserted in a response.
		add = append(add, fmt.Sprintf("the message payload fields under test.globalNoise.global.body with root-relative keys (e.g. body: {%s: []}), the bucket that also stops keploy test asserting those fields in HTTP response bodies", strings.Join(body.add, ": [], ")))
	default:
		add = append(add, fmt.Sprintf("the request-body fields under test.globalNoise.global.%[1]s with root-relative keys (e.g. %[1]s: {%[2]s: []})", bodyKey, strings.Join(body.add, ": [], ")))
	}
	if len(header.add) > 0 {
		add = append(add, fmt.Sprintf("the headers under test.globalNoise.global.header (e.g. header: {%s: []})", strings.Join(header.add, ": [], ")))
	}
	if len(add) > 0 {
		remedy = append(remedy, "add "+strings.Join(add, " and "))
	}
	for _, w := range []struct {
		key     string
		entries []*PatternMiss
		header  bool
	}{{bodyKey, body.widen, false}, {"header", header.widen, true}} {
		if len(w.entries) == 0 {
			continue
		}
		named := make([]string, len(w.entries))
		for i, g := range w.entries {
			named[i] = widenRemedy(g, w.header)
		}
		remedy = append(remedy, fmt.Sprintf("widen the patterns of keploy.yml's test.globalNoise %s entries for %s", w.key, strings.Join(named, " and for ")))
	}
	return strings.Join(remedy, " and "), len(remedy) > 0
}

// widenRemedy is how the hint names the entry whose patterns g says miss
// values, and the values they are to match: the entry (a header's by its
// lowercased name, as the HTTP matcher reads it), its patterns and the
// shapes of each side's values (ValueShapes, so a few) where the parser
// named them (WithPatternMisses), else both values (WithGuarded).
func widenRemedy(g *PatternMiss, header bool) string {
	key := g.Entry
	if key == "" {
		key = strings.TrimPrefix(g.Path, "body.")
		if header {
			key = strings.ToLower(strings.TrimPrefix(g.Path, "header."))
		}
	}
	values := g.Live.Len() + g.Recorded.Len()
	if len(g.Patterns) == 0 || values == 0 {
		return key + " so they match the recorded and the live value"
	}
	verb := "one matches"
	if values > 1 {
		verb = "they match"
	}
	return fmt.Sprintf("%s (%s) so %s %s", key, quotedPatterns(g.Patterns), verb, strings.Join(g.sides(), " and "))
}

// quotedPatterns is patterns as a hint names them, each quoted.
func quotedPatterns(patterns []string) string {
	quoted := make([]string, len(patterns))
	for i, p := range patterns {
		quoted[i] = strconv.Quote(p)
	}
	return strings.Join(quoted, ", ")
}

// containerMisses are, of diffs, the fields keploy.yml's noise guards with
// patterns whose value on a side is an array or an object, each once, named
// with its patterns, for the hint to say why it offers no noise for them.
func containerMisses(diffs []models.MockFieldDiff, n noiseFacts) []string {
	var out []string
	named := map[string]bool{}
	for _, d := range diffs {
		if g, _ := n.miss(d.Path); g.Container && len(g.Patterns) > 0 && !named[fieldKey(d.Path)] {
			named[fieldKey(d.Path)] = true
			out = append(out, fmt.Sprintf("%s vary only within %s", d.Path, quotedPatterns(g.Patterns)))
		}
	}
	return out
}

// requestBodyNoiseKey is the keploy.yml test.globalNoise bucket a
// protocol's mock matcher reads for a request-body field, "" for a protocol
// whose matcher reads none: requestbody for the HTTP, MySQL, MongoDB and
// HBase matchers, and body for Pulsar's message payload. No other
// protocol's matcher reads keploy.yml's noise for a body field.
func requestBodyNoiseKey(protocol string) string {
	switch protocol {
	case ProtocolHTTP, ProtocolMySQL, ProtocolMongo, ProtocolHBase:
		return "requestbody"
	case ProtocolPulsar:
		return "body"
	}
	return ""
}

// unreadFields are the paths of diffs, of a call of protocol, that its mock
// matcher reads no keploy.yml noise for (matchingNoise): outside HTTP, the
// request body or a field of it where requestBodyNoiseKey is "", and any
// field that is neither of those nor a header. A header is not named:
// gRPC's and HTTP/2's matchers read header noise.
func unreadFields(protocol string, diffs []models.MockFieldDiff) []string {
	if protocol == "" || protocol == ProtocolHTTP {
		return nil
	}
	bodyRead := requestBodyNoiseKey(protocol) != ""
	var out []string
	for _, d := range diffs {
		isBody := d.Path == "body" || strings.HasPrefix(d.Path, "body.")
		if !strings.HasPrefix(d.Path, "header.") && (!isBody || !bodyRead) {
			out = append(out, d.Path)
		}
	}
	return out
}

// authorizationHeader reports whether a header's report name (its path less
// "header.") is the Authorization header, or a part of a SigV4 one
// ("authorization.region"), in any case. The hint offers no keploy.yml noise
// for it, of any scheme. Under the proven switch the parts of a recorded
// SigV4 Authorization are compared so that it may differ only in its access
// key id, date and signature. keploy.yml's header noise, global or a test
// set's (test.globalNoise.test-sets), applies to every recording it reaches,
// so declaring authorization, even to excuse a Bearer token that drifts,
// would stop that comparison for each signed recording there. Nothing the
// hint can read says that none is signed: the mock store holds only what the
// replay sends it, less what it has served, and is never told whether that
// is the whole test set. One keploy.yml serves both modes, so neither
// offers it.
func authorizationHeader(name string) bool {
	n := strings.ToLower(name)
	return n == "authorization" || strings.HasPrefix(n, "authorization.")
}

// fieldSet is paths as a set of fieldKey.
func fieldSet(paths []string) map[string]bool {
	set := make(map[string]bool, len(paths))
	for _, p := range paths {
		set[fieldKey(p)] = true
	}
	return set
}

// fieldKey is how a field path is named in a fieldSet: a header by its
// lowercased name, as the HTTP matcher reads header names in any case, and
// any other field as it is.
func fieldKey(path string) string {
	if strings.HasPrefix(path, "header.") {
		return strings.ToLower(path)
	}
	return path
}

// rerecordRemedy is how the hint says to record r's call again: a mock run
// is recorded again as a mock set, and only the HTTP parser acts on
// --on-miss, only when it can reach the real dependency.
func rerecordRemedy(r *models.MockMismatchReport) string {
	if !mockMode.Load() {
		return "re-record with '" + RecordCommand() + "'"
	}
	rerecord := "re-record the mock set with '" + RecordCommand() + "'"
	if r.Protocol == ProtocolHTTP {
		rerecord += ", or, with the dependency reachable, capture the new calls with 'keploy mock replay --on-miss record'"
	}
	return rerecord
}

// JSONBodyDiffs computes field-level diffs between a recorded JSON body and
// the live one, excluding paths in `ignore` (root-relative noise map, e.g.
// learned req_body_noise plus user-configured body noise). Paths come back
// prefixed with "body.".
func JSONBodyDiffs(recordedBody, liveBody string, ignore map[string][]string) []models.MockFieldDiff {
	return matcher.JSONFieldDiffs(recordedBody, liveBody, ignore, "body.", maxFieldDiffValueLen)
}

// HeaderKeyDiffs reports header keys present on one side and missing on the
// other. Values are intentionally not compared — mock matching itself only
// matches on header keys, so reporting value drift here would tell users to
// fix something the matcher never looks at. Keys in `ignore` (lowercased,
// e.g. the auto-noised flaky headers and user header noise) and keploy's own
// headers are skipped.
func HeaderKeyDiffs(recorded map[string]string, live map[string][]string, ignore map[string][]string) []models.MockFieldDiff {
	skip := func(k string) bool {
		lk := strings.ToLower(k)
		if strings.HasPrefix(lk, "keploy") {
			return true
		}
		if ignore != nil {
			if _, ok := ignore[lk]; ok {
				return true
			}
		}
		return false
	}

	liveSet := make(map[string]struct{}, len(live))
	for k := range live {
		liveSet[strings.ToLower(k)] = struct{}{}
	}
	recordedSet := make(map[string]struct{}, len(recorded))
	for k := range recorded {
		recordedSet[strings.ToLower(k)] = struct{}{}
	}

	var out []models.MockFieldDiff
	for k := range recorded {
		if skip(k) {
			continue
		}
		if _, ok := liveSet[strings.ToLower(k)]; !ok {
			out = append(out, models.MockFieldDiff{
				Path: "header." + k,
				Kind: models.DiffKindMissingInLive,
			})
		}
	}
	for k := range live {
		if skip(k) {
			continue
		}
		if _, ok := recordedSet[strings.ToLower(k)]; !ok {
			out = append(out, models.MockFieldDiff{
				Path: "header." + k,
				Kind: models.DiffKindMissingInMock,
			})
		}
	}
	sortDiffs(out)
	return out
}

// QueryParamDiffs reports query parameters whose keys or values differ.
// recorded/live map a key to its values (url.Values semantics).
func QueryParamDiffs(recorded, live map[string][]string) []models.MockFieldDiff {
	var out []models.MockFieldDiff
	for k, rv := range recorded {
		lv, ok := live[k]
		if !ok {
			out = append(out, models.MockFieldDiff{
				Path:     "query." + k,
				Kind:     models.DiffKindMissingInLive,
				Expected: strings.Join(rv, ","),
			})
			continue
		}
		if strings.Join(rv, ",") != strings.Join(lv, ",") {
			out = append(out, models.MockFieldDiff{
				Path:     "query." + k,
				Kind:     models.DiffKindValueChanged,
				Expected: strings.Join(rv, ","),
				Actual:   strings.Join(lv, ","),
			})
		}
	}
	for k, lv := range live {
		if _, ok := recorded[k]; !ok {
			out = append(out, models.MockFieldDiff{
				Path:   "query." + k,
				Kind:   models.DiffKindMissingInMock,
				Actual: strings.Join(lv, ","),
			})
		}
	}
	sortDiffs(out)
	return out
}

func sortDiffs(d []models.MockFieldDiff) {
	sort.Slice(d, func(i, j int) bool { return d[i].Path < d[j].Path })
}

// destinationScope decides whether `dest` is absent from, present in, or
// simply unjudgeable against the destinations of the mocks this miss was
// compared against. It returns one of the models.DestinationScope* constants.
//
// A wrong verdict would be exactly the same class of misdirection this check
// exists to remove, so it is deliberately biased toward silence:
// models.DestinationScopeNotInComparedSet is returned only when all three
// hold:
//
//   - the live destination is known. Parsers pass "" when they cannot tell
//     which upstream a call targeted; "" is undecidable, never "absent".
//   - the compared set is non-empty. An empty set means either no mocks at
//     all (models.MatchPhaseNoMocks owns that case) or mocks whose
//     destination could not be read — neither is evidence of anything.
//   - no compared entry matches on the bare host (port stripped, case and
//     trailing DNS root normalised).
//
// Anything else that leaves the question open returns
// models.DestinationScopeUnknown, which is distinct from
// models.DestinationScopeInComparedSet on purpose: "we checked and the host
// was among the compared mocks" and "we could not check" are different facts,
// and reporting the second as the first is an unchecked negative asserted as
// evidence.
//
// Host vs host:port: recorded HTTP mocks store the authority exactly as the
// app put it in the Host header, which is inconsistent by construction — mock
// sets in the field carry both "192.0.2.10" (default port elided) and
// "192.0.2.30:9090" (explicit) — and the live Host header for that same
// upstream may or may not carry the port. So a port-only difference must never
// produce the claim, and comparison is on the BARE HOST alone: a call to a
// compared host on a new port is reported as in-set rather than out. (An
// authority-equality arm would be dead code next to it — normalizeDestination
// derives the host from the authority by stripping the port, so two equal
// authorities always have equal hosts.) The asymmetry is intentional; the cost
// of staying quiet is a slightly vaguer message, the cost of over-claiming is
// sending an operator after an upstream difference that is not there.
func destinationScope(dest string, compared []string) string {
	liveHost, ok := normalizeDestination(dest)
	if !ok || len(compared) == 0 {
		return models.DestinationScopeUnknown
	}
	for _, c := range compared {
		cHost, ok := normalizeDestination(c)
		if !ok {
			// An unreadable compared entry could be the live destination, so
			// nothing can be concluded. Callers are expected to pass nil
			// rather than a partial set; this is the belt-and-braces.
			return models.DestinationScopeUnknown
		}
		if cHost == liveHost {
			return models.DestinationScopeInComparedSet
		}
	}
	return models.DestinationScopeNotInComparedSet
}

// normalizeDestination reduces a destination to the one comparable form the
// verdict is decided on: the bare host, lowercased, with any port stripped.
// ok is false for anything that carries no usable host identity — blank,
// whitespace, a bare port like ":8080", or a scheme-carrying string — which
// the caller must treat as undecidable.
func normalizeDestination(dest string) (host string, ok bool) {
	authority := strings.ToLower(strings.TrimSpace(dest))
	if authority == "" {
		return "", false
	}
	// A scheme-prefixed value is not an authority and must not be parsed as
	// one: net.SplitHostPort("http://example.com") SUCCEEDS, yielding host
	// "http", which would make every scheme-carrying destination compare
	// equal to every other and produce a confident, wrong verdict. No
	// in-tree producer emits one today — models.RecordedDestination returns
	// url.Parse's Host, and the live side reads request.Host /
	// request.URL.Host, all scheme-free — but this is the exact failure
	// class the feature exists to prevent, so it is refused rather than
	// guessed at.
	if strings.Contains(authority, "://") {
		return "", false
	}
	// net.SplitHostPort is used rather than a LastIndex(":") so bracketed IPv6
	// literals ("[::1]:8080") split correctly and unbracketed ones
	// ("fd00::1", which has many colons and no port) fall through unsplit.
	host = authority
	if h, _, err := net.SplitHostPort(authority); err == nil {
		// A successful split with an empty host means the caller handed us a
		// bare port (":8080") — there is no upstream identity in that, so it
		// falls through to the "not ok" check below rather than comparing as
		// the literal ":8080".
		host = h
	}
	host = strings.Trim(host, "[]")
	// Strip the root label of a rooted FQDN. A Go client that resolves through a
	// search-domain-qualified name can present "svc.ns.svc.cluster.local." while
	// the recorded mock's Host header carries the same name unrooted — they are
	// the SAME upstream, and DNS says so, but a byte comparison does not. Without
	// this the verdict is FALSE for exactly the in-cluster names this diagnostic
	// exists to explain: it would report "no compared mock targets this
	// destination" about an upstream sitting in the compared set. A lone "." is
	// the DNS root and carries no upstream identity, so it falls through to the
	// empty check below.
	if len(host) > 1 {
		host = strings.TrimSuffix(host, ".")
	}
	if host == "" || host == "." {
		return "", false
	}
	return host, true
}
