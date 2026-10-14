package mismatch

import (
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

// destinationScope is the safety gate for the whole feature: it may only
// answer "not in the compared set" when the compared destinations say so
// unambiguously. Every row here is a rule that, if broken, produces a false
// claim — the same class of misdirection the feature exists to remove.
//
// The three states are distinct on purpose. "in_compared_set" and "unknown"
// both leave today's message alone, but only the first means the question was
// actually answered, and downstream surfaces (the agent log's
// destination_scope field) render the two differently.
func TestDestinationScope(t *testing.T) {
	tests := []struct {
		name     string
		dest     string
		compared []string
		want     string
	}{
		{
			name:     "absent from a non-empty set",
			dest:     "192.0.2.20",
			compared: []string{"192.0.2.10", "192.0.2.30:9090", "192.0.2.40"},
			want:     models.DestinationScopeNotInComparedSet,
		},
		{
			name:     "present in the set",
			dest:     "192.0.2.10",
			compared: []string{"192.0.2.10", "192.0.2.40"},
			want:     models.DestinationScopeInComparedSet,
		},
		{
			name:     "live carries a port the recording elided",
			dest:     "192.0.2.10:80",
			compared: []string{"192.0.2.10"},
			want:     models.DestinationScopeInComparedSet,
		},
		{
			name:     "recording carries a port the live call elided",
			dest:     "192.0.2.30",
			compared: []string{"192.0.2.30:9090"},
			want:     models.DestinationScopeInComparedSet,
		},
		{
			name:     "same host, different port, is NOT a new destination",
			dest:     "192.0.2.30:9999",
			compared: []string{"192.0.2.30:9090"},
			want:     models.DestinationScopeInComparedSet,
		},
		{
			name:     "host case is not a difference",
			dest:     "API.Example.COM:8443",
			compared: []string{"api.example.com:8443"},
			want:     models.DestinationScopeInComparedSet,
		},
		{
			name:     "surrounding whitespace is not a difference",
			dest:     "  192.0.2.10  ",
			compared: []string{"192.0.2.10"},
			want:     models.DestinationScopeInComparedSet,
		},
		{
			name:     "blank live destination is undecidable",
			dest:     "",
			compared: []string{"192.0.2.10"},
			want:     models.DestinationScopeUnknown,
		},
		{
			name:     "whitespace-only live destination is undecidable",
			dest:     "   ",
			compared: []string{"192.0.2.10"},
			want:     models.DestinationScopeUnknown,
		},
		{
			name:     "bare port carries no host, undecidable",
			dest:     ":8080",
			compared: []string{"192.0.2.10"},
			want:     models.DestinationScopeUnknown,
		},
		{
			name:     "nil compared set is undecidable, not absence",
			dest:     "192.0.2.20",
			compared: nil,
			want:     models.DestinationScopeUnknown,
		},
		{
			name:     "empty compared set is undecidable, not absence",
			dest:     "192.0.2.20",
			compared: []string{},
			want:     models.DestinationScopeUnknown,
		},
		{
			name:     "one unreadable compared entry vetoes the claim",
			dest:     "192.0.2.20",
			compared: []string{"192.0.2.10", ""},
			want:     models.DestinationScopeUnknown,
		},
		{
			name:     "IPv6 literal with a port matches its bracketless form",
			dest:     "[fd00::1]:8080",
			compared: []string{"fd00::1"},
			want:     models.DestinationScopeInComparedSet,
		},
		{
			name:     "IPv6 literal absent from the set",
			dest:     "[fd00::2]:8080",
			compared: []string{"fd00::1", "192.0.2.10"},
			want:     models.DestinationScopeNotInComparedSet,
		},
		{
			name:     "hostname absent from a set of IPs",
			dest:     "payments.example.com:8443",
			compared: []string{"192.0.2.10", "192.0.2.30:9090"},
			want:     models.DestinationScopeNotInComparedSet,
		},
		{
			// net.SplitHostPort("http://example.com") SUCCEEDS with host
			// "http". Parsed as an authority, every scheme-carrying value
			// collapses to its scheme and compares equal to every other —
			// so a live "http://a.example.com" against a compared
			// "https://a.example.com" would read as a different upstream,
			// and two genuinely different hosts on the same scheme would
			// read as the same one. Refuse to judge instead.
			name:     "scheme-prefixed live destination is undecidable",
			dest:     "http://192.0.2.20",
			compared: []string{"192.0.2.10"},
			want:     models.DestinationScopeUnknown,
		},
		{
			name:     "scheme-prefixed compared entry vetoes the claim",
			dest:     "192.0.2.20",
			compared: []string{"http://192.0.2.10"},
			want:     models.DestinationScopeUnknown,
		},
		{
			// The trap in full: without the guard both sides normalise to
			// "http" and the verdict is a confident, wrong "in_compared_set".
			name:     "two different scheme-prefixed hosts never read as the same upstream",
			dest:     "http://192.0.2.20",
			compared: []string{"http://192.0.2.10"},
			want:     models.DestinationScopeUnknown,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := destinationScope(tt.dest, tt.compared); got != tt.want {
				t.Errorf("destinationScope(%q, %v) = %q, want %q", tt.dest, tt.compared, got, tt.want)
			}
		})
	}
}

