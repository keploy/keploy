package learn

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

// mainAnswerWords is main's readAnswerWords of a recording whose kind holds
// its answer apart: one list of every answer field's forms, its answer's
// own clock reads blanked in each form.
func mainAnswerWords(x Raw, read func([]string) []map[string]bool) []map[string]bool {
	xb := x.payload()
	text := blankStamps(xb.text, xb.stamps)
	keys := clockKeys(x)
	var forms []string
	for _, f := range xb.fields {
		if f.end > len(text) || !answerPath(f.path) {
			continue
		}
		for _, form := range textForms(text[f.start:f.end]) {
			if len(keys) > 0 {
				form = blankSpans(form, func(sp instant) bool { return sp.keyed && keys[sp.key] })
			}
			forms = append(forms, form)
		}
	}
	return read(forms)
}

// An outside recording's answer words are its answer fields' words, each
// field's read once for the test (rawTokens): a recording of no set, handed
// to every later call of the test, tokenizes no field again, and a view of
// it that holds the same values but one tokenizes that one field alone. The
// words are main's, in every reading, at two sizes; a protocol-phase answer
// still has its own clock reads blanked; a field past the text is skipped,
// as main skips it. Counted, not timed: tokenizing such a recording's answer
// again in each call was most of a slow call's time.
func TestOutcomeCostAnAnswerReadAgainTokenizesNoFieldAgain(t *testing.T) {
	lists, read := 0, tokensOfForms
	t.Cleanup(func() { tokensOfForms = read })
	tokensOfForms = func(f []string) []map[string]bool { lists++; return read(f) }
	answerWords := func(dc *DriftCache, x Raw, keep bool) (got []map[string]bool, panicked any) {
		defer func() { panicked = recover() }()
		return dc.readAnswerWords(x, keep), nil
	}
	bodyOf := func(n, changed int) string {
		var b strings.Builder
		b.WriteString(`{"items":[`)
		for i := range n {
			if i > 0 {
				b.WriteByte(',')
			}
			v := fmt.Sprintf("tok_a-%d-b_c Item%d", i, i)
			if i == changed {
				v = fmt.Sprintf("tok_z-%d-y_x Other%d", i, i)
			}
			fmt.Fprintf(&b, `{"ref":"Ref-%05d","note":%q}`, i, v)
		}
		b.WriteString(`]}`)
		return b.String()
	}
	for _, n := range []int{4, 40} {
		tr := NewTracker()
		tr.NewSet()
		tr.Begin()
		dc := tr.Drifts()
		x := RawOf(getAnswer("mock-1", "http://api/items", bodyOf(n, -1), eRecAt), true)
		want := mainAnswerWords(x, read)
		texts := map[string]bool{}
		xb := x.payload()
		blanked := blankStamps(xb.text, xb.stamps)
		for _, f := range xb.fields {
			if answerPath(f.path) {
				texts[blanked[f.start:f.end]] = true
			}
		}
		lists = 0
		first, p := answerWords(dc, x, false)
		if p != nil || !reflect.DeepEqual(first, want) {
			t.Errorf("%d items, first read: words %v (panic %v); main's %v", n, first, p, want)
		}
		if lists != len(texts) {
			t.Errorf("%d items, first read tokenized %d texts; want each of its %d answer field texts once", n, lists, len(texts))
		}
		lists = 0
		again, p := answerWords(dc, x, false)
		if p != nil || !reflect.DeepEqual(again, want) {
			t.Errorf("%d items, read again: words %v (panic %v); main's %v", n, again, p, want)
		}
		if lists != 0 {
			t.Errorf("%d items, read again in a later call: tokenized %d texts; want none", n, lists)
		}
		view := RawOf(getAnswer("mock-1", "http://api/items", bodyOf(n, n/2), eRecAt), true)
		lists = 0
		got, p := answerWords(dc, view, false)
		if want := mainAnswerWords(view, read); p != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("%d items, a view changing one value: words %v (panic %v); main's %v", n, got, p, want)
		}
		if lists != 1 {
			t.Errorf("%d items, a view changing one value tokenized %d texts; want the changed one alone", n, lists)
		}
		// A field past the text is no field main reads.
		bad := x.withoutStamps()
		path := ""
		for _, f := range bad.rFields {
			if answerPath(f.path) {
				path = f.path
			}
		}
		bad.rFields = append(append([]rawField(nil), bad.rFields...), rawField{start: len(bad.rText) - 1, end: len(bad.rText) + 4, path: path})
		got, p = answerWords(dc, bad, false)
		if want := mainAnswerWords(bad, read); p != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("%d items, a field past the text: words %v (panic %v); main's %v", n, got, p, want)
		}
		tr.End()
	}
	// A protocol-phase answer's own clock read is no word of it.
	at := time.Date(2026, 11, 18, 10, 0, 0, 123000000, time.UTC)
	tr := NewTracker()
	tr.NewSet()
	tr.Begin()
	defer tr.End()
	x := RawOf(getAnswer("mock-hello", "http://api/hello", `{"localTime":"2026-11-18T10:00:00.123Z","note":"Hello7"}`, eRecAt), true).withoutStamps()
	x.rClock = []time.Time{at}
	want := mainAnswerWords(x, read)
	if want[0]["Hello7"] != true || want[0]["123Z"] {
		t.Fatalf("the row's answer: main's words %v; want Hello7 and not the clock read's", want)
	}
	if got, p := answerWords(tr.Drifts(), x, false); p != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("a protocol-phase answer: words %v (panic %v); main's, its clock read blanked: %v", got, p, want)
	}
}
