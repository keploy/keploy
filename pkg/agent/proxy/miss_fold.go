package proxy

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// missFolds is what a testErrorAccumulator folded into the misses it holds
// or spilled once their room was full (foldAlike): each such miss by its
// alike key (missAlikeKey), and, by its place, the times of the misses folded
// into it, in the order they were.
type missFolds struct {
	held map[string]foldTarget
	at   map[foldTarget][]time.Time
}

// foldTarget is where a miss misses fold into is: its index in its
// accumulator's list, or, spilled, its place on the spill (missSpill.keep).
type foldTarget struct {
	spilled bool
	i       int
}

// keepPast keeps err, a miss past its accumulator's room: folded into the
// first miss held or spilled it is alike to (foldAlike), or else kept on the
// accumulator's spill, where the misses alike to it fold into it. It
// returns false when err was not kept: the spill is full, or err is no
// parser's miss (missSpill.keep). Under a.mu.
func (a *testErrorAccumulator) keepPast(err error) bool {
	key, alike := missAlikeKey(err)
	if alike && a.foldAlike(key, err) {
		return true
	}
	if a.spill == nil {
		a.spill = &missSpill{}
	}
	i, kept := a.spill.keep(err)
	if kept && alike {
		a.folds.held[key] = foldTarget{spilled: true, i: i}
	}
	return kept
}

// foldAlike folds err, a miss past its accumulator's room whose alike key is
// key (missAlikeKey), into the first miss held or spilled it is alike to:
// err's time, and the times folded into err before (a miss moved from one
// list to another), join that miss's. It returns false, folding nothing, for
// a miss alike to none. Under a.mu; a.folds is set after it.
func (a *testErrorAccumulator) foldAlike(key string, err error) bool {
	if a.folds == nil {
		a.folds = &missFolds{held: make(map[string]foldTarget, len(a.errs)), at: map[foldTarget][]time.Time{}}
		for i, e := range a.errs {
			if k, ok := missAlikeKey(e); ok {
				if _, dup := a.folds.held[k]; !dup {
					a.folds.held[k] = foldTarget{i: i}
				}
			}
		}
	}
	at, ok := a.folds.held[key]
	if !ok {
		return false
	}
	pe, _, _ := filedMiss(err)
	a.folds.at[at] = append(append(a.folds.at[at], pe.MismatchReport.At), pe.MismatchReport.AlikeAt...)
	return true
}

// applyTo writes into errs and spilled, the misses f's were folded into, in
// the list and read back from its spill, the times folded into each
// (withAlikeAt). A nil f folded nothing; a spilled miss not read back
// (missSpill.readBack) takes none.
func (f *missFolds) applyTo(errs, spilled []error) {
	if f == nil {
		return
	}
	for t, at := range f.at {
		into := errs
		if t.spilled {
			into = spilled
		}
		if t.i < len(into) {
			into[t.i] = withAlikeAt(into[t.i], at)
		}
	}
}

// filedMiss is the miss err holds, as the proxy files one: bare, or marked as
// made before its test set (beforeSetMiss) or in an earlier set
// (earlierSetMiss), with those marks, outermost first. ok is false for no
// mock-not-found error with a report.
func filedMiss(err error) (pe models.ParserError, ok bool, marks []string) {
	for {
		if b, isBefore := err.(beforeSetMiss); isBefore {
			marks, err = append(marks, "made before its set"), b.error
			continue
		}
		if e, isEarlier := err.(earlierSetMiss); isEarlier {
			marks, err = append(marks, "made in an earlier set"), e.error
			continue
		}
		break
	}
	pe, ok = err.(models.ParserError)
	if !ok || pe.ParserErrorType != models.ErrMockNotFound || pe.MismatchReport == nil {
		return models.ParserError{}, false, nil
	}
	return pe, true, marks
}