// The guidance a miss carries must follow the evidence: a destination absent
// from the compared set gets the scope message, everything else keeps the
// message it had before this feature existed. The cascade phase is never
// rewritten — it is a separate fact from the scope verdict, and the only one
// that says how far matching got.
func TestBuilder_OutOfScopeDestinationGuidance(t *testing.T) {
	valueDrift := []models.MockFieldDiff{
		{Path: "body.order_id", Kind: models.DiffKindValueChanged, Expected: "o-1", Actual: "o-2"},
	}
	structuralDrift := []models.MockFieldDiff{
		{Path: "body.new_field", Kind: models.DiffKindMissingInMock, Actual: "1"},
	}

	tests := []struct {
		name        string
		dest        string
		compared    []string
		phase       string
		candidates  int
		diffs       []models.MockFieldDiff
		wantScope   string
		wantContain []string
		wantAbsent  []string
	}{
		{
			// The reported case: a sidecar's call to a host that none of
			// the compared mocks target, reported until now as "Request
			// structure changed since recording".
			name:       "destination absent from a non-empty compared set",
			dest:       "192.0.2.20",
			compared:   []string{"192.0.2.10", "192.0.2.30:9090"},
			phase:      models.MatchPhaseSchema,
			candidates: 19,
			diffs:      structuralDrift,
			wantScope:  models.DestinationScopeNotInComparedSet,
			wantContain: []string{
				"No recorded HTTP mock in the compared set targets 192.0.2.20",
			},
			// The causes live in models.OutOfScopeDestinationCauses and are
			// rendered once per test; per-call guidance must not carry them.
			wantAbsent: []string{"Request structure changed", "sidecar", "ONE container per session"},
		},
		{
			name:        "destination in the compared set, request drifted structurally",
			dest:        "192.0.2.10",
			compared:    []string{"192.0.2.10", "192.0.2.30:9090"},
			phase:       models.MatchPhaseSchema,
			candidates:  19,
			diffs:       structuralDrift,
			wantScope:   models.DestinationScopeInComparedSet,
			wantContain: []string{"Request structure changed since recording"},
			wantAbsent:  []string{"compared set", "sidecar"},
		},
		{
			// Value drift still wins where it should: the destination IS in
			// the compared set, so the noise hint remains the right advice.
			name:        "destination in the compared set, only values drifted",
			dest:        "192.0.2.10",
			compared:    []string{"192.0.2.10"},
			phase:       models.MatchPhaseBody,
			candidates:  4,
			diffs:       valueDrift,
			wantScope:   models.DestinationScopeInComparedSet,
			wantContain: []string{"Only values drifted", "requestbody"},
			wantAbsent:  []string{"compared set"},
		},
		{
			// ...and loses where it should: diffs against a mock on a
			// DIFFERENT upstream describe a comparison that never meant
			// anything, so noising those fields would be a second wrong turn.
			name:        "out-of-scope destination beats value drift",
			dest:        "192.0.2.20",
			compared:    []string{"192.0.2.10"},
			phase:       models.MatchPhaseBody,
			candidates:  4,
			diffs:       valueDrift,
			wantScope:   models.DestinationScopeNotInComparedSet,
			wantContain: []string{"in the compared set targets 192.0.2.20"},
			wantAbsent:  []string{"Only values drifted"},
		},
		{
			name:        "empty compared set keeps the no_mocks message",
			dest:        "192.0.2.20",
			compared:    nil,
			phase:       models.MatchPhaseNoMocks,
			candidates:  0,
			wantScope:   models.DestinationScopeUnknown,
			wantContain: []string{"No recorded mocks were available", "keploy record"},
			wantAbsent:  []string{"compared set", "sidecar"},
		},
		{
			// Defence in depth: even if a caller contradicts itself by
			// declaring no_mocks while handing over destinations, the
			// accurate no_mocks message is not stolen and no verdict is
			// invented.
			name:        "no_mocks phase is never scored",
			dest:        "192.0.2.20",
			compared:    []string{"192.0.2.10"},
			phase:       models.MatchPhaseNoMocks,
			candidates:  0,
			wantScope:   models.DestinationScopeUnknown,
			wantContain: []string{"No recorded mocks were available"},
			wantAbsent:  []string{"compared set"},
		},
		{
			name:        "unknown live destination falls back unchanged",
			dest:        "",
			compared:    []string{"192.0.2.10"},
			phase:       models.MatchPhaseSchema,
			candidates:  19,
			diffs:       structuralDrift,
			wantScope:   models.DestinationScopeUnknown,
			wantContain: []string{"Request structure changed since recording"},
			wantAbsent:  []string{"compared set"},
		},
		{
			name:        "unreadable mock destinations fall back unchanged",
			dest:        "192.0.2.20",
			compared:    nil, // the HTTP side passes nil when the evidence is not conclusive
			phase:       models.MatchPhaseSchema,
			candidates:  19,
			diffs:       structuralDrift,
			wantScope:   models.DestinationScopeUnknown,
			wantContain: []string{"Request structure changed since recording"},
			wantAbsent:  []string{"compared set"},
		},
		{
			name:        "host:port equivalence keeps today's message",
			dest:        "192.0.2.30",
			compared:    []string{"192.0.2.30:9090"},
			phase:       models.MatchPhaseSchema,
			candidates:  19,
			diffs:       structuralDrift,
			wantScope:   models.DestinationScopeInComparedSet,
			wantContain: []string{"Request structure changed since recording"},
			wantAbsent:  []string{"compared set"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			report := NewReport(ProtocolHTTP, "GET /v1/metrics").
				WithDestination(tt.dest).
				WithComparedDestinations(tt.compared).
				WithPhase(tt.phase, tt.candidates).
				WithClosest("mock-1", tt.diffs).
				Build()

			if report.DestinationScope != tt.wantScope {
				t.Errorf("DestinationScope = %q, want %q", report.DestinationScope, tt.wantScope)
			}
			// The scope verdict never overwrites the cascade's stopping
			// point: both facts have to survive to the report.
			if report.MatchPhase != tt.phase {
				t.Errorf("MatchPhase = %q, want the real cascade stop %q", report.MatchPhase, tt.phase)
			}
			for _, want := range tt.wantContain {
				if !strings.Contains(report.NextSteps, want) {
					t.Errorf("NextSteps missing %q:\n%s", want, report.NextSteps)
				}
			}
			for _, absent := range tt.wantAbsent {
				if strings.Contains(report.NextSteps, absent) {
					t.Errorf("NextSteps must not contain %q:\n%s", absent, report.NextSteps)
				}
			}
			// THE rule this simplification exists to enforce: local evidence
			// can never support a claim about the whole recording, so the
			// words must never appear — in any branch, in any phrasing.
			for _, forbidden := range []string{"never recorded", "was not recorded", "outside the recorded scope"} {
				if strings.Contains(strings.ToLower(report.NextSteps), forbidden) {
					t.Errorf("guidance claims global knowledge (%q):\n%s", forbidden, report.NextSteps)
				}
			}
		})
	}
}

