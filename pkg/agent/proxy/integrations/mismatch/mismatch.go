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
	"sort"
	"strings"

	"go.keploy.io/server/v3/pkg/matcher"
	"go.keploy.io/server/v3/pkg/models"
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
)

// maxFieldDiffValueLen bounds recorded/live values embedded in reports so a
// large payload can't bloat the report yaml or the CLI table.
const maxFieldDiffValueLen = 256

// maxFieldDiffs bounds how many per-field diffs a single report carries.
const maxFieldDiffs = 25

// Builder assembles a models.MockMismatchReport with consistent rendering.
type Builder struct {
	report models.MockMismatchReport
}

// NewReport starts a report for one missed call. actualSummary should be the
// shortest string that identifies the live call ("POST /orders", "SELECT
// (prepared stmt 12)", "find users").
func NewReport(protocol, actualSummary string) *Builder {
	return &Builder{report: models.MockMismatchReport{
		Protocol:      protocol,
		ActualSummary: actualSummary,
	}}
}

// WithPhase records how far the match cascade got and how many candidate
// mocks were considered. Use the models.MatchPhase* constants.
func (b *Builder) WithPhase(phase string, candidateCount int) *Builder {
	b.report.MatchPhase = phase
	b.report.CandidateCount = candidateCount
	return b
}

// WithClosest attaches the nearest candidate and its field-level diffs.
func (b *Builder) WithClosest(mockName string, diffs []models.MockFieldDiff) *Builder {
	b.report.ClosestMock = mockName
	if len(diffs) > maxFieldDiffs {
		diffs = diffs[:maxFieldDiffs]
	}
	b.report.FieldDiffs = diffs
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
	if r.Diff == "" {
		r.Diff = RenderFieldDiffs(r.FieldDiffs)
	}
	if r.NextSteps == "" {
		r.NextSteps = defaultNextSteps(&r)
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
			parts = append(parts, fmt.Sprintf("%s: recorded %q, live %q", d.Path, d.Expected, d.Actual))
		}
	}
	return strings.Join(parts, "; ")
}

// defaultNextSteps derives an actionable hint from the report shape. The
// wording deliberately never references commands that don't exist; the two
// real remedies are noise (for drifting values) and re-recording (for
// structural change).
func defaultNextSteps(r *models.MockMismatchReport) string {
	onlyValueDrift := len(r.FieldDiffs) > 0
	for _, d := range r.FieldDiffs {
		if d.Kind != models.DiffKindValueChanged {
			onlyValueDrift = false
			break
		}
	}
	switch {
	case r.MatchPhase == models.MatchPhaseNoMocks:
		return "No recorded mocks exist for this protocol in the selected test set. Re-record the test set with 'keploy record'."
	case onlyValueDrift:
		paths := make([]string, 0, len(r.FieldDiffs))
		// Body diffs are reported "body."-prefixed for readability, but HTTP
		// request matching reads the request-body noise bucket with
		// root-relative keys — so strip the prefix in the copy-paste hint to
		// avoid pointing users at the response-noise (body) bucket, which the
		// request matcher never consults.
		bodyKeys := make([]string, 0, len(r.FieldDiffs))
		for _, d := range r.FieldDiffs {
			paths = append(paths, d.Path)
			if k := strings.TrimPrefix(d.Path, "body."); k != d.Path {
				bodyKeys = append(bodyKeys, k)
			}
		}
		if len(bodyKeys) > 0 {
			return fmt.Sprintf("Only values drifted (%s). If these are dynamic (timestamps, ids, tokens), add the request-body fields under test.globalNoise.requestbody with root-relative keys (e.g. requestbody: {%s: []}); otherwise re-record with 'keploy record'.", strings.Join(paths, ", "), strings.Join(bodyKeys, ": [], "))
		}
		return fmt.Sprintf("Only values drifted (%s). If these are dynamic (timestamps, ids, tokens), add them to the matching noise (test.globalNoise); otherwise re-record with 'keploy record'.", strings.Join(paths, ", "))
	default:
		return "Request structure changed since recording. Re-record the test set with 'keploy record', or refresh mappings with --update-test-mapping if mocks were edited."
	}
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
