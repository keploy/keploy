package learn

import (
	"encoding/binary"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
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
// two fields of all, and one none holds. Where dc is given, the recordings
// are lazy, read through its set's store of payloads.
func setFieldsFixture(dc *DriftCache, n int) (recs, views []Raw, words func(i int) []string) {
	for i := range n {
		at := eRecAt.Add(time.Duration(i) * time.Millisecond)
		m := getAnswer(fmt.Sprintf("mock-%d", i), "http://api/items/1",
			fmt.Sprintf(`{"a":"w%dq","b":{"c":"shared x"},"d":["x","w%dq"]}`, i, i), at)
		r := RawOf(m, false)
		if dc != nil {
			r = dc.LazyRawOf(m, false, nil)
		}
		recs = append(recs, r)
		v := RawOf(getAnswer(fmt.Sprintf("view-%d", i), "http://api/items/1",
			fmt.Sprintf(`{"a":"x","b":{"c":"w%dq"},"d":["shared"]}`, i), at), false)
		v.id = ""
		views = append(views, v)
	}
	return recs, views, func(i int) []string { return []string{fmt.Sprintf("w%dq", i), "shared", "x", "absent"} }
}

// askAll is one call of the set: fieldsWith of every word at every
// recording and view, through one index on dc, released at its end. got is
// what each answered, in order; used, what the call's index kept by its own
// count at its end; held, the questions after which a place still held a
// payload read for one.
func askAll(dc *DriftCache, recs, views []Raw, words func(int) []string) (got [][]fieldAt, used int64, held int) {
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
				if t.held != nil {
					held++
				}
				got = append(got, at)
			}
		}
	}
	return got, x.used, held
}

// setUsed is what s counts on its budget.
func setUsed(s *setDerived) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.budget.used
}

// keptViews are the field views fs keeps.
func keptViews(fs *setFilters) (kept int) {
	for _, e := range fs.fieldTexts {
		if e.ok {
			kept++
		}
	}
	return kept
}

// A set builds each recording's field view, and each view's F served,
// once, in the first call that asks it a word: every later call, one at a
// time or eight at once, builds none, so a call's work per earlier place is
// flat in the set's size (50 and 400 places alike), and every answer is
// the one a call's own index gives with no set. Every call reads each
// recording once, whatever it asks of it, and keeps nothing of its own; the
// set counts each view and its entry, once.
func TestASetBuildsEachFieldViewOnceAtAnySize(t *testing.T) {
	defer func(b, r int64) { bodyBudget, readerBudget = b, r }(bodyBudget, readerBudget)
	bodyBudget, readerBudget = 0, 0
	for _, n := range []int{50, 400} {
		dc := NewDriftCache()
		dc.set = newSetDerived()
		fs := filtersOf(dc.set)
		recs, views, words := setFieldsFixture(dc, n)
		want, _, _ := askAll(nil, recs, views, words)
		loads, u := dc.Loads(), setUsed(dc.set)
		if got, used, held := askAll(dc, recs, views, words); !slices.EqualFunc(got, want, slices.Equal) || used != 0 || held != 0 {
			t.Fatalf("%d places, first call: answers as a call with no set %v, %d bytes the call's own, %d payloads held past a question; want the same answers, 0, 0", n, slices.EqualFunc(got, want, slices.Equal), used, held)
		}
		if fs.fieldBuilds != 2*n || len(fs.fieldTexts) != 2*n || keptViews(fs) != 2*n {
			t.Fatalf("%d places, first call: %d builds, %d views, %d kept; want %d each", n, fs.fieldBuilds, len(fs.fieldTexts), keptViews(fs), 2*n)
		}
		if l := dc.Loads() - loads; l != n {
			t.Errorf("%d places, first call: %d loads; want %d, one a recording", n, l, n)
		}
		counted := int64(2*n)*(fieldsEntryBytes+fieldsTextSlotBytes) + fieldsTextGroupBytes + fs.fieldsUsed
		if d := setUsed(dc.set) - u; d != counted {
			t.Errorf("%d places, first call: the set counts %d bytes more; want %d, the views and their entries", n, d, counted)
		}
		loads, u = dc.Loads(), setUsed(dc.set)
		if got, used, held := askAll(dc, recs, views, words); !slices.EqualFunc(got, want, slices.Equal) || used != 0 || held != 0 || dc.Loads()-loads != n {
			t.Errorf("%d places, second call: answers as a call with no set %v, %d bytes the call's own, %d payloads held past a question, %d loads; want the same answers, 0, 0, %d", n, slices.EqualFunc(got, want, slices.Equal), used, held, dc.Loads()-loads, n)
		}
		// Eight at once share a load of one payload (bodyStore.get): at most
		// one a recording each.
		loads = dc.Loads()
		var wg sync.WaitGroup
		same := make([]bool, 8)
		for c := range same {
			wg.Add(1)
			go func() {
				defer wg.Done()
				got, used, held := askAll(dc, recs, views, words)
				same[c] = slices.EqualFunc(got, want, slices.Equal) && used == 0 && held == 0
			}()
		}
		wg.Wait()
		if slices.Contains(same, false) {
			t.Errorf("%d places, eight calls at once: answers as a call with no set, nothing of the call's own, no payload held (%v)", n, same)
		}
		if fs.fieldBuilds != 2*n || dc.Loads()-loads > 8*n || setUsed(dc.set) != u {
			t.Errorf("%d places, 9 later calls: %d views built more, %d loads at once, %d bytes counted more; want none, at most %d, none", n, fs.fieldBuilds-2*n, dc.Loads()-loads, setUsed(dc.set)-u, 8*n)
		}
		if fs.fieldsUsed <= 0 || fs.fieldsUsed > readerBudgetBytes(dc.set) {
			t.Errorf("%d places: the set's views count %d bytes; want some, within %d", n, fs.fieldsUsed, readerBudgetBytes(dc.set))
		}
	}
}