// The no-candidate branch of the HTTP builder produces a report with no
// closest mock at all. The guidance must not then point at "the closest mock",
// which is not rendered anywhere.
func TestBuilder_OutOfScopeWithoutAClosestMock(t *testing.T) {
	report := NewReport(ProtocolHTTP, "GET /v1/metrics").
		WithDestination("192.0.2.20").
		WithComparedDestinations([]string{"192.0.2.10"}).
		WithPhase(models.MatchPhaseSchema, 19).
		Build()

	if !strings.Contains(report.NextSteps, "in the compared set targets 192.0.2.20") {
		t.Errorf("out-of-scope verdict lost its lead sentence:\n%s", report.NextSteps)
	}
	if strings.Contains(report.NextSteps, "The closest mock is on a different upstream") {
		t.Errorf("guidance refers to a closest mock that was never picked:\n%s", report.NextSteps)
	}
}

// The per-call hint must stay ONE sentence about THIS call. An out-of-scope
// container produces one miss per outgoing call it makes; the shared causes
// paragraph used to be inlined here, so a single test could carry the same
// ~1 KB of prose dozens of times over — in the report, and in the agent's
// per-miss next_step log field.
func TestBuilder_OutOfScopeHintIsShortAndPerCall(t *testing.T) {
	report := NewReport(ProtocolHTTP, "GET /v1/metrics").
		WithDestination("192.0.2.20").
		WithComparedDestinations([]string{"192.0.2.10"}).
		WithPhase(models.MatchPhaseSchema, 19).
		WithClosest("mock-1", nil).
		Build()

	// Comfortably under the ~1079 chars the inlined paragraph cost, and in
	// the same size class as every other hint in the switch.
	if len(report.NextSteps) > 260 {
		t.Errorf("per-call hint is %d chars, must stay a short lead sentence:\n%s",
			len(report.NextSteps), report.NextSteps)
	}
	// The one call-specific fact stays.
	if !strings.Contains(report.NextSteps, "192.0.2.20") {
		t.Errorf("hint lost the upstream it is about:\n%s", report.NextSteps)
	}
	// Everything shared must have moved to the once-per-test block.
	for _, moved := range []string{"(1)", "(2)", "(3)", "(4)", "sidecar", "per-test mock window"} {
		if strings.Contains(report.NextSteps, moved) {
			t.Errorf("shared guidance %q must not be repeated per call:\n%s", moved, report.NextSteps)
		}
	}
}

