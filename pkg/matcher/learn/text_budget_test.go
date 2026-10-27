package learn

import (
	"fmt"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// bigFiller is fillers' i-th recording of n with a first cell of size bytes
// of words no other recording holds, beside a number.
func bigFiller(i, n, size int, at time.Time) *models.Mock {
	m := fillers(n, at)[i]
	var b strings.Builder
	b.Grow(size + 32)
	for j := 0; b.Len() < size; j++ {
		fmt.Fprintf(&b, "f%d.w%d ", i, j)
	}
	m.Spec.PostgresV3.Query.Response.Rows[0][0].Value = b.String()
	return m
}

// A test's drift cache kept what it derived of each text a rule read
// (rawExtra, rawTokens, spansOf, searched), keyed by the text itself, for
// the whole test: a field's text is a part of its payload's, and a
// payload's text is the set's store's (bodyStore), so a call whose rules
// read every earlier payload kept all of them at once, whatever the set's
// store let go (612 MiB over 256 recordings of 1 MiB). It now keeps a copy
// of each text within a fixed budget (textCache), and reads a payload that
// is all of its recording's by the recording, within the set's budget
// (setDerived): the heap the learn step keeps for the test stays within the
// three budgets, however many payloads its rules read, and it learns what a
// cache of no tracker learns.
func TestATestsCacheKeepsNoEarlierPayloadsText(t *testing.T) {
	defer func(b, d, x int64) { bodyBudget, derivedBudget, textBudget = b, d, x }(bodyBudget, derivedBudget, textBudget)
	bodyBudget, derivedBudget, textBudget = 256<<10, 1<<20, 1<<20
	const n, size = 32, 16 << 10
	at := eRecAt
	const word, num = "Req.query.bind[0]", "Req.query.bind[1]"
	rec := Shape{Leaves: map[string]Leaf{word: {Path: word, Type: String, Value: "a1b2c3d4"}, num: {Path: num, Type: Number, Value: "7041"}}}
	live := Shape{Leaves: map[string]Leaf{word: {Path: word, Type: String, Value: "z9y8x7w6"}, num: {Path: num, Type: Number, Value: "7093"}}}
	classify := func(dc *DriftCache, raw []Raw) []Learned {
		return ClassifyFollowing(dc, []string{word, num}, live, rec, at, nil, raw, nil, nil, nil)
	}
	want := func() []Learned {
		raw := make([]Raw, n)
		for i := range raw {
			raw[i] = RawOf(bigFiller(i, n, size, at), true).ServedAs(i + 1).ServedAsRecorded()
		}
		return classify(NewDriftCache(), raw)
	}()

	tr := NewTracker()
	tr.NewSet()
	tr.Begin()
	dc := tr.Drifts()
	meta := fillers(n, at)
	raw := make([]Raw, n)
	for i := range raw {
		i := i
		raw[i] = dc.LazyRawOf(meta[i], true, func() (*models.Mock, error) { return bigFiller(i, n, size, at), nil }).ServedAs(i + 1).ServedAsRecorded()
	}
	heap := func() int64 {
		runtime.GC()
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		return int64(ms.HeapAlloc)
	}
	before := heap()
	got := classify(dc, raw)
	kept := heap() - before
	runtime.KeepAlive(dc)
	runtime.KeepAlive(raw)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("learned %v, want %v (a cache of no tracker's)", got, want)
	}
	if loads := dc.Loads(); loads < n {
		t.Fatalf("the call's rules loaded %d of the %d earlier payloads: the row reads too few to tell", loads, n)
	}
	budgets := bodyBudget + derivedBudget + textBudget
	if bound := budgets + 1<<20; kept > bound {
		t.Errorf("the test keeps %d MiB of heap after reading %d payloads of %d KiB (%d KiB): past the budgets' %d MiB and 1 MiB besides (text cache %d bytes, set %d bytes)",
			kept>>20, n, size>>10, n*size>>10, budgets>>20, dc.text.keptBytes(), tr.derived.keptBytes())
	}
	tr.End()
}