// The set's views in all stay within the cap of one call's index: past it a
// view is not kept, built once and never again, and its calls answer it by
// their own index, as before, with the same answers. A view that alone
// passes the cap is not kept, not even the part of it that fits.
func TestASetsFieldViewsStayWithinOneCallsCap(t *testing.T) {
	defer func(b int64) { readerBudget = b }(readerBudget)
	recs, views, words := setFieldsFixture(nil, 40)
	readerBudget = 1 << 40
	want, _, _ := askAll(nil, recs, views, words)
	one := NewDriftCache()
	one.set = newSetDerived()
	askAll(one, recs[:1], views[:1], words)
	per := filtersOf(one.set).fieldsUsed / 2
	readerBudget = 10 * per
	dc := NewDriftCache()
	dc.set = newSetDerived()
	fs := filtersOf(dc.set)
	for c := range 3 {
		if got, _, _ := askAll(dc, recs, views, words); !slices.EqualFunc(got, want, slices.Equal) {
			t.Fatalf("call %d: answers differ from a call with no set", c)
		}
	}
	if kept := keptViews(fs); fs.fieldsUsed > readerBudget || kept == 0 || kept >= 80 {
		t.Errorf("views kept %d of 80 in %d bytes; want some, not all, within %d", kept, fs.fieldsUsed, readerBudget)
	}
	if fs.fieldBuilds != 80 {
		t.Errorf("%d builds in three calls; want 80, each view once", fs.fieldBuilds)
	}
	readerBudget = per / 2
	dc = NewDriftCache()
	dc.set = newSetDerived()
	fs = filtersOf(dc.set)
	if got, _, _ := askAll(dc, recs, views, words); !slices.EqualFunc(got, want, slices.Equal) {
		t.Errorf("a cap of half a view: answers differ from a call with no set")
	}
	if kept := keptViews(fs); kept != 0 || fs.fieldsUsed != 0 || fs.fieldBuilds != 80 {
		t.Errorf("a cap of half a view: %d views kept in %d bytes, %d built; want none kept, 0, 80 built", kept, fs.fieldsUsed, fs.fieldBuilds)
	}
}