// The shared block is the whole user-facing explanation, so every cause it has
// to offer is pinned here.
//
//   - Endpoint/config drift is a real, test-affecting problem that is neither
//     a container-scope mistake nor ignorable; an "app-under-test → re-record,
//     otherwise → ignore" binary buries it in the ignore bucket.
//   - Per-test-window misrouting is keploy's OWN failure mode: the compared set
//     is GetPerTestMocksInWindow() + GetSessionMocks(), so a mock recorded for
//     this very dependency but timestamped outside the current test's window is
//     absent from it. Without this cause the message sends the user to
//     re-record something that is already recorded.
//   - The container/pod claim must be scoped to Kubernetes. keploy also runs
//     natively ("keploy record -c 'go run .'") and under docker, where there is
//     no pod for replay to intercept, and unconditional pod guidance is
//     literally inapplicable there.
func TestOutOfScopeDestinationCauses(t *testing.T) {
	causes := models.OutOfScopeDestinationCauses

	for _, want := range []string{
		"ONE container per session",
		"different endpoint than the recording",
		"align the replay configuration",
		"outside this test's per-test mock window",
		"timestamps and windowing before re-recording",
		"sidecar",
	} {
		if !strings.Contains(causes, want) {
			t.Errorf("shared causes missing %q:\n%s", want, causes)
		}
	}

	// The pod model is a Kubernetes fact, stated as one.
	podIdx := strings.Index(causes, "whole pod")
	k8sIdx := strings.Index(causes, "In Kubernetes")
	if k8sIdx < 0 || podIdx < 0 || k8sIdx > podIdx {
		t.Errorf("the pod/container claim must be qualified as Kubernetes-specific:\n%s", causes)
	}

	// Ordering: the two causes that are real problems come before the one
	// that says "ignore it", or a genuine drift lands in the ignore bucket.
	ignoreIdx := strings.Index(causes, "can be ignored")
	driftIdx := strings.Index(causes, "different endpoint")
	windowIdx := strings.Index(causes, "per-test mock window")
	if driftIdx < 0 || windowIdx < 0 || ignoreIdx < 0 || driftIdx > ignoreIdx || windowIdx > ignoreIdx {
		t.Errorf("actionable causes must precede the ignore-it clause:\n%s", causes)
	}

	// Framed as possibilities, never as findings, and never as a claim about
	// the whole recording.
	if !strings.Contains(causes, "Likely causes") {
		t.Errorf("causes must be framed as likely, not asserted:\n%s", causes)
	}
	for _, forbidden := range []string{"never recorded", "was not recorded", "outside the recorded scope"} {
		if strings.Contains(strings.ToLower(causes), forbidden) {
			t.Errorf("shared causes claim global knowledge (%q):\n%s", forbidden, causes)
		}
	}
	// The caveat has to cover BOTH ways a recorded mock can be missing from
	// the compared set, or cause (3) contradicts it.
	if !strings.Contains(causes, "already served earlier in this run, or recorded outside this test's mock window") {
		t.Errorf("caveat must cover consumption AND windowing:\n%s", causes)
	}
	// Every line fits a 100-column terminal unwrapped.
	for _, line := range strings.Split(causes, "\n") {
		if len(line) > 100 {
			t.Errorf("line exceeds 100 columns (%d): %q", len(line), line)
		}
	}
}

