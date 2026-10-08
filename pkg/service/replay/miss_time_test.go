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
// not back, so the report lost it with nothing failing. Every field the two
// share is carried; a field added to both is carried or fails here.
func TestTheMismatchReportCarriesEveryFieldOfTheMiss(t *testing.T) {
	var call models.UnmatchedCall
	fillEveryField(reflect.ValueOf(&call).Elem())
	s := NewTestFailureStore()
	s.AddUnmatchedCallForTest("test-set-0", "test-1", call)
	report := s.GetFailures()[0].MismatchReport

	from, to := reflect.ValueOf(call), reflect.ValueOf(*report)
	for i := 0; i < from.NumField(); i++ {
		name := from.Type().Field(i).Name
		got := to.FieldByName(name)
		if !got.IsValid() {
			continue // the report has no such field
		}
		if !reflect.DeepEqual(got.Interface(), from.Field(i).Interface()) {
			t.Errorf("the report dropped the miss's %s: got %v, want %v", name, got.Interface(), from.Field(i).Interface())
		}
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
		case f.Kind() == reflect.Slice:
			s := reflect.MakeSlice(f.Type(), 1, 1)
			if s.Index(0).Kind() == reflect.Struct {
				fillEveryField(s.Index(0))
			}
			f.Set(s)
		case f.Kind() == reflect.Struct:
			fillEveryField(f)
		default:
			panic("fillEveryField: a field of kind " + f.Kind().String() + " (" + v.Type().Field(i).Name + "): teach it")
		}
	}
}
