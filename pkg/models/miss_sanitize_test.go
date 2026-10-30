package models

import (
	"reflect"
	"testing"
	"time"
)

// Evidence keeps a miss as Sanitized writes it, and keploy mock record's
// judging reads only that (the agent's per-test evidence, the receipt's
// uncharged misses). Sanitized copies one field at a time, so a field it
// drops is lost with nothing failing: dropping Shape once took every 0 -> 1
// copy of a background series off judging under keploy mock record, and
// the series went unmarked. Every field is carried as it is, carried in its
// shape (sanitizedShapedFields), or dropped for the reason
// sanitizedDroppedFields gives; a field added to UnmatchedCall is carried
// or fails here until one of those lists names it.
func TestSanitizedCarriesEveryFieldOfTheMissButTheNamedOnes(t *testing.T) {
	var call UnmatchedCall
	fillMissFields(t, reflect.ValueOf(&call).Elem())
	// The variable phase keeps FieldDiffs (in their shape), and only a
	// path place of a variable stays (Sanitized's own comments).
	call.MatchPhase = MatchPhaseVariable
	call.VariablePlaces = []string{"url.path[2]"}
	// A summary MissShape can read: a method and a URL.
	call.Protocol, call.ActualSummary = "HTTP", "GET /v1/items/42"
	got := call.Sanitized()

	from, to := reflect.ValueOf(call), reflect.ValueOf(got)
	typ := from.Type()
	for i := 0; i < typ.NumField(); i++ {
		name := typ.Field(i).Name
		_, shaped := sanitizedShapedFields[name]
		_, dropped := sanitizedDroppedFields[name]
		switch {
		case shaped && dropped:
			t.Errorf("%s is on both lists", name)
		case dropped:
			if !to.Field(i).IsZero() {
				t.Errorf("%s is on the dropped list (%s), but Sanitized keeps %v", name, sanitizedDroppedFields[name], to.Field(i).Interface())
			}
		case shaped:
			if to.Field(i).IsZero() {
				t.Errorf("Sanitized dropped the miss's %s, which it should keep in its shape (%s)", name, sanitizedShapedFields[name])
			}
		default:
			if !reflect.DeepEqual(to.Field(i).Interface(), from.Field(i).Interface()) {
				t.Errorf("Sanitized dropped or changed the miss's %s: got %v, want %v; carry it, or name it on a list with the reason", name, to.Field(i).Interface(), from.Field(i).Interface())
			}
		}
	}
	for _, list := range []map[string]string{sanitizedShapedFields, sanitizedDroppedFields} {
		for name := range list {
			if _, ok := typ.FieldByName(name); !ok {
				t.Errorf("a list names %s, which UnmatchedCall no longer has", name)
			}
		}
	}
}

// sanitizedShapedFields are the fields Sanitized keeps, but not as they
// are: their shape, without the call's values.
var sanitizedShapedFields = map[string]string{
	"ActualSummary":  "MissShape: the call's method and target, statement or command, values aside",
	"NextSteps":      "SanitizedNextStep: the advice with every field path it names in its shape",
	"Differs":        "FieldShapes: the differing fields' paths in their shape",
	"FieldReasons":   "FieldReasonShapes: why a field was refused, its path in its shape",
	"VariablePlaces": "only a path place, which names a segment, never a value",
	"FieldDiffs":     "only for a variable miss, the path in its shape and the kind, never the values",
}

// sanitizedDroppedFields are the fields Sanitized does not keep, and why.
var sanitizedDroppedFields = map[string]string{
	"Diff":           "the diff holds the call's and the recording's values",
	"ClosestMockReq": "the closest recording's request holds its values",
	"ReceivedReq":    "the live request holds the call's values",
	"ReachesTest":    "names the word of the test's answer that reached it: a value",
	// No value in these four, but evidence has not kept them since before
	// this list existed; a reader of the evidence that needs one takes it
	// off this list, with a row.
	"DestinationScope": "not kept in evidence: keploy test's report carries it from the agent's miss",
	"CandidateCount":   "not kept in evidence: keploy test's report carries it from the agent's miss",
	"BeforeSet":        "not kept in evidence: keploy test reads it off the agent's miss (Kind says MissOutsideTest in both)",
	"BackgroundWhy":    "not kept in evidence: keploy test's replay writes it",
}

// fillMissFields sets every field of the struct v to a value other than
// its zero, so a field a copy drops is told from one it carries.
func fillMissFields(t *testing.T, v reflect.Value) {
	t.Helper()
	for i := 0; i < v.NumField(); i++ {
		f := v.Field(i)
		switch {
		case f.Type() == reflect.TypeOf(time.Time{}):
			f.Set(reflect.ValueOf(time.Date(2026, 10, 30, 9, 0, i, 0, time.UTC)))
		case f.Kind() == reflect.String:
			f.SetString(v.Type().Field(i).Name)
		case f.Kind() == reflect.Int:
			f.SetInt(int64(i + 1))
		case f.Kind() == reflect.Uint32:
			f.SetUint(uint64(i + 1))
		case f.Kind() == reflect.Float64:
			f.SetFloat(float64(i) + 0.5)
		case f.Kind() == reflect.Bool:
			f.SetBool(true)
		case f.Kind() == reflect.Slice && f.Type().Elem().Kind() == reflect.String:
			f.Set(reflect.ValueOf([]string{v.Type().Field(i).Name}).Convert(f.Type()))
		case f.Kind() == reflect.Slice && f.Type().Elem().Kind() == reflect.Struct:
			s := reflect.MakeSlice(f.Type(), 1, 1)
			fillMissFields(t, s.Index(0))
			f.Set(s)
		case f.Kind() == reflect.Pointer && f.Type().Elem().Kind() == reflect.Struct:
			p := reflect.New(f.Type().Elem())
			fillMissFields(t, p.Elem())
			f.Set(p)
		default:
			t.Fatalf("fillMissFields: field %s of kind %s: teach it", v.Type().Field(i).Name, f.Kind())
		}
	}
}
