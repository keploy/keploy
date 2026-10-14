// Package proxy — the one agent-wide mock-mismatch log line must not assert a
// negative it never checked.
//
// destination_scope is only meaningful for a report whose parser supplied
// destination evidence. HTTP is the only protocol that does today (nothing
// else records an upstream authority in its mocks), so an unconditional field
// printed a negative verdict on every mongo/mysql/generic/pulsar miss — which
// reads as "we checked, and it was fine" for a check that never ran.
package proxy

import (
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func mismatchLogFields(t *testing.T, report *models.MockMismatchReport) map[string]any {
	t.Helper()
	core, logs := observer.New(zap.WarnLevel)
	p := &Proxy{logger: zap.New(core), errChannel: make(chan error, 4)}

	p.sendMockNotFoundError(models.NewMockMismatchError(models.ErrNoMockMatched, report))

	entries := logs.All()
	if len(entries) != 1 {
		t.Fatalf("expected exactly one mismatch log entry, got %d", len(entries))
	}
	return entries[0].ContextMap()
}

func TestSendMockNotFoundError_ScopeFieldOmittedWithoutEvidence(t *testing.T) {
	// A Mongo miss: no destination evidence was ever gathered.
	fields := mismatchLogFields(t, &models.MockMismatchReport{
		Protocol:      "MongoDB",
		Destination:   "mongo.example.com:27017",
		ActualSummary: "find users",
		MatchPhase:    models.MatchPhaseExhausted,
	})

	if _, present := fields["destination_scope"]; present {
		t.Errorf("unchecked destination scope must be absent from the log, got %v", fields["destination_scope"])
	}
	if fields["match_phase"] != models.MatchPhaseExhausted {
		t.Errorf("match_phase = %v, want %q", fields["match_phase"], models.MatchPhaseExhausted)
	}
}

func TestSendMockNotFoundError_ScopeFieldReflectsTheVerdict(t *testing.T) {
	for _, scope := range []string{
		models.DestinationScopeNotInComparedSet,
		models.DestinationScopeInComparedSet,
	} {
		t.Run(scope, func(t *testing.T) {
			fields := mismatchLogFields(t, &models.MockMismatchReport{
				Protocol:         "HTTP",
				Destination:      "192.0.2.20",
				ActualSummary:    "GET /v1/metrics",
				MatchPhase:       models.MatchPhaseSchema,
				DestinationScope: scope,
			})
			got, present := fields["destination_scope"]
			if !present {
				t.Fatalf("an answered scope check must be logged; fields: %v", fields)
			}
			if got != scope {
				t.Errorf("destination_scope = %v, want %v", got, scope)
			}
			// The cascade's stopping point survives alongside the verdict.
			if fields["match_phase"] != models.MatchPhaseSchema {
				t.Errorf("match_phase = %v, want %q", fields["match_phase"], models.MatchPhaseSchema)
			}
		})
	}
}