// RenderOutOfScopeDestinationCauses indents every line, including blank-ish
// ones, so a renderer can nest the block without re-wrapping it.
func TestRenderOutOfScopeDestinationCauses(t *testing.T) {
	rendered := models.RenderOutOfScopeDestinationCauses("  ")
	for _, line := range strings.Split(rendered, "\n") {
		if !strings.HasPrefix(line, "  ") {
			t.Fatalf("line not indented: %q", line)
		}
	}
	if strings.Count(rendered, "\n") != strings.Count(models.OutOfScopeDestinationCauses, "\n") {
		t.Errorf("indenting must not add or drop lines")
	}
}

// The scope check is diagnostic only. It may set its own verdict and pick the
// guidance; it must never touch the evidence a user reads to judge the miss
// (candidate count, closest mock, field diffs, the cascade phase) or override
// a hint the parser set explicitly.
func TestBuilder_OutOfScopeDestinationPreservesEvidence(t *testing.T) {
	diffs := []models.MockFieldDiff{
		{Path: "path", Kind: models.DiffKindValueChanged, Expected: "/api/orders", Actual: "/v1/metrics"},
	}
	report := NewReport(ProtocolHTTP, "GET /v1/metrics").
		WithDestination("192.0.2.20").
		WithComparedDestinations([]string{"192.0.2.10"}).
		WithPhase(models.MatchPhaseSchema, 19).
		WithClosest("mock-1", diffs).
		Build()

	if report.CandidateCount != 19 || report.ClosestMock != "mock-1" || len(report.FieldDiffs) != 1 {
		t.Errorf("scope check must not disturb the evidence: %+v", report)
	}
	if !strings.Contains(report.Diff, "/v1/metrics") {
		t.Errorf("rendered diff lost: %q", report.Diff)
	}
	// Triage information: "the cascade stopped for want of schema candidates"
	// is what a reader needs next, and an earlier cut of this code destroyed
	// it by writing the verdict over the phase.
	if report.MatchPhase != models.MatchPhaseSchema {
		t.Errorf("MatchPhase = %q, want the real cascade stop %q", report.MatchPhase, models.MatchPhaseSchema)
	}

	explicit := NewReport(ProtocolHTTP, "GET /v1/metrics").
		WithDestination("192.0.2.20").
		WithComparedDestinations([]string{"192.0.2.10"}).
		WithPhase(models.MatchPhaseSchema, 19).
		WithNextSteps("parser-supplied hint").
		Build()
	if explicit.NextSteps != "parser-supplied hint" {
		t.Errorf("explicit NextSteps overridden: %q", explicit.NextSteps)
	}
	if explicit.DestinationScope != models.DestinationScopeNotInComparedSet {
		t.Errorf("verdict should still record the out-of-scope destination, got %q", explicit.DestinationScope)
	}
	if explicit.MatchPhase != models.MatchPhaseSchema {
		t.Errorf("MatchPhase = %q, want %q", explicit.MatchPhase, models.MatchPhaseSchema)
	}
}

