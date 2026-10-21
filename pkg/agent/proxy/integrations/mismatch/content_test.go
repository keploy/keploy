package mismatch

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils/logcontent"
)

// contentReport is a report whose every field that can carry the
// application's content does: the field diffs' values, the rendered
// requests, the summary's query.
func contentReport() *Builder {
	return NewReport(ProtocolHTTP, "POST /api/users?token=kp-canary-query").
		WithDestination("users.internal:8080").
		WithPhase(models.MatchPhaseBody, 2).
		WithClosest("mock-1", []models.MockFieldDiff{
			{Path: "body.email", Kind: models.DiffKindValueChanged, Expected: "kp-canary-rec", Actual: "kp-canary-live"},
			{Path: "header.X-Gone", Kind: models.DiffKindMissingInLive, Expected: "kp-canary-gone"},
		}).
		WithRenderedRequests("POST /api/users\n\n{\"email\":\"kp-canary-mock-req\"}", "POST /api/users\n\n{\"email\":\"kp-canary-live-req\"}")
}

var builderCanaries = []string{"kp-canary-query", "kp-canary-rec", "kp-canary-live", "kp-canary-gone", "kp-canary-mock-req", "kp-canary-live-req"}

// By default a built report carries none of the application's content and
// still says what differed: each field's path and kind, how far matching got,
// against how many candidates, the closest mock, and the advice that names
// the fields.
func TestBuildWithholdsTheApplicationsContentByDefault(t *testing.T) {
	r := contentReport().Build()
	text := strings.Join([]string{r.ActualSummary, r.Diff, r.NextSteps, r.ClosestMockReq, r.ReceivedReq}, "\n")
	for _, d := range r.FieldDiffs {
		text += "\n" + d.Expected + "\n" + d.Actual
	}
	for _, c := range builderCanaries {
		if strings.Contains(text, c) {
			t.Errorf("the default report carries %q: %+v", c, *r)
		}
	}
	if !r.ContentWithheld {
		t.Errorf("the default report does not say it withheld its content: %+v", *r)
	}
	if r.ActualSummary != "POST /api/users" || r.ClosestMock != "mock-1" || r.CandidateCount != 2 || r.MatchPhase != models.MatchPhaseBody {
		t.Errorf("the default report lost what is not content: %+v", *r)
	}
	if want := "body.email: value changed; header.X-Gone: absent in live call"; r.Diff != want {
		t.Errorf("diff = %q, want %q", r.Diff, want)
	}
	if len(r.FieldDiffs) != 2 || r.FieldDiffs[0].Path != "body.email" || r.FieldDiffs[1].Kind != models.DiffKindMissingInLive {
		t.Errorf("field diffs = %+v, want both paths and kinds", r.FieldDiffs)
	}
	if !strings.Contains(r.NextSteps, "header.X-Gone") && !strings.Contains(r.NextSteps, "Re-record") {
		t.Errorf("next steps = %q", r.NextSteps)
	}
	// A diff the parser wrote is its own words, and kept.
	own := "closest recorded exchange \"mock-1\" has Jaccard similarity 0.32"
	if r := contentReport().WithDiff(own).Build(); r.Diff != own {
		t.Errorf("the parser's diff became %q", r.Diff)
	}
	// So is its advice.
	if r := contentReport().WithNextSteps("re-record the test").Build(); r.NextSteps != "re-record the test" {
		t.Errorf("the parser's advice became %q", r.NextSteps)
	}
}

// With the switch on a report is what it always was: the values, the
// rendered requests and the summary whole.
func TestBuildKeepsTheContentWithTheSwitchOn(t *testing.T) {
	t.Cleanup(logcontent.Set(true))
	r := contentReport().Build()
	r.At = time.Time{}
	want := models.MockMismatchReport{
		Protocol: ProtocolHTTP, ActualSummary: "POST /api/users?token=kp-canary-query", Destination: "users.internal:8080",
		ClosestMock: "mock-1", MatchPhase: models.MatchPhaseBody, CandidateCount: 2,
		Diff: `body.email: recorded "kp-canary-rec", live "kp-canary-live"; header.X-Gone: recorded "kp-canary-gone", absent in live call`,
		FieldDiffs: []models.MockFieldDiff{
			{Path: "body.email", Kind: models.DiffKindValueChanged, Expected: "kp-canary-rec", Actual: "kp-canary-live"},
			{Path: "header.X-Gone", Kind: models.DiffKindMissingInLive, Expected: "kp-canary-gone"},
		},
		ClosestMockReq: "POST /api/users\n\n{\"email\":\"kp-canary-mock-req\"}",
		ReceivedReq:    "POST /api/users\n\n{\"email\":\"kp-canary-live-req\"}",
		NextSteps:      r.NextSteps,
	}
	if !reflect.DeepEqual(*r, want) {
		t.Errorf("with the switch on:\n got %+v\nwant %+v", *r, want)
	}
}

