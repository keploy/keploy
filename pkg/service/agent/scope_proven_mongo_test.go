package agent

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/models"
)

// mongoRequestValues are values the requests behind the rows' misses sent:
// evidence holds none of them.
var mongoRequestValues = []string{"5e246614", "u-1", "u-2", "a@b.c", "t-9", "kqzx9w"}

// The api-server entries' miss (keploy record's full round pairs a data call
// with the test's recording and refuses its drift: request_changed,
// body_mismatch), for each command they were made with: the test's reason
// names the call (command, database and collection), the fields it differs
// in by their shape, the closest recording and why, and the test to
// re-record; the run's summary says the same fields in its next step. No
// value the call sent is in either.
func TestMongoMissEvidenceNamesTheCallFieldsClosestAndTest(t *testing.T) {
	for _, c := range []struct {
		summary, call string
		differs       []string
		fields        string
	}{
		{"update on app.docs", "update on app.docs", []string{"body.cmd.updates[].u.$set.createdBy"}, "body.cmd.updates[].u.$set.createdBy differs"},
		{"find on keploy.apps", "find on keploy.apps", []string{"body.cmd.filter.appId"}, "body.cmd.filter.appId differs"},
		{"insert on app.users", "insert on app.users", []string{"body.documents[].email", "body.documents[].createdAt"}, "body.documents[].email and body.documents[].createdAt differ"},
		{"aggregate on app.events", "aggregate on app.events", []string{"body.cmd.pipeline[0].$match.tenant"}, "body.cmd.pipeline[0].$match.tenant differs"},
		{"delete on app.sessions", "delete on app.sessions", []string{"body.cmd.deletes[].q._id.$oid"}, "body.cmd.deletes[].q._id.$oid differs"},
		{"findAndModify on app.counters", "findAndModify on app.counters", []string{"body.cmd.update.$inc.seq"}, "body.cmd.update.$inc.seq differs"},
		{"count on app.users", "count on app.users", []string{"body.cmd.query.orgId"}, "body.cmd.query.orgId differs"},
		{"find on app.events_5e246614", "find on app.{}", []string{"body.cmd.filter.users.u-1.at"}, "body.cmd.filter.users.{}.at differs"},
	} {
		t.Run(c.summary, func(t *testing.T) {
			var diffs []models.MockFieldDiff
			for _, d := range c.differs {
				diffs = append(diffs, models.MockFieldDiff{Path: d})
			}
			raw := models.UnmatchedCall{Protocol: "MongoDB", Destination: "[::1]:27017", MatchPhase: models.MatchPhaseBody,
				ActualSummary: c.summary, ClosestMock: "mock-12", Reason: models.MissRequestChanged, Differs: c.differs, FieldDiffs: diffs,
				NextSteps:      "the call's request changed from mock-12 (" + strings.Join(c.differs, ", ") + " changed from it): re-record the test",
				ReceivedReq:    `{"update":"docs","updates":[{"q":{"owner":"u-1"},"u":{"$set":{"createdBy":"u-2"}}}],"$db":"app","appId":"5e246614"}`,
				ClosestMockReq: `{"email":"a@b.c","tenant":"t-9"}`}
			m := sanitizedMiss(raw)
			require.Equal(t, c.call, m.ActualSummary)
			require.Equal(t, c.fields+" from the recording; if the change is expected, re-record the mock set with 'keploy mock record'", m.NextSteps)

			reason := evidenceReason("api::TestAppCRUD", &models.ScopeEvidence{Misses: []models.UnmatchedCall{m}}, nil, time.Time{})
			require.Contains(t, reason, "MongoDB "+c.call+" to [::1]:27017 had no recorded answer ("+c.fields+
				"), closest recording mock-12 (a call of the same shape, its request body differs).")
			require.Contains(t, reason, `selecting api::TestAppCRUD>`)
			for _, v := range mongoRequestValues {
				require.NotContains(t, reason, v)
				require.NotContains(t, m.NextSteps, v)
				require.NotContains(t, m.ActualSummary, v)
				require.NotContains(t, strings.Join(m.Differs, " "), v)
			}

			// The same miss unsanitized reads the same: the reason shapes
			// what it names itself.
			require.Contains(t, evidenceReason("api::TestAppCRUD", &models.ScopeEvidence{Misses: []models.UnmatchedCall{raw}}, nil, time.Time{}),
				"MongoDB "+c.call+" to [::1]:27017 had no recorded answer ("+c.fields+"), closest recording mock-12")
		})
	}
}

// A MongoDB miss with no shape (a summary keploy cannot read), no fields
// and no closest recording is named by its protocol and destination alone;
// another protocol's reason does not name its closest recording, as before.
func TestMongoMissEvidenceWithoutItsParts(t *testing.T) {
	bare := evidenceReason("t", &models.ScopeEvidence{Misses: []models.UnmatchedCall{{Protocol: "MongoDB", Destination: "[::1]:27017",
		ActualSummary: `{ SectionSingle msg: {"find":"apps","filter":{"appId":"5e246614"}} }`}}}, nil, time.Time{})
	require.Contains(t, bare, "keploy: a MongoDB call to [::1]:27017 had no recorded answer. ")
	require.NotContains(t, bare, "5e246614")

	ping := evidenceReason("t", &models.ScopeEvidence{Misses: []models.UnmatchedCall{{Protocol: "MongoDB", Destination: "[::1]:27017",
		ActualSummary: "ping on ", MatchPhase: models.MatchPhaseSchema, ClosestMock: "mock-3"}}}, nil, time.Time{})
	require.Contains(t, ping, "keploy: MongoDB ping to [::1]:27017 had no recorded answer, closest recording mock-3. ")

	http := evidenceReason("t", &models.ScopeEvidence{Misses: []models.UnmatchedCall{{Protocol: "HTTP", Destination: "api.internal",
		ActualSummary: "POST /x", MatchPhase: models.MatchPhaseBody, ClosestMock: "mock-4", Differs: []string{"body.q"}}}}, nil, time.Time{})
	require.Contains(t, http, "keploy: POST /x to api.internal had no recorded answer (body.q differs). ")
	require.NotContains(t, http, "mock-4")

	// A miss made before the test began, or as it ended, names its parts
	// too.
	began := time.Unix(1000, 0)
	m := models.UnmatchedCall{Protocol: "MongoDB", Destination: "[::1]:27017", ActualSummary: "find on app.docs",
		MatchPhase: models.MatchPhaseBody, ClosestMock: "mock-12", Differs: []string{"body.cmd.filter.owner"}}
	early := m
	early.At = began.Add(-time.Second)
	require.Contains(t, evidenceReason("t", &models.ScopeEvidence{Misses: []models.UnmatchedCall{early}}, nil, began),
		"MongoDB find on app.docs to [::1]:27017, made before this one began, had no recorded answer (body.cmd.filter.owner differs), closest recording mock-12 (")
	late := m
	late.Kind, late.At = models.MissOutsideTest, began.Add(time.Second)
	require.Contains(t, evidenceReason("t", &models.ScopeEvidence{Misses: []models.UnmatchedCall{late}}, nil, began),
		"MongoDB find on app.docs to [::1]:27017, made as this test ended, had no recorded answer (body.cmd.filter.owner differs), closest recording mock-12 (")
	outside := m
	outside.Kind = models.MissOutsideTest
	require.Contains(t, evidenceReason("t", &models.ScopeEvidence{Misses: []models.UnmatchedCall{outside}}, nil, time.Time{}),
		"made outside every test before this one began, had no recorded answer (body.cmd.filter.owner differs), closest recording mock-12 (")
}