// withAlikeAt is err, a miss filed as filedMiss reads one (foldAlike folds
// into no other), with at added to the times folded into it (a copy of its
// report: the report is its parser's).
func withAlikeAt(err error, at []time.Time) error {
	switch e := err.(type) {
	case beforeSetMiss:
		return beforeSetMiss{withAlikeAt(e.error, at)}
	case earlierSetMiss:
		return earlierSetMiss{withAlikeAt(e.error, at)}
	case models.ParserError:
		r := *e.MismatchReport
		r.AlikeAt = append(slices.Clone(r.AlikeAt), at...)
		e.MismatchReport = &r
		return e
	}
	return err
}

// missAlikeKey is what two misses share when they are alike: everything the
// proxy's read (GetMockErrors) lists of a miss, but its time and the values
// it carried. That is the call in its shape (models.MissShape, as the proven
// model's evidence names it, with no value its URL, statement or filter
// held), where it went, its closest recording, how and why it missed, the
// names of the fields it differs in, and its next steps; the test worker
// that made it, where it was made (MissKind, its set's marks) and where its
// parser marked it (every report field no JSON writes, whose values place it
// at a test's end: an alike miss is placed where the miss it folds into is).
// Its time, rendered request, diff and the values of its field diffs are
// left out: a poller's calls differ only there. ok is false for an err
// filedMiss reads as no miss, and for a report keploy cannot write: such a
// miss is never folded.
func missAlikeKey(err error) (string, bool) {
	pe, ok, marks := filedMiss(err)
	if !ok {
		return "", false
	}
	r := *pe.MismatchReport
	r.ActualSummary = models.MissShape(models.UnmatchedCall{Protocol: r.Protocol, ActualSummary: r.ActualSummary, Command: r.Command,
		FieldDiffs: r.FieldDiffs, VariablePlaces: r.VariablePlaces, ReceivedReq: r.ReceivedReq})
	r.At, r.AlikeAt, r.ReceivedReq, r.Diff = time.Time{}, nil, "", ""
	r.FieldDiffs = make([]models.MockFieldDiff, 0, len(pe.MismatchReport.FieldDiffs))
	for _, d := range pe.MismatchReport.FieldDiffs {
		r.FieldDiffs = append(r.FieldDiffs, models.MockFieldDiff{Path: d.Path, Kind: d.Kind})
	}
	body, jerr := json.Marshal(struct {
		Marks     []string
		Kind      string
		Instance  uint32
		Recorded  int
		Staging   bool
		Report    models.MockMismatchReport
		Unwritten string
	}{marks, pe.MissKind, pe.Instance, pe.Recorded, pe.Staging, r, unwrittenFields(r)})
	if jerr != nil {
		return "", false
	}
	sum := sha256.Sum256(body)
	return string(sum[:]), true
}

// unwrittenFields writes every field of r that no JSON writes (json:"-"),
// by name and value: where the call's parser marked it, and any such field
// added later, so a miss placed apart is never alike. missAlikeKey clears
// AlikeAt first.
func unwrittenFields(r models.MockMismatchReport) string {
	v := reflect.ValueOf(r)
	out := ""
	for i := 0; i < v.NumField(); i++ {
		f := v.Type().Field(i)
		if !f.IsExported() || f.Tag.Get("json") != "-" {
			continue
		}
		out += fmt.Sprintf("%s=%#v;", f.Name, v.Field(i).Interface())
	}
	return out
}

// appendFolded appends to calls, whose last is the miss the times at were
// folded into, a call of its own for each: at its time, Folded, the rest
// that miss's.
func appendFolded(calls []models.UnmatchedCall, at []time.Time) []models.UnmatchedCall {
	first := calls[len(calls)-1]
	for _, t := range at {
		c := first
		c.At, c.Folded = t, true
		c.BoundIn = slices.Clone(first.BoundIn)
		c.BackgroundBound = slices.Clone(first.BackgroundBound)
		c.Repeat = first.Repeat.Clone()
		calls = append(calls, c)
	}
	return calls
}