// A parser that builds its report without the builder, or writes content into
// one after Build, withholds it as Build does: its own diff kept, everything
// else that is content dropped. With the switch on it changes nothing.
func TestWithholdIsBuildsRuleForAReportBuiltElsewhere(t *testing.T) {
	if Withhold(nil) != nil {
		t.Fatal("Withhold(nil) is not nil")
	}
	literal := func() *models.MockMismatchReport {
		return &models.MockMismatchReport{
			Protocol: ProtocolMySQL, ActualSummary: "COM_QUERY SELECT * FROM t WHERE a = 'kp-canary-rec'",
			Diff:        "actual: SELECT * FROM t WHERE a = ?",
			FieldDiffs:  []models.MockFieldDiff{{Path: "body.a", Kind: models.DiffKindValueChanged, Expected: "kp-canary-rec", Actual: "kp-canary-live"}},
			ReceivedReq: "kp-canary-live-req",
		}
	}
	r := Withhold(literal())
	if r.ActualSummary != "COM_QUERY SELECT * FROM t WHERE a = ?" || r.Diff != "actual: SELECT * FROM t WHERE a = ?" ||
		r.FieldDiffs[0].Expected != "" || r.FieldDiffs[0].Actual != "" || r.ReceivedReq != "" || !r.ContentWithheld {
		t.Errorf("withheld: %+v", *r)
	}
	t.Cleanup(logcontent.Set(true))
	if r := Withhold(literal()); !reflect.DeepEqual(r, literal()) {
		t.Errorf("with the switch on Withhold changed the report: %+v", *r)
	}
}

// A field diff that names a path segment says it holds one of the
// application's values: by default the summary names the path with it {}.
func TestBuildWritesAFieldDiffsPathPlaceInTheSummary(t *testing.T) {
	b := func() *Builder {
		return NewReport(ProtocolHTTP, "GET /orders/kp-canary-order/items").
			WithPhase(models.MatchPhaseVariable, 1).
			WithClosest("mock-2", []models.MockFieldDiff{{Path: "url.path[1]", Kind: models.DiffKindVariable, Expected: "kp-canary-rec", Actual: "kp-canary-order"}})
	}
	if r := b().Build(); r.ActualSummary != "GET /orders/{}/items" || r.Diff != "url.path[1]: not the test variable's value" {
		t.Errorf("summary %q, diff %q", r.ActualSummary, r.Diff)
	}
	t.Cleanup(logcontent.Set(true))
	if r := b().Build(); r.ActualSummary != "GET /orders/kp-canary-order/items" {
		t.Errorf("with the switch on the summary is %q", r.ActualSummary)
	}
}

// A parser's words that quote the call's summary, its own Diff and next
// steps, quote it as the summary the report keeps names it: by default the
// path's value segment written {} and no query, there too.
func TestBuildsWordsQuoteTheSummaryItKeeps(t *testing.T) {
	b := func() *Builder {
		return NewReport(ProtocolHTTP, "GET /orders/kp-canary-order/items?token=kp-canary-q").
			WithPhase(models.MatchPhaseVariable, 1).
			WithClosest("mock-2", []models.MockFieldDiff{{Path: "url.path[1]", Kind: models.DiffKindVariable, Expected: "kp-canary-rec", Actual: "kp-canary-order"}}).
			WithDiff("GET /orders/kp-canary-order/items?token=kp-canary-q: url.path[1] is not the test variable's").
			WithNextSteps(`serve "/orders/kp-canary-order/items" to every test, or re-record`)
	}
	r := b().Build()
	if r.ActualSummary != "GET /orders/{}/items" || r.Diff != "GET /orders/{}/items: url.path[1] is not the test variable's" ||
		r.NextSteps != `serve "/orders/{}/items" to every test, or re-record` {
		t.Errorf("summary %q, diff %q, next steps %q", r.ActualSummary, r.Diff, r.NextSteps)
	}
	t.Cleanup(logcontent.Set(true))
	if r := b().Build(); r.Diff != "GET /orders/kp-canary-order/items?token=kp-canary-q: url.path[1] is not the test variable's" ||
		r.NextSteps != `serve "/orders/kp-canary-order/items" to every test, or re-record` {
		t.Errorf("with the switch on: diff %q, next steps %q", r.Diff, r.NextSteps)
	}
}

// A PostgreSQL miss's summary keeps its statement's double-quoted
// identifiers, the names an ORM writes, and loses its literals: by default
// the report, the MOCK MISMATCH heading and the agent's miss line still say
// which statement missed. A writer withholding a call it was handed keeps
// them too. With the switch on the statement is whole.
func TestAPostgresMissSummaryKeepsItsNames(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{`SELECT "users"."id", "users"."email" FROM "users" WHERE "users"."email" = $1 AND "users"."deleted_at" IS NULL LIMIT $2`,
			`SELECT "users"."id", "users"."email" FROM "users" WHERE "users"."email" = $1 AND "users"."deleted_at" IS NULL LIMIT $2`},
		{`SELECT "public"."Post"."id" FROM "public"."Post" WHERE "public"."Post"."authorId" = 'kp-canary-author'`,
			`SELECT "public"."Post"."id" FROM "public"."Post" WHERE "public"."Post"."authorId" = ?`},
	} {
		if r := NewReport(ProtocolPostgres, c.in).Build(); r.ActualSummary != c.want || !r.ContentWithheld {
			t.Errorf("Build: summary %q (withheld %v), want %q", r.ActualSummary, r.ContentWithheld, c.want)
		}
		if got := (models.UnmatchedCall{Protocol: ProtocolPostgres, ActualSummary: c.in}).WithoutContent().ActualSummary; got != c.want {
			t.Errorf("WithoutContent: summary %q, want %q", got, c.want)
		}
		restore := logcontent.Set(true)
		if r := NewReport(ProtocolPostgres, c.in).Build(); r.ActualSummary != c.in {
			t.Errorf("with the switch on the summary is %q, want %q", r.ActualSummary, c.in)
		}
		restore()
	}
}