// A call reads a recording once for each question the set keeps no view to
// answer, never twice, and once in all where its own index or the set keeps
// one: the set's views past its cap, kept by the call's index; kept by
// neither; the set's budget full before any view and full once its views
// are kept; room on it for a view's entry and not the view. Every answer is
// the one a call with no set gives, and a place holds no payload past the
// question it was read for, though the call's index answered it from a view
// another place of the text built.
func TestASetsCallReadsARecordingOncePerQuestionItKeepsNoViewOf(t *testing.T) {
	defer func(b, r int64) { bodyBudget, readerBudget = b, r }(bodyBudget, readerBudget)
	bodyBudget = 0
	const n = 20
	fresh := func(budget int64) (*DriftCache, *setFilters, []Raw, []Raw, func(int) []string) {
		readerBudget = budget
		dc := NewDriftCache()
		dc.set = newSetDerived()
		recs, views, words := setFieldsFixture(dc, n)
		return dc, filtersOf(dc.set), recs, views, words
	}
	call := func(what string, dc *DriftCache, recs, views []Raw, words func(int) []string, loads int) {
		t.Helper()
		want, _, _ := askAll(nil, recs, views, words)
		l := dc.Loads()
		got, _, held := askAll(dc, recs, views, words)
		if !slices.EqualFunc(got, want, slices.Equal) || held != 0 || dc.Loads()-l != loads {
			t.Errorf("%s: answers as a call with no set %v, %d payloads held past a question, %d loads; want the same answers, 0, %d", what, slices.EqualFunc(got, want, slices.Equal), held, dc.Loads()-l, loads)
		}
	}
	dc, fs, recs, views, words := fresh(1 << 30)
	fs.fieldsUsed = readerBudgetBytes(dc.set)
	for c := range 2 {
		call(fmt.Sprintf("the set's views past its cap, call %d", c), dc, recs, views, words, n)
	}
	if keptViews(fs) != 0 || fs.fieldBuilds != 2*n {
		t.Errorf("the set's views past its cap: %d kept, %d built; want none kept, each built once", keptViews(fs), fs.fieldBuilds)
	}
	x := newReaderIndex(dc)
	a := x.textOf(recs[0])
	wa, _ := a.fieldsWith("w0q")
	b := x.textOf(recs[0])
	if _, read := b.holdsWord("w0q"); !read || !b.e.fields.ok {
		t.Fatalf("a second place of the text: read %v, its entry keeps the call's view %v; want both", read, b.e.fields.ok)
	}
	if wb, read := b.fieldsWith("w0q"); !read || !slices.Equal(wb, wa) || b.held != nil {
		t.Errorf("a second place of the text, answered from the call's view: %v (read %v), payload still held %v; want %v, none held", wb, read, b.held != nil, wa)
	}
	x.release()

	dc, _, recs, views, words = fresh(1)
	call("a cap of 1 byte", dc, recs, views, words, 4*n)

	dc, fs, recs, views, words = fresh(0)
	dc.set.budget.limit = setUsed(dc.set)
	call("the set's budget full", dc, recs, views, words, 4*n)
	if len(fs.fieldTexts) != 0 || fs.fieldBuilds != 0 {
		t.Errorf("the set's budget full: %d entries, %d builds; want none", len(fs.fieldTexts), fs.fieldBuilds)
	}

	dc, fs, recs, views, words = fresh(0)
	call("the set's first call", dc, recs, views, words, n)
	dc.set.budget.limit = setUsed(dc.set)
	call("the set's budget full once its views are kept", dc, recs, views, words, n)
	if keptViews(fs) != 2*n || fs.fieldBuilds != 2*n {
		t.Errorf("the set's budget full once its views are kept: %d kept, %d built; want %d each", keptViews(fs), fs.fieldBuilds, 2*n)
	}

	dc, fs, recs, views, words = fresh(0)
	limit := setUsed(dc.set) + fieldsEntryBytes + fieldsTextSlotBytes + fieldsTextGroupBytes
	dc.set.budget.limit = limit
	call("room for a view's entry and not the view", dc, recs[:1], views[:1], words, 4)
	if len(fs.fieldTexts) != 1 || keptViews(fs) != 0 || fs.fieldsUsed != 0 || setUsed(dc.set) != limit {
		t.Errorf("room for a view's entry and not the view: %d entries, %d views kept in %d bytes, the set counts %d of %d; want 1, none, 0, all", len(fs.fieldTexts), keptViews(fs), fs.fieldsUsed, setUsed(dc.set), limit)
	}
}

// A recording keploy could not read has no view in the set: a place asks
// it once, whatever it asks, and answers each question unread, though it
// first asked another kind of question.
func TestASetsCallAsksAnUnreadRecordingNoView(t *testing.T) {
	defer func(b int64) { bodyBudget = b }(bodyBudget)
	bodyBudget = 0
	dc := NewDriftCache()
	dc.set = newSetDerived()
	fs := filtersOf(dc.set)
	m := getAnswer("mock-1", "http://api/items/1", `{"a":"w0q"}`, eRecAt)
	r := dc.LazyRawOf(m, false, func() (*models.Mock, error) { return nil, errors.New("gone") })
	x := newReaderIndex(dc)
	defer x.release()
	a := x.textOf(r)
	for range 2 {
		if at, read := a.fieldsWith("w0q"); read || at != nil {
			t.Errorf("an unread recording: %v, read %v; want none, unread", at, read)
		}
	}
	if dc.Loads() != 1 {
		t.Errorf("a place asked an unread recording twice: %d loads; want 1", dc.Loads())
	}
	b := x.textOf(r)
	if _, read := b.holdsWord("w0q"); read {
		t.Fatalf("an unread recording read")
	}
	if at, read := b.fieldsWith("w0q"); read || at != nil || dc.Loads() != 2 {
		t.Errorf("an unread recording asked another question first: %v, read %v, %d loads in all; want none, unread, 2", at, read, dc.Loads())
	}
	if len(fs.fieldTexts) != 0 || fs.fieldBuilds != 0 || x.unreadTexts != 2 {
		t.Errorf("an unread recording: %d views, %d builds, %d places unread; want none, none, 2", len(fs.fieldTexts), fs.fieldBuilds, x.unreadTexts)
	}
}

