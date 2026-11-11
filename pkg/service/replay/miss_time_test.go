package replay

import (
	"reflect"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// The report made from a miss the agent returned keeps when the call missed,
// as the miss does (UnmatchedCall.At): enterprise's mock report says by it
// which test made the call. It was dropped here, so every report said zero.
func TestTheMismatchReportKeepsTheTimeOfTheMiss(t *testing.T) {
	at := time.Date(2026, 10, 7, 9, 0, 0, 0, time.UTC)
	s := NewTestFailureStore()
	s.AddUnmatchedCallForTest("test-set-0", "test-1", models.UnmatchedCall{At: at, Protocol: "HTTP", ActualSummary: "GET /orders"})
	failures := s.GetFailures()
	if len(failures) != 1 || failures[0].MismatchReport == nil {
		t.Fatalf("want one failure with a report, got %+v", failures)
	}
	if got := failures[0].MismatchReport.At; !got.Equal(at) {
		t.Fatalf("the report says the call missed at %v, want %v", got, at)
	}
}

// The report is the miss field for field. It was copied one field at a time,
// and a field added to both (At, keploy#4655) was copied into the miss and
// not back, so the report lost it with nothing failing; a field the two name
// apart (the miss's Held, the report's HeldPastWindow) was dropped the same
// way, so every report said a held call was not held. Every field of the miss
// is carried, under its own name or the one reportFieldOf gives, or is one
// the report has no place for (notInTheReport, with why); every field of the
// report is fed by one of the miss, or is one no miss carries
// (notFromTheMiss, with why). A field added to either is carried or fails
// here, and so does a waiver or a rename that names no such field.
func TestTheMismatchReportCarriesEveryFieldOfTheMiss(t *testing.T) {
	var call models.UnmatchedCall
	fillEveryField(reflect.ValueOf(&call).Elem())
	s := NewTestFailureStore()
	s.AddUnmatchedCallForTest("test-set-0", "test-1", call)
	report := s.GetFailures()[0].MismatchReport

	from, to := reflect.ValueOf(call), reflect.ValueOf(*report)
	fed := map[string]bool{}
	for i := 0; i < from.NumField(); i++ {
		name := from.Type().Field(i).Name
		target := name
		if r, ok := reportFieldOf[name]; ok {
			target = r
		}
		got := to.FieldByName(target)
		if !got.IsValid() {
			if _, waived := notInTheReport[name]; !waived {
				t.Errorf("the miss's %s has no field of the report (%s) to carry it, and notInTheReport does not say why: carry it, or waive it with the reason", name, target)
			}
			continue
		}
		if why, waived := notInTheReport[name]; waived {
			t.Errorf("notInTheReport waives the miss's %s (%q), but the report has a field %s for it: carry it and drop the waiver", name, why, target)
		}
		fed[target] = true
		if !reflect.DeepEqual(got.Interface(), from.Field(i).Interface()) {
			t.Errorf("the report dropped the miss's %s (as its %s): got %v, want %v", name, target, got.Interface(), from.Field(i).Interface())
		}
	}
	for i := 0; i < to.NumField(); i++ {
		name := to.Type().Field(i).Name
		_, waived := notFromTheMiss[name]
		switch {
		case fed[name] && waived:
			t.Errorf("notFromTheMiss waives the report's %s, but a field of the miss feeds it: drop the waiver", name)
		case !fed[name] && !waived:
			t.Errorf("the report's %s is fed by no field of the miss, and notFromTheMiss does not say why: name the miss's field in reportFieldOf, or waive it with the reason", name)
		}
	}
	for name := range notFromTheMiss {
		if !to.FieldByName(name).IsValid() {
			t.Errorf("notFromTheMiss names %s, no field of the report any more: drop the stale waiver", name)
		}
	}
	for name, target := range reportFieldOf {
		if !from.FieldByName(name).IsValid() || !to.FieldByName(target).IsValid() {
			t.Errorf("reportFieldOf carries the miss's %s as the report's %s, but one of the two is no field any more: drop it", name, target)
		}
	}
}

// reportFieldOf names, for each field of the miss (models.UnmatchedCall)
// that the report (models.MockMismatchReport) carries under another name,
// the report's.
var reportFieldOf = map[string]string{
	"Kind": "MissKind",
	"Held": "HeldPastWindow",
}

// notInTheReport names each field of the miss that the report has no field
// for, with why. The test's result keeps the miss whole
// (FailureInfo.UnmatchedCalls), so none of them is lost to the test's verdict.
var notInTheReport = map[string]string{
	"LiveDestination": "the address the application dialled where the report's Destination names the recording's, as the call was matched against it",
	"Instance":        "the test worker that made the call: the failure names the test it is charged to (TestID)",
	"BeforeSet":       "where the call was made (before the set's first test): the replay fails the set with it from the miss, never from the report",
	"EarlierSetTest":  "where the call was made (an earlier set's test): the replay fails the run with it from the miss, never from the report",
	"EarlierSet":      "where the call was made (sets back): the replay names that set from the miss, never from the report",
	"ChargedTo":       "the test the agent charged the call to: the failure names it (TestID)",
	"WindowOf":        "the test whose window a call charged to no test was made in: keploy mock's receipt lists it by that name, from the miss",
	"Marked":          "the agent's own mark of the call's round, never on the wire (json:\"-\")",
	"OrderDependent":  "set by keploy mock's evidence on its own copy of the miss, never on the replay's",
	"EarlierIn":       "named on the test's result's copy of the miss once the run's tests are read (markRecordedIn), after this report is made",
}

// notFromTheMiss names each field of the report that no field of the miss
// feeds, with why.
var notFromTheMiss = map[string]string{
	"Mark":         "the parser's mark of the call's round, read by the agent's routing, never on the wire (json:\"-\")",
	"MarkedInTest": "part of the parser's round mark (Mark), never on the wire",
	"MarkEpoch":    "part of the parser's round mark (Mark), never on the wire",
	"MarkBounds":   "part of the parser's round mark (Mark), never on the wire",
	"MarkRound":    "part of the parser's round mark (Mark), never on the wire",
	"MarkReads":    "part of the parser's round mark (Mark), never on the wire",
	"MarkedTest":   "part of the parser's round mark (Mark), never on the wire",
}

// A call a parser held past its window for a later test's recording, which
// none answered (models.UnmatchedCall.Held), says so in its failure's report
// (HeldPastWindow), which enterprise's mock report copies. It was not copied,
// so every report said such a call was not held.
func TestTheMismatchReportOfAHeldCallSaysItWasHeld(t *testing.T) {
	s := NewTestFailureStore()
	s.AddUnmatchedCallForTest("test-set-0", "test-1", models.UnmatchedCall{Protocol: "HTTP", ActualSummary: "POST /held", Held: true})
	s.AddUnmatchedCallForTest("test-set-0", "test-2", models.UnmatchedCall{Protocol: "HTTP", ActualSummary: "POST /not-held"})
	failures := s.GetFailures()
	if len(failures) != 2 || failures[0].MismatchReport == nil || failures[1].MismatchReport == nil {
		t.Fatalf("want two failures with a report each, got %+v", failures)
	}
	if !failures[0].MismatchReport.HeldPastWindow {
		t.Errorf("the report of a call held past its window says it was not held")
	}
	if failures[1].MismatchReport.HeldPastWindow {
		t.Errorf("the report of a call not held says it was held")
	}
}

// fillEveryField sets every field of the struct v to a value other than its
// zero, so a field a copy drops is told from one it carries.
func fillEveryField(v reflect.Value) {
	for i := 0; i < v.NumField(); i++ {
		f := v.Field(i)
		switch {
		case f.Type() == reflect.TypeOf(time.Time{}):
			f.Set(reflect.ValueOf(time.Date(2026, 10, 7, 9, 0, i, 0, time.UTC)))
		case f.Kind() == reflect.String:
			f.SetString(v.Type().Field(i).Name)
		case f.Kind() == reflect.Int:
			f.SetInt(int64(i + 1))
		case f.Kind() == reflect.Uint32, f.Kind() == reflect.Uint64:
			f.SetUint(uint64(i + 1))
		case f.Kind() == reflect.Bool:
			f.SetBool(true)
		case f.Kind() == reflect.Float64:
			f.SetFloat(float64(i) + 0.5)
		case f.Kind() == reflect.Slice:
			s := reflect.MakeSlice(f.Type(), 1, 1)
			if s.Index(0).Kind() == reflect.Struct {
				fillEveryField(s.Index(0))
			}
			f.Set(s)
		case f.Kind() == reflect.Struct:
			fillEveryField(f)
		case f.Kind() == reflect.Pointer && f.Type().Elem().Kind() == reflect.Struct:
			p := reflect.New(f.Type().Elem())
			fillEveryField(p.Elem())
			f.Set(p)
		default:
			panic("fillEveryField: a field of kind " + f.Kind().String() + " (" + v.Type().Field(i).Name + "): teach it")
		}
	}
}

// The report keeps a background miss's bound places as its own copy
// (BackgroundBound): the replay goes on reading and rewriting the miss it
// was made from, and a report that shared the slice would change with it.
func TestTheMismatchReportCopiesTheBoundPlacesOfTheMiss(t *testing.T) {
	bound := []models.BackgroundBinding{{Recording: "mock-469", Place: "body.docs.msgs.0.q.app_id", BoundMock: "mock-326", BoundPlace: "body.docs.msgs.0.appId", BoundTest: "TestAppCRUD"}}
	want := bound[0]
	s := NewTestFailureStore()
	s.AddUnmatchedCallForTest("test-set-0", "test-1", models.UnmatchedCall{Protocol: "Redis", Command: "GET", Background: true, BackgroundBound: bound})
	bound[0].Place = "body.rewritten"

	report := s.GetFailures()[0].MismatchReport
	if report.Command != "GET" {
		t.Errorf("the report names the call %q, want GET", report.Command)
	}
	if len(report.BackgroundBound) != 1 || report.BackgroundBound[0] != want {
		t.Errorf("the report's bound places are %+v, want its own copy of %+v", report.BackgroundBound, want)
	}
}
