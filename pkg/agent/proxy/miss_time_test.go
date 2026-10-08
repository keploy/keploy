package proxy

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// The report a parser built is the parser's: the time is put on a copy.
func TestAMissWithoutATimeIsStampedOnACopy(t *testing.T) {
	report := &models.MockMismatchReport{Protocol: "SQS", ActualSummary: "SendMessage q"}
	p := &Proxy{logger: zap.NewNop(), errChannel: make(chan error, 4)}
	p.BeginTestErrorCapture()
	p.sendMockNotFoundError(models.NewMockMismatchError(models.ErrNoMockMatched, report))
	if got, _ := p.GetMockErrors(context.Background()); len(got) != 1 || got[0].At.IsZero() {
		t.Fatalf("want the one miss, with a time, got %+v", got)
	}
	if !report.At.IsZero() {
		t.Fatalf("the parser's own report was changed: At = %v", report.At)
	}
}

// The miss the agent returns is its report field for field: every field the
// two share is carried from one to the other (see the reverse in
// pkg/service/replay), so a field added to both is carried or fails here.
func TestAMissCarriesEveryFieldOfItsReport(t *testing.T) {
	// Both ways through sendMockNotFoundError: a report with its time, and one
	// without, which it stamps on a copy -- that copy carries the rest.
	for _, stamped := range []bool{false, true} {
		var report models.MockMismatchReport
		fillEveryField(reflect.ValueOf(&report).Elem())
		if stamped {
			report.At = time.Time{}
		}
		p := &Proxy{logger: zap.NewNop(), errChannel: make(chan error, 4)}
		p.BeginTestErrorCapture()
		p.sendMockNotFoundError(models.NewMockMismatchError(models.ErrNoMockMatched, &report))
		got, err := p.GetMockErrors(context.Background())
		if err != nil || len(got) != 1 {
			t.Fatalf("stamped=%v: want the one miss, got %+v (%v)", stamped, got, err)
		}
		from, to := reflect.ValueOf(report), reflect.ValueOf(got[0])
		for i := 0; i < from.NumField(); i++ {
			name := from.Type().Field(i).Name
			field := to.FieldByName(name)
			if !field.IsValid() || (stamped && name == "At") {
				continue // the miss has no such field, or its time is the stamp
			}
			if !reflect.DeepEqual(field.Interface(), from.Field(i).Interface()) {
				t.Errorf("stamped=%v: the miss dropped its report's %s: got %v, want %v", stamped, name, field.Interface(), from.Field(i).Interface())
			}
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

// Every miss says when it happened. `keploy mock` files each miss under the
// test it was made in by that time (FlowMocks), and one without it is dropped
// from every test; enterprise's mock report uses it to say which test made the
// call.
// A parser that built its report without a time, and a protocol whose matcher
// builds none, left the time zero.
func TestEveryMissSaysWhenItHappened(t *testing.T) {
	recorded := time.Date(2026, 10, 7, 9, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name     string
		err      error
		want     func(before, after, got time.Time) bool
		wantDesc string
	}{
		{
			"a report with its time keeps it",
			models.NewMockMismatchError(models.ErrNoMockMatched, &models.MockMismatchReport{At: recorded, Protocol: "HTTP", ActualSummary: "GET /a"}),
			func(_, _, got time.Time) bool { return got.Equal(recorded) },
			"the time the parser recorded, " + recorded.String(),
		},
		{
			"a report without one gets the time it was sent",
			models.NewMockMismatchError(models.ErrNoMockMatched, &models.MockMismatchReport{Protocol: "SQS", ActualSummary: "SendMessage q"}),
			func(before, after, got time.Time) bool { return !got.Before(before) && !got.After(after) },
			"the time it was sent",
		},
		{
			// What a parser with no report to give sends: keploy/integrations'
			// mongo v2 for a request it cannot deliver.
			"a miss with no report gets the time it was sent",
			fmt.Errorf("mongo: the request is undeliverable: %w", models.ErrNoMockMatched),
			func(before, after, got time.Time) bool { return !got.Before(before) && !got.After(after) },
			"the time it was sent",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &Proxy{logger: zap.NewNop(), errChannel: make(chan error, 4)}
			p.BeginTestErrorCapture()
			before := time.Now()
			p.sendMockNotFoundError(tc.err)
			after := time.Now()
			got, err := p.GetMockErrors(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != 1 {
				t.Fatalf("want the one miss, got %+v", got)
			}
			if !tc.want(before, after, got[0].At) {
				t.Fatalf("the miss says it happened at %v, want %s (sent between %v and %v)", got[0].At, tc.wantDesc, before, after)
			}
		})
	}
}
