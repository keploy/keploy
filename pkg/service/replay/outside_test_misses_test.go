package replay

import (
	"fmt"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// The set's outside-test misses (TestReport.OutsideTestMisses): with the
// proven switch off each call made before the set's first test, which fails
// nothing; with it on those that count against no test, since failing takes
// the others, and the set's end read's that count against none, last. An
// answered call is never one.
func TestTheSetsOutsideTestMissesFailNothingInEitherMode(t *testing.T) {
	dns := models.UnmatchedCall{Protocol: "DNS", ActualSummary: "A db.example.", BeforeSet: true}
	call := models.UnmatchedCall{Protocol: "HTTP", ActualSummary: "GET /config", BeforeSet: true}
	answered := models.UnmatchedCall{Protocol: "HTTP", ActualSummary: "GET /ok", BeforeSet: true, Answer: "by_kind"}
	lateDNS := models.UnmatchedCall{Protocol: "DNS", ActualSummary: "A late.example."}
	lateCall := models.UnmatchedCall{Protocol: "HTTP", ActualSummary: "GET /late"}
	lateBackground := models.UnmatchedCall{Protocol: "HTTP", ActualSummary: "GET /tick", Background: true}

	var s setCalls
	if rest := s.take([]models.UnmatchedCall{dns, call, answered}); len(rest) != 0 {
		t.Fatalf("the first test's read kept %+v as its own", rest)
	}
	s.keepAfter([]models.UnmatchedCall{lateDNS, lateCall, lateBackground})

	names := func(calls []models.UnmatchedCall) string {
		out := ""
		for _, c := range calls {
			out += c.ActualSummary + ";"
		}
		return out
	}
	if got, want := names(s.outside(false)), "A db.example.;GET /config;A late.example.;GET /tick;"; got != want {
		t.Errorf("switch off: outside = %s, want %s", got, want)
	}
	if got, want := names(s.outside(true)), "A db.example.;A late.example.;GET /tick;"; got != want {
		t.Errorf("switch on: outside = %s, want %s", got, want)
	}
	if got, want := names(s.failing()), "GET /config;"; got != want {
		t.Errorf("failing = %s, want %s", got, want)
	}
	s.reset()
	if len(s.outside(false)) != 0 || len(s.outside(true)) != 0 {
		t.Error("a reset set keeps outside-test misses")
	}
}

// Each outside-test miss is said at WARN, by its shape and tags, with its
// next step; past outsideLogMax, one line says how many more the report
// names.
func TestEachOutsideTestMissIsSaidWithItsNextStep(t *testing.T) {
	core, logs := observer.New(zap.WarnLevel)
	r := &Replayer{logger: zap.New(core)}
	misses := models.OutsideTestMissesOf([]models.UnmatchedCall{{Protocol: "DNS", ActualSummary: "A db.example.", HostTag: "#ab12", BeforeSet: true}})
	r.logOutsideTestMisses("test-set-0", misses)
	e := logs.FilterMessageSnippet("outside every test").All()
	if len(e) != 1 {
		t.Fatalf("got %d lines, want 1: %+v", len(e), logs.All())
	}
	f := e[0].ContextMap()
	if f["call"] != "A {}.{}." || f["destination"] != "{}.{}. #ab12" || f["when"] != models.OutsideBeforeFirstTest || f["next_step"] != misses[0].NextStep || f["test-set"] != "test-set-0" {
		t.Errorf("the line reads %+v", f)
	}

	logs.TakeAll()
	var many []models.UnmatchedCall
	for i := 0; i < outsideLogMax+3; i++ {
		many = append(many, models.UnmatchedCall{Protocol: "HTTP", ActualSummary: fmt.Sprintf("GET /c%d", i), Destination: fmt.Sprintf("10.0.0.%d:80", i), BeforeSet: true})
	}
	r.logOutsideTestMisses("test-set-0", models.OutsideTestMissesOf(many))
	if n := logs.Len(); n != outsideLogMax+1 {
		t.Fatalf("got %d lines for %d misses, want %d", n, len(many), outsideLogMax+1)
	}
	if more := logs.FilterMessageSnippet("more calls").All(); len(more) != 1 || more[0].ContextMap()["more"] != int64(3) {
		t.Fatalf("the last line does not say 3 more: %+v", more)
	}
}
