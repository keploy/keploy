package learn

import (
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"
)

// noSetCache is a cache of no set, whatever mode the package's rows run in
// (everyCacheSet): a row of the call's own field view reads a recording's
// view there, as a call of no set does, never the set's (setFields).
func noSetCache() *DriftCache {
	dc := NewDriftCache()
	dc.set = nil
	return dc
}

// setFieldsFixture is n recordings of a set, each with a view F served of
// it (a Raw with no id, read by its text and fields), and the words a call
// asks of them: one at two fields of each, one at one field of all, one at
// two fields of all, and one none holds.
func setFieldsFixture(n int) (recs, views []Raw, words func(i int) []string) {
	for i := range n {
		at := eRecAt.Add(time.Duration(i) * time.Millisecond)
		recs = append(recs, RawOf(getAnswer(fmt.Sprintf("mock-%d", i), "http://api/items/1",
			fmt.Sprintf(`{"a":"w%dq","b":{"c":"shared x"},"d":["x","w%dq"]}`, i, i), at), false))
		v := RawOf(getAnswer(fmt.Sprintf("view-%d", i), "http://api/items/1",
			fmt.Sprintf(`{"a":"x","b":{"c":"w%dq"},"d":["shared"]}`, i), at), false)
		v.id = ""
		views = append(views, v)
	}
	return recs, views, func(i int) []string { return []string{fmt.Sprintf("w%dq", i), "shared", "x", "absent"} }
}

// askAll is one call of the set: fieldsWith of every word at every
// recording and view, through one index on dc, released at its end, and
// what each answered, in order.
func askAll(dc *DriftCache, recs, views []Raw, words func(int) []string) (got [][]fieldAt) {
	x := newReaderIndex(dc)
	defer x.release()
	for i := range recs {
		for _, r := range []Raw{recs[i], views[i]} {
			t := x.textOf(r)
			for _, w := range words(i) {
				at, read := t.fieldsWith(w)
				if !read {
					at = append(at, fieldAt{path: "unread"})
				}
				got = append(got, at)
			}
		}
	}
	return got
}

// A set builds each recording's field view, and each view's F served,
// once, in the first call that asks it a word: every later call, one at a
// time or eight at once, builds none, so a call's work per earlier place is
// flat in the set's size (50 and 400 places alike), and every answer is
// the one a call's own index gives with no set.
func TestASetBuildsEachFieldViewOnceAtAnySize(t *testing.T) {
	defer func(b int64) { readerBudget = b }(readerBudget)
	readerBudget = 0
	for _, n := range []int{50, 400} {
		recs, views, words := setFieldsFixture(n)
		want := askAll(nil, recs, views, words)
		dc := NewDriftCache()
		dc.set = newSetDerived()
		fs := filtersOf(dc.set)
		if got := askAll(dc, recs, views, words); !slices.EqualFunc(got, want, slices.Equal) {
			t.Fatalf("%d places, first call: answers differ from a call with no set", n)
		}
		if fs.fieldBuilds != 2*n || len(fs.fieldTexts) != 2*n {
			t.Fatalf("%d places, first call: %d builds, %d views; want %d, %d", n, fs.fieldBuilds, len(fs.fieldTexts), 2*n, 2*n)
		}
		var wg sync.WaitGroup
		same := make([]bool, 9)
		for c := range same {
			wg.Add(1)
			go func() {
				defer wg.Done()
				same[c] = slices.EqualFunc(askAll(dc, recs, views, words), want, slices.Equal)
			}()
			if c == 0 {
				wg.Wait()
			}
		}
		wg.Wait()
		if slices.Contains(same, false) {
			t.Errorf("%d places, later calls: answers differ from a call with no set (%v)", n, same)
		}
		if fs.fieldBuilds != 2*n {
			t.Errorf("%d places: later calls built %d views more; want none", n, fs.fieldBuilds-2*n)
		}
		if fs.fieldsUsed <= 0 || fs.fieldsUsed > readerBudgetBytes() {
			t.Errorf("%d places: the set's views count %d bytes; want some, within %d", n, fs.fieldsUsed, readerBudgetBytes())
		}
	}
}

// The set's views in all stay within the cap of one call's index: past it a
// view is not kept, built once and never again, and its calls answer it by
// their own index, as before, with the same answers.
func TestASetsFieldViewsStayWithinOneCallsCap(t *testing.T) {
	defer func(b int64) { readerBudget = b }(readerBudget)
	recs, views, words := setFieldsFixture(40)
	readerBudget = 1 << 40
	want := askAll(nil, recs, views, words)
	one := NewDriftCache()
	one.set = newSetDerived()
	askAll(one, recs[:1], views[:1], words)
	per := filtersOf(one.set).fieldsUsed / 2
	readerBudget = 10 * per
	dc := NewDriftCache()
	dc.set = newSetDerived()
	fs := filtersOf(dc.set)
	for c := range 3 {
		if got := askAll(dc, recs, views, words); !slices.EqualFunc(got, want, slices.Equal) {
			t.Fatalf("call %d: answers differ from a call with no set", c)
		}
	}
	kept := 0
	for _, e := range fs.fieldTexts {
		if e.ok {
			kept++
		}
	}
	if fs.fieldsUsed > readerBudget || kept == 0 || kept >= 80 {
		t.Errorf("views kept %d of 80 in %d bytes; want some, not all, within %d", kept, fs.fieldsUsed, readerBudget)
	}
	if fs.fieldBuilds != 80 {
		t.Errorf("%d builds in three calls; want 80, each view once", fs.fieldBuilds)
	}
}

// A view's text read with other fields is another view: one set entry each,
// each answering as a call with no set does (a text with no fields is one
// field, the zero fieldAt).
func TestASetsViewOfATextReadWithOtherFieldsIsAnother(t *testing.T) {
	_, views, _ := setFieldsFixture(1)
	b := views[0].payload()
	byFields := Raw{mock: "view-0", rText: b.text, rFields: b.fields, rRead: true}
	whole := Raw{mock: "view-0", rText: b.text, rRead: true}
	dc := NewDriftCache()
	dc.set = newSetDerived()
	fs := filtersOf(dc.set)
	for _, v := range []Raw{byFields, whole} {
		want, _ := newReaderIndex(nil).textOf(v).fieldsWith("w0q")
		x := newReaderIndex(dc)
		got, read := x.textOf(v).fieldsWith("w0q")
		x.release()
		if !read || !slices.Equal(got, want) || len(got) != 1 {
			t.Errorf("view with %d fields: %v (read %v); want %v", len(v.rFields), got, read, want)
		}
	}
	if len(fs.fieldTexts) != 2 || fs.fieldBuilds != 2 {
		t.Errorf("%d views by text, %d builds; want 2 and 2", len(fs.fieldTexts), fs.fieldBuilds)
	}
}