// WORDING ONLY. The guidance names the protocol so a mixed-protocol report
// reads correctly, and stays grammatical when the parser did not set one.
//
// The configurations fed in here are NOT reachable in production: HTTP is the
// only protocol whose recorded mocks carry an upstream authority
// (models.Mock.RecordedDestination returns ok=false for every non-HTTP kind),
// so nothing but HTTP can supply a compared set, and no Mongo miss will ever
// reach this branch. The point of the test is the sentence the branch builds,
// not the reachability of the inputs — the mechanism is protocol-agnostic by
// design and this pins the wording for the day a second protocol records a
// destination.
func TestBuilder_OutOfScopeDestinationProtocolWording(t *testing.T) {
	withProto := NewReport(ProtocolMongo, "find users").
		WithDestination("192.0.2.20:27017").
		WithComparedDestinations([]string{"mongo.example.com:27017"}).
		WithPhase(models.MatchPhaseExhausted, 3).Build()
	if !strings.Contains(withProto.NextSteps, "No recorded MongoDB mock in the compared set targets") {
		t.Errorf("expected protocol-named scope, got %q", withProto.NextSteps)
	}

	noProto := NewReport("", "opaque exchange").
		WithDestination("192.0.2.20:9000").
		WithComparedDestinations([]string{"192.0.2.10:9000"}).
		WithPhase(models.MatchPhaseExhausted, 3).Build()
	if !strings.Contains(noProto.NextSteps, "No recorded mock in the compared set targets") {
		t.Errorf("expected protocol-less wording, got %q", noProto.NextSteps)
	}
}

// A protocol that supplies no destination evidence — every protocol but HTTP
// today — must leave the verdict empty rather than "in_compared_set". An empty
// verdict is what stops the agent log from asserting a scope check nobody ran
// on a Mongo miss.
func TestBuilder_NoEvidenceLeavesVerdictUnset(t *testing.T) {
	report := NewReport(ProtocolMongo, "find users").
		WithDestination("mongo.example.com:27017").
		WithPhase(models.MatchPhaseExhausted, 7).Build()

	if report.DestinationScope != models.DestinationScopeUnknown {
		t.Errorf("DestinationScope = %q, want unset for a protocol that supplied no evidence", report.DestinationScope)
	}
}

// TestNormalizeDestinationRootedFQDN pins the rooted-FQDN equivalence. A Go
// client resolving through a search domain presents the name rooted; the
// recorded mock's Host header carries it unrooted. They are the same upstream,
// and without the trailing-dot strip the diagnostic reports "no compared mock
// targets this destination" about a host sitting in the compared set — a false
// statement, in exactly the in-cluster shape this diagnostic exists to explain.
func TestNormalizeDestinationRootedFQDN(t *testing.T) {
	for _, tc := range []struct {
		name, dest, wantHost string
		wantOK               bool
	}{
		{"rooted fqdn", "svc.ns.svc.cluster.local.", "svc.ns.svc.cluster.local", true},
		{"unrooted fqdn", "svc.ns.svc.cluster.local", "svc.ns.svc.cluster.local", true},
		{"rooted with port", "svc.ns.svc.cluster.local.:8443", "svc.ns.svc.cluster.local", true},
		{"bare dns root", ".", "", false},
		{"ipv6 literal is untouched", "[fd00::1]:8080", "fd00::1", true},
		{"scheme-carrying value is refused, not parsed as host \"http\"", "http://example.com", "", false},
		{"scheme with a port is refused too", "https://example.com:8443", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			host, ok := normalizeDestination(tc.dest)
			if ok != tc.wantOK || host != tc.wantHost {
				t.Fatalf("normalizeDestination(%q) = (%q, %v), want (%q, %v)",
					tc.dest, host, ok, tc.wantHost, tc.wantOK)
			}
		})
	}
}

// TestRootedAndUnrootedAreTheSameUpstream is the end-to-end form: the verdict
// must NOT claim absence when the compared set holds the same name unrooted.
func TestRootedAndUnrootedAreTheSameUpstream(t *testing.T) {
	if got := destinationScope("svc.ns.svc.cluster.local.",
		[]string{"svc.ns.svc.cluster.local"}); got == models.DestinationScopeNotInComparedSet {
		t.Fatal("a rooted FQDN was reported as absent from a compared set that holds it unrooted")
	}
}