// A view's text read with other fields is another view: one set entry each,
// each answering as a call with no set does (a text with no fields is one
// field, the zero fieldAt). So is one whose fields differ only in whether
// one is a key, only in where one starts, or only in where one ends, and one
// whose fields' places and paths run, byte by byte, as another's do without
// each path's length.
func TestASetsViewOfATextReadWithOtherFieldsIsAnother(t *testing.T) {
	_, views, _ := setFieldsFixture(nil, 1)
	b := views[0].payload()
	byFields := Raw{mock: "view-0", rText: b.text, rFields: b.fields, rRead: true}
	whole := Raw{mock: "view-0", rText: b.text, rRead: true}
	text := `{"a":"w0q","b":"w1q","c":"w2q"}`
	at := func(s string, key bool, path string) rawField {
		i := strings.Index(text, s)
		return rawField{start: i, end: i + len(s), key: key, path: path}
	}
	byKey := Raw{mock: "key", rText: text, rFields: []rawField{at("a", true, "a"), at("w0q", false, "a")}, rRead: true}
	byValue := Raw{mock: "key", rText: text, rFields: []rawField{at("a", false, "a"), at("w0q", false, "a")}, rRead: true}
	// Without each path's length, a field's place and kind (17 bytes) read
	// as the end of the path before it.
	place := func(f rawField) string {
		var p [17]byte
		binary.LittleEndian.PutUint64(p[0:], uint64(f.start))
		binary.LittleEndian.PutUint64(p[8:], uint64(f.end))
		return string(p[:])
	}
	f1, f2, f3, h := at("w0q", false, ""), at("w1q", false, ""), at("w2q", false, ""), at("c", false, "")
	f1.path, f2.path, f3.path, h.path = "a", "b", place(h)+"c", "c"
	threeRuns := Raw{mock: "runs", rText: text, rFields: []rawField{f1, f2, f3}, rRead: true}
	f1.path, f3.path = "a"+place(f2)+"b", ""
	runsAlike := Raw{mock: "runs", rText: text, rFields: []rawField{f1, f3, h}, rRead: true}
	// Two fields, then the same but the second's start, or its end, a byte
	// over: the word asked is in the first, alike in all three.
	two := []rawField{at("w0q", false, "a"), at("w1q", false, "b")}
	moved := func(start, end int) Raw {
		fl := slices.Clone(two)
		fl[1].start += start
		fl[1].end += end
		return Raw{mock: "two", rText: text, rFields: fl, rRead: true}
	}
	dc := NewDriftCache()
	dc.set = newSetDerived()
	fs := filtersOf(dc.set)
	for _, c := range []struct {
		what string
		v    Raw
		w    string
	}{{"by fields", byFields, "w0q"}, {"whole", whole, "w0q"}, {"a key", byKey, "w0q"}, {"its value", byValue, "w0q"},
		{"three fields", threeRuns, "w0q"}, {"three fields that run alike", runsAlike, "w0q"},
		{"two fields", moved(0, 0), "w0q"}, {"two fields, the second started earlier", moved(-1, 0), "w0q"},
		{"two fields, the second ended later", moved(0, 1), "w0q"}} {
		want, _ := newReaderIndex(nil).textOf(c.v).fieldsWith(c.w)
		x := newReaderIndex(dc)
		got, read := x.textOf(c.v).fieldsWith(c.w)
		x.release()
		if !read || !slices.Equal(got, want) || len(got) != 1 {
			t.Errorf("view %s: %v (read %v); want %v", c.what, got, read, want)
		}
	}
	if len(fs.fieldTexts) != 9 || fs.fieldBuilds != 9 {
		t.Errorf("%d views by text, %d builds; want 9 and 9", len(fs.fieldTexts), fs.fieldBuilds)
	}
}