// unaccounted read each earlier payload's words (searchedTokens) once for
// each changed word of the value, in each reading: where the set had let a
// payload's words go (setDerived's budget), each word read the payload
// again, and a call's cost grew with its value's words times the earlier
// payloads. Each payload's words are now read once for the value
// (wordsHeld): a value of six changed words reads the earlier payloads as
// often as a value of one.
func TestAValuesWordsReadEachEarlierPayloadOnce(t *testing.T) {
	defer func(d, x int64, f func([]string) []map[string]bool) {
		derivedBudget, textBudget, tokensOfForms = d, x, f
	}(derivedBudget, textBudget, tokensOfForms)
	derivedBudget, textBudget = 0, 0
	const n = 8
	at := eRecAt
	const word = "Req.query.bind[0]"
	reads, read := 0, tokensOfForms
	tokensOfForms = func(forms []string) []map[string]bool {
		if len(forms) > 0 && strings.Contains(forms[0], "filler") {
			reads++
		}
		return read(forms)
	}
	count := func(rec, live string) int {
		reads = 0
		tr := NewTracker()
		tr.NewSet()
		tr.Begin()
		dc := tr.Drifts()
		raw := make([]Raw, n)
		for i, m := range fillers(n, at) {
			raw[i] = dc.LazyRawOf(m, true, nil).ServedAs(i + 1).ServedAsRecorded()
		}
		r := Shape{Leaves: map[string]Leaf{word: {Path: word, Type: String, Value: rec}}}
		l := Shape{Leaves: map[string]Leaf{word: {Path: word, Type: String, Value: live}}}
		ClassifyFollowing(dc, []string{word}, l, r, at, nil, raw, nil, nil, nil)
		tr.End()
		return reads
	}
	one := count("a1b2c3d4", "z9y8x7w6")
	six := count("a1 b2 c3 d4 e5 f6", "z1 y2 x3 w4 v5 u6")
	if one == 0 {
		t.Fatalf("no earlier payload's words were read: the row tells nothing")
	}
	if six != one {
		t.Errorf("a value of six changed words read the %d earlier payloads' words %d times, one of one word %d times: they are read again for each word", n, six, one)
	}
}

// A test's text cache was keyed by the text a rule read, and a field's
// text is a part of its payload's: a key of a few bytes kept the whole
// payload in memory for the test. It now keeps a copy of each text, and
// derives from the copy, so a payload no one else holds is let go however
// many of its fields' texts the cache keeps.
func TestATextsCopyKeepsNoLongerTextItWasCutFrom(t *testing.T) {
	heap := func() int64 {
		runtime.GC()
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		return int64(ms.HeapAlloc)
	}
	dc := NewDriftCache()
	before := heap()
	func() {
		big := strings.Repeat("word ", 2<<20) + "at 2026-10-27T10:00:00Z AbCdEfGhIjKlMnOp"
		end := len(big)
		dc.spansOf(big[end-36 : end-16])
		dc.rawExtra(big[end-16:])
		dc.rawTokens(big[end-39 : end-16])
	}()
	if kept := heap() - before; kept > 1<<20 {
		t.Errorf("the cache keeps %d KiB after three fields' texts of a 10 MiB text were read: it keeps the text they were cut from", kept>>10)
	}
	if dc.text.keptBytes() == 0 {
		t.Fatalf("the cache kept none of the three texts: the row tells nothing")
	}
	runtime.KeepAlive(dc)
}

// A test's text cache keeps within its budget whatever the texts its rules
// read, derives of each what a cache of none derives, and the set keeps no
// payload's text where nothing of it is blanked (setSearched: only that it
// is plain): the text is the set's store's, within its own budget.
func TestATestsTextCacheKeepsWithinItsBudget(t *testing.T) {
	defer func(x int64) { textBudget = x }(textBudget)
	textBudget = 32 << 10
	dc := NewDriftCache()
	for i := 0; i < 100; i++ {
		text := fmt.Sprintf("row %d at 2026-10-27T10:%02d:00Z ", i, i%60) + strings.Repeat(fmt.Sprintf("v%d.x ", i), 400)
		if got, want := dc.rawTokens(text), tokensOfForms(textForms(text)); !reflect.DeepEqual(got, want) {
			t.Fatalf("text %d: words %d forms, want %d", i, len(got), len(want))
		}
		if got, want := dc.rawExtra(text), extraOf(text); got != want {
			t.Fatalf("text %d: other forms differ from a cache of none's", i)
		}
		if got, want := dc.spansOf(text), instantSpans(text); !reflect.DeepEqual(got, want) {
			t.Fatalf("text %d: instants %v, want %v", i, got, want)
		}
		if kept := dc.text.keptBytes(); kept > textBudget {
			t.Fatalf("after %d texts of 2 KiB the test's text cache keeps %d bytes, past its budget of %d", i+1, kept, textBudget)
		}
	}
	kept := dc.text.keptBytes()
	if kept == 0 {
		t.Fatalf("the cache kept none of the texts: the row tells nothing")
	}
	dc.rawExtra(strings.Repeat("past the budget ", 4<<10))
	if after := dc.text.keptBytes(); after != kept {
		t.Errorf("a text past the whole budget changed what the cache keeps from %d bytes to %d: it lets the others go and keeps none", kept, after)
	}

	tr := NewTracker()
	tr.NewSet()
	tr.Begin()
	sd := tr.Drifts()
	m := fillers(1, eRecAt)[0]
	x := sd.LazyRawOf(m, true, nil)
	for _, keep := range []bool{false, true} {
		got := sd.searched(x, keep)
		if want := NewDriftCache().searched(RawOf(m, true), keep); got != want {
			t.Errorf("keep %v: searched %q, want %q (a cache of no tracker's)", keep, got, want)
		}
		e := tr.derived.by[x.id]
		if k := keepIndex(keep); e == nil || !e.plain[k] || e.searched[k] != [2]string{} {
			t.Errorf("keep %v: the set keeps the plain payload's searched texts %v, not only that it is plain", keep, e)
		}
	}
	tr.End()
}
