package learn

import (
	"encoding/base64"
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// changedPairs are the changed words of a reading, "rec>live" each, in
// order; a word only one run has reads "rec>" or ">live".
func changedPairs(rd textReading) []string {
	var out []string
	for _, w := range rd.Words {
		out = append(out, w.Rec+">"+w.Live)
	}
	return out
}

// Every isWord run is a word, whatever stands around it: '-', '_', '+',
// '/', '=' and '.' always split. The "17" of "order-17", "uploads/17/a.png"
// and "Order+17" is the one changed word, at its index, and so is the 42 of
// "cart_42", whose whole id token the reader lists beside it.
func TestReaderChangedWordsAreIsWordRuns(t *testing.T) {
	for _, c := range []struct {
		rec, live string
		idx       int
		token     string
	}{
		{"order-17", "order-18", 1, "order-17"},
		{"uploads/17/a.png", "uploads/18/a.png", 1, ""},
		{"Order+17", "Order+18", 1, ""},
		{"cart_42", "cart_43", 1, "cart_42"},
		{"v1.17.3", "v1.18.3", 1, ""},
		{"k=17", "k=18", 1, ""},
	} {
		rd := readText(c.rec, c.live)
		got := changedPairs(rd)
		if len(rd.Words) != 1 || rd.ChangedShape {
			t.Errorf("%q -> %q: changed %q (shape changed %v); want one word", c.rec, c.live, got, rd.ChangedShape)
			continue
		}
		w := rd.Words[0]
		if w.RecIdx != c.idx || w.LiveIdx != c.idx || c.rec[w.RecAt[0]:w.RecAt[1]] != w.Rec || c.live[w.LiveAt[0]:w.LiveAt[1]] != w.Live {
			t.Errorf("%q -> %q: word %q>%q at %d/%d %v/%v; want index %d at its own bytes", c.rec, c.live, w.Rec, w.Live, w.RecIdx, w.LiveIdx, w.RecAt, w.LiveAt, c.idx)
		}
		if w.Rec == w.Live || !strings.Contains(c.rec, w.Rec) || len(w.Rec) != 2 {
			t.Errorf("%q -> %q: changed word %q>%q; want the two-digit word", c.rec, c.live, w.Rec, w.Live)
		}
		var id string
		for _, tok := range w.Tokens {
			if tok.Kind == tokenID {
				id = tok.Rec
			}
		}
		if c.token != "" && id != c.token {
			t.Errorf("%q -> %q: id token %q; want %q listed beside the word", c.rec, c.live, id, c.token)
		}
		if c.token == "" && id != "" && strings.ContainsAny(c.rec, "/+.=") {
			t.Errorf("%q -> %q: id token %q across a separator no id holds", c.rec, c.live, id)
		}
	}
}

// Moved words between two kept words are a stretch. Equal counts pair by
// position; a stretch only one run has is a changed shape; unequal counts
// pair nothing, and bind only as a whole token both runs hold there.
func TestReaderStretches(t *testing.T) {
	// a,b to a,b,c: a word only F has.
	rd := readText("a,b", "a,b,c")
	if !rd.ChangedShape || len(rd.Stretches) != 1 || rd.Stretches[0].Shape != stretchShapeChanged {
		t.Errorf("a,b -> a,b,c: %+v; want one stretch only F has, a changed shape", rd.Stretches)
	}
	if got := changedPairs(rd); len(got) != 1 || got[0] != ">c" {
		t.Errorf("a,b -> a,b,c: words %q; want F's c alone", got)
	}
	// Equal counts pair by position.
	rd = readText("x-1-y-2-z", "x-7-y-8-z")
	if rd.ChangedShape || len(rd.Stretches) != 2 || fmt.Sprint(changedPairs(rd)) != "[1>7 2>8]" {
		t.Errorf("x-1-y-2-z -> x-7-y-8-z: %q, %+v; want 1>7 and 2>8, two stretches", changedPairs(rd), rd.Stretches)
	}
	rd = readText("a 1 2 b", "a 3 4 b")
	if len(rd.Stretches) != 1 || rd.Stretches[0].Shape != stretchPaired || fmt.Sprint(changedPairs(rd)) != "[1>3 2>4]" {
		t.Errorf("a 1 2 b -> a 3 4 b: %q, %+v; want one stretch paired by position", changedPairs(rd), rd.Stretches)
	}
	// A URL-safe tail split differently: unequal counts, bound only as
	// the whole token, which both runs hold whole there.
	rec, live := "tok=ab-cd_ef.end", "tok=gh_ij.end"
	rd = readText(rec, live)
	if len(rd.Stretches) != 1 || rd.Stretches[0].Shape != stretchUnequal || rd.ChangedShape {
		t.Fatalf("%q -> %q: %+v; want one stretch of unequal counts", rec, live, rd.Stretches)
	}
	st := rd.Stretches[0]
	if len(st.Tokens) == 0 || st.Tokens[0].Kind != tokenID || st.Tokens[0].Rec != "ab-cd_ef" || st.Tokens[0].Live != "gh_ij" {
		t.Errorf("%q -> %q: stretch tokens %+v; want the id token ab-cd_ef>gh_ij whole", rec, live, st.Tokens)
	}
	for _, w := range rd.Words {
		if w.RecIdx >= 0 && w.LiveIdx >= 0 {
			t.Errorf("%q -> %q: %q>%q paired across unequal counts", rec, live, w.Rec, w.Live)
		}
	}
	// Unequal counts across two tokens in a run: no whole token.
	rd = readText("p ab-cd q", "p xy zw uv q")
	if len(rd.Stretches) != 1 || rd.Stretches[0].Shape != stretchUnequal || len(rd.Stretches[0].Tokens) != 0 {
		t.Errorf("unequal counts across two tokens: %+v; want no whole token", rd.Stretches)
	}
	// Two words that swapped have both moved.
	rd = readText("a b", "b a")
	if fmt.Sprint(changedPairs(rd)) != "[a>b b>a]" {
		t.Errorf("a b -> b a: %q; want both moved, paired by position", changedPairs(rd))
	}
	// Nothing changed.
	if rd := readText("same-1", "same-1"); len(rd.Words) != 0 || len(rd.Stretches) != 0 {
		t.Errorf("an unchanged value: %+v", rd)
	}
}

// A separator that changed between paired words is listed with the whole
// tokens that hold it and the words beside it: word by word, replay cannot
// give the new separator. Where no whole token holds it, the text changed
// shape. Separators that stayed, between words that changed, are none.
func TestReaderSeparatorChanges(t *testing.T) {
	for _, c := range []struct {
		rec, live string
		seps      []string // "rec>live" of each changed separator
		token     string   // R's whole token around the first, "" for none
		shape     bool
	}{
		{"a-17", "a_42", []string{"->_"}, "a-17", false},
		{"x-1-y-2-z", "x-7-y-8-z", nil, "", false},
		{"Sfl-KxwRJ_SMe", "Q1x_Yp-9vTq", []string{"->_", "_>-"}, "Sfl-KxwRJ_SMe", false},
		{"v1.17", "v1,18", []string{".>,"}, "", true},
		{"a 1", "a  1", []string{" >  "}, "", true},
		{" a-1", "a-2", []string{" >"}, "", true},
		{"--", "__", []string{"-->__"}, "--", false},
		{"a,b", "a,b,c", nil, "", true},
	} {
		rd := readText(c.rec, c.live)
		var got []string
		for _, sc := range rd.Separators {
			got = append(got, c.rec[sc.RecAt[0]:sc.RecAt[1]]+">"+c.live[sc.LiveAt[0]:sc.LiveAt[1]])
		}
		if !sameStrings(got, c.seps) || rd.ChangedShape != c.shape {
			t.Errorf("%q -> %q: separators %q, shape changed %v; want %q, %v", c.rec, c.live, got, rd.ChangedShape, c.seps, c.shape)
			continue
		}
		if len(rd.Separators) > 0 {
			var tok string
			if ts := rd.Separators[0].Tokens; len(ts) > 0 {
				tok = ts[0].Rec
			}
			if tok != c.token {
				t.Errorf("%q -> %q: the token around the separator %q; want %q", c.rec, c.live, tok, c.token)
			}
		}
	}
}

// A changed JWT signature is one whole id token (its '.'s split the
// token, its '-' and '_' do not), listed beside each of its changed words:
// it binds whole where a place holds it whole in each run, its words one
// by one otherwise.
func TestReaderJWTSignatureIsOneWholeToken(t *testing.T) {
	head, body := "eyJhbGciOiJIUzI1NiJ9", "eyJzdWIiOiJ1LTEifQ"
	rs, ls := "Sfl-KxwRJ_SMeKKF2QT4fwpMeJf36POk6yJV_adQssw", "Q1x_Yp-9vTq3LmN0Rr4sWkHd2cJ7aBfE5gUoZ_iXyPq"
	rec, live := "Bearer "+head+"."+body+"."+rs, "Bearer "+head+"."+body+"."+ls
	rd := readText(rec, live)
	if len(rd.Words) == 0 {
		t.Fatalf("a changed signature: no changed words")
	}
	for _, w := range rd.Words {
		var whole *wholeToken
		for k := range w.Tokens {
			if w.Tokens[k].Kind == tokenID {
				whole = &w.Tokens[k]
			}
		}
		if whole == nil || whole.Rec != rs || whole.Live != ls {
			t.Errorf("signature word %q>%q: tokens %+v; want the whole signature", w.Rec, w.Live, w.Tokens)
		}
	}
	x := newReaderIndex(nil)
	both := `{"auth":"` + rs + `"}`
	if held, read := x.holdsToken(both, tokenID, rs); !held || !read {
		t.Errorf("a place holding the signature whole: held %v read %v", held, read)
	}
	if held, _ := x.holdsToken(`{"auth":"x`+rs+`"}`, tokenID, rs); held {
		t.Errorf("a place holding the signature inside a longer id: held")
	}
}

// uuidAnswer is an answer of about size bytes holding n distinct UUIDs,
// api-server's aggregate shape (a 498 KB answer with about 400 UUIDs).
func uuidAnswer(n, size int) (string, []string) {
	var b strings.Builder
	ids := make([]string, n)
	for i := range ids {
		ids[i] = fmt.Sprintf("%08x-%04x-4%03x-8%03x-%012x", 0x1f00a000+i*7919, i*31%0xffff, i%0xfff, (i*17)%0xfff, 0x5e0000000000+i*104729)
	}
	b.WriteString(`{"events":[`)
	for i := 0; b.Len() < size; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"id":"%s","suite":"%s","kind":"step-%d","note":"ran on client at node %d with status ok"}`, ids[i%n], ids[(i*7)%n], i%13, i%97)
	}
	b.WriteString(`]}`)
	return b.String(), ids
}

// The index reads a 498 KB answer of about 400 UUIDs within its budget and
// finds each id's 4-hex group as a word; past its budget it still finds
// them, reading the answer again, and keeps nothing of it.
func TestReaderIndexBudget(t *testing.T) {
	answer, ids := uuidAnswer(400, 498<<10)
	x := newReaderIndex(nil)
	for _, id := range ids {
		group := strings.Split(id, "-")[1]
		if held, read := x.holdsWord(answer, group); !held || !read {
			t.Fatalf("group %q of %s: held %v read %v; want held", group, id, held, read)
		}
	}
	if held, read := x.holdsWord(answer, "zzzz9"); held || !read {
		t.Errorf("a word the answer lacks: held %v read %v", held, read)
	}
	if x.unread() {
		t.Errorf("the index reports unread within its budget")
	}
	if x.used <= 0 || x.used > x.budget {
		t.Errorf("index used %d of %d bytes", x.used, x.budget)
	}
	defer func(b int64) { readerBudget = b }(readerBudget)
	readerBudget = 4 << 10
	small := newReaderIndex(nil)
	if held, read := small.holdsWord(answer, strings.Split(ids[0], "-")[1]); !held || !read {
		t.Errorf("past the budget: held %v read %v; want held, as within it", held, read)
	}
	if small.unread() || small.used != 0 {
		t.Errorf("past the budget: unread %v, %d bytes kept; want read, nothing kept", small.unread(), small.used)
	}
	if held, read := small.holdsWord("a short text 42", "42"); !held || !read || small.used == 0 {
		t.Errorf("a text within what is left of the budget: held %v read %v, %d bytes kept", held, read, small.used)
	}
}

// BenchmarkReaderIndex498KB indexes api-server's aggregate shape: one
// 498 KB answer with about 400 UUIDs, every group of every id looked up.
func BenchmarkReaderIndex498KB(b *testing.B) {
	answer, ids := uuidAnswer(400, 498<<10)
	var peak uint64
	for i := 0; i < b.N; i++ {
		place := newReaderIndex(NewDriftCache()).text(answer)
		for _, id := range ids {
			for _, g := range strings.Split(id, "-") {
				if held, read := place.holdsWord(g); !held || !read {
					b.Fatalf("%q: held %v read %v", g, held, read)
				}
			}
		}
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		if ms.HeapInuse > peak {
			peak = ms.HeapInuse
		}
	}
	b.ReportMetric(float64(peak)/(1<<20), "heapMiB")
}

// Past the budget a word is found by a scan of the text and its other
// declared forms, exactly where main's word lists hold it, in each reading,
// and no word list of the text is built: a recording past the budget costs
// a scan per question, not a list of its every word.
func TestReaderAWordPastTheBudgetIsScannedAsMainsListsHoldIt(t *testing.T) {
	defer func(b int64, f func([]string) []map[string]bool) { readerBudget, tokensOfForms = b, f }(readerBudget, tokensOfForms)
	texts := []string{
		`{"id":"order-17","path":"uploads/17/a.png","q":"Order+17"}`,
		`cart_42 and 43, caf\u00e9 é9 tok_a-b_c --x__`,
		`{"jwt":"eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.sig_-x"}`,
		`a%2Fb%20c%3D17 dGVzdC12YWx1ZQ== x`,
		`{"note":"{\"inner\":\"v-9\"}"} 17`,
		`17`,
		``,
	}
	lists := tokensOfForms
	built := 0
	tokensOfForms = func(f []string) []map[string]bool { built++; return lists(f) }
	readerBudget = 1
	for _, text := range texts {
		want := lists(textForms(text))
		words := map[string]bool{"": true, "zz": true, "17/a": true, "-": true, "a-b": true, "order-17": true}
		for _, set := range want {
			for w := range set {
				words[w] = true
				words[w[:len(w)/2]] = true
			}
		}
		x := newReaderIndex(nil)
		place := x.text(text)
		for w := range words {
			for r := range readings {
				if held, read := place.wordIn(w, r); held != want[r][w] || !read {
					t.Errorf("%q in %q, reading %d: held %v read %v; main's lists hold it: %v", w, text, r, held, read, want[r][w])
				}
			}
			any := want[0][w] || want[1][w] || want[2][w]
			if held, _ := place.holdsWord(w); held != any {
				t.Errorf("%q in %q: holdsWord %v; main's lists hold it in some reading: %v", w, text, held, any)
			}
			if held, _ := place.holdsToken(tokenID, w); held != want[2][w] {
				t.Errorf("%q in %q: an idByte token %v; main's idByte list holds it: %v", w, text, held, want[2][w])
			}
		}
		if x.used != 0 || x.unread() {
			t.Errorf("%q: %d bytes kept, unread %v; want nothing kept past the budget, and read", text, x.used, x.unread())
		}
	}
	if built != 0 {
		t.Errorf("past the budget the index built %d word lists; want a scan per question, and none", built)
	}
}

// A call asking about many large earlier recordings, each handed over
// and dropped in turn as keploy's store streams them, holds at once only
// what fits its budget and main's text cache (whose heap is about twice
// what it counts, as set_budget_test.go allows): a text past the budget is
// read, and answers as within it, and the index keeps no byte of it.
func TestReaderIndexHoldsNoTextPastItsBudget(t *testing.T) {
	defer func(b int64) { readerBudget = b }(readerBudget)
	readerBudget = 1 << 20
	const n, size = 64, 1 << 20
	heap := func() uint64 {
		runtime.GC()
		var st runtime.MemStats
		runtime.ReadMemStats(&st)
		return st.HeapAlloc
	}
	x := newReaderIndex(NewDriftCache())
	before := heap()
	for i := 0; i < n; i++ {
		text := fmt.Sprintf("%07d ", i) + strings.Repeat("a", size-8)
		if held, read := x.holdsWord(text, fmt.Sprintf("%07d", i)); !held || !read {
			t.Fatalf("recording %d: held %v read %v; want its own id held", i, held, read)
		}
	}
	grew := int64(heap()) - int64(before)
	runtime.KeepAlive(x)
	if x.unread() || x.used > x.budget {
		t.Fatalf("%d recordings of %d bytes against a %d byte budget: unread %v, %d bytes kept; want read, within the budget", n, size, readerBudget, x.unread(), x.used)
	}
	if grew > readerBudget+2*textBudget+8<<20 {
		t.Fatalf("the index holds %d MiB after %d recordings of 1 MiB were asked about and dropped: it keeps texts past its %d MiB budget", grew>>20, n, readerBudget>>20)
	}
}

// A place keeps its recording's handle in the call's index (textOf),
// never the recording's text. A call that asks about every recording before
// it, each read from the set's store and let go again, holds at once no more
// than the index's budget and the one text it reads, however many handles
// it keeps. A question the index answered within the budget is answered
// again without reading the recording; past the budget it is read again
// from the store, answers as within it, and is kept by no one.
func TestReaderTextOfARecordingPinsNothingPastTheBudget(t *testing.T) {
	defer func(b, r int64) { bodyBudget, readerBudget = b, r }(bodyBudget, readerBudget)
	bodyBudget, readerBudget = 0, 5<<19
	const n, size = 64, 1 << 20
	tr := NewTracker()
	tr.NewSet()
	tr.Begin()
	defer tr.End()
	dc := tr.Drifts()
	id := func(i int) string { return fmt.Sprintf("%07d", i) }
	mocks := make([]*models.Mock, n)
	raws := make([]Raw, n)
	for i := range mocks {
		body := `{"id":"` + id(i) + `","note":"` + strings.Repeat("w ", size/2) + `"}`
		mocks[i] = getAnswer("mock-"+id(i), "http://api/items/"+id(i), body, eRecAt.Add(-time.Duration(n-i)*time.Second))
		raws[i] = dc.LazyRawOf(mocks[i], false, nil)
	}
	heap := func() int64 {
		runtime.GC()
		var st runtime.MemStats
		runtime.ReadMemStats(&st)
		return int64(st.HeapAlloc)
	}
	x := newReaderIndex(dc)
	before := heap()
	places := make([]*readerText, n)
	for i := range raws {
		places[i] = x.textOf(raws[i])
		if held, read := places[i].holdsWord(id(i)); read && !held {
			t.Fatalf("recording %d: read, and its own id not held", i)
		}
	}
	grew := heap() - before
	runtime.KeepAlive(places)
	if grew > readerBudget+2*textBudget+8<<20 {
		t.Fatalf("the call holds %d MiB with a handle to each of %d recordings of 1 MiB: it keeps texts past its %d KiB budget", grew>>20, n, readerBudget>>10)
	}
	if x.unread() || x.used > x.budget {
		t.Fatalf("%d recordings of 1 MiB against a %d KiB budget: unread %v, %d bytes kept; want read, within the budget", n, readerBudget>>10, x.unread(), x.used)
	}
	if held, read := places[0].holdsWord(id(0)); !held || !read {
		t.Fatalf("the first recording, within the budget: held %v read %v", held, read)
	}
	loads := dc.Loads()
	for _, w := range []string{id(0), "w", "zzz"} {
		places[0].holdsWord(w)
		places[0].holdsToken(tokenID, w)
	}
	if got := dc.Loads(); got != loads {
		t.Errorf("asking a recording within the budget again read it again (%d loads, want %d)", got, loads)
	}
	loads = dc.Loads()
	if held, read := places[n-1].holdsWord(id(n - 1)); !held || !read {
		t.Errorf("the last recording, past the budget: held %v read %v; want its own id held", held, read)
	}
	if _, _, read := places[n-1].holdsMoment(moment{}); !read {
		t.Errorf("the last recording's instants, past the budget, read as unread")
	}
	if got := dc.Loads(); got <= loads {
		t.Errorf("a recording past the budget answered without reading it again (%d loads, want more than %d): something keeps its text", got, loads)
	}
	// Past the budget each question reads the recording once: its text and
	// its fields from one read, its instants and their declared forms from
	// one read.
	last := places[n-1]
	for _, q := range []struct {
		name string
		ask  func()
	}{
		{"a word", func() { last.holdsWord("zzz") }},
		{"a base64 run", func() { last.holdsToken(tokenB64, "QUJDREVGR0g") }},
		{"a word's fields", func() { last.fieldsWith("zzz") }},
		{"an instant", func() { last.holdsMoment(moment{}) }},
		{"its instants", func() { last.spansOf() }},
	} {
		loads = dc.Loads()
		q.ask()
		if got := dc.Loads() - loads; got != 1 {
			t.Errorf("%s, past the budget: the recording was read %d times for one question; want once", q.name, got)
		}
	}
	if s, ok := places[0].textNow(); !ok || s != raws[0].Text() {
		t.Errorf("the first recording's text: ok %v, the payload's %v", ok, s == raws[0].Text())
	}
	if s, ok := places[n-1].textNow(); !ok || s != raws[n-1].Text() {
		t.Errorf("the last recording's text, past the budget: ok %v, the payload's %v", ok, s == raws[n-1].Text())
	}
	// A recording keploy could not read holds no word: unread, never
	// "not held".
	if _, read := x.textOf(Raw{mock: "mock-unread"}).holdsWord("w"); read || !x.unread() {
		t.Errorf("a recording keploy could not read: read %v, index unread %v", read, x.unread())
	}
}

// A base64 token is held where it stands whole, no byte of the base64
// alphabets or its padding on either side. The index reads a text's whole
// runs once (holdsToken's kept runs) instead of scanning the text per token, and past its
// budget finds a run by a scan that lists no other (eachB64Run): each base64
// token (isB64Run, as tokenSpans cuts them) is held by both exactly where
// the scan holds it.
func TestReaderWholeBase64RunsAreTheScansTokens(t *testing.T) {
	scan := func(text, tok string) bool {
		for i := 0; i+len(tok) <= len(text); i++ {
			end := i + len(tok)
			if text[i:end] == tok && (i == 0 || !isB64Byte(text[i-1])) && (end == len(text) || !isB64Byte(text[end]) && text[end] != '=') {
				return true
			}
		}
		return false
	}
	texts := []string{"ab=cd", "abc==x", "abc==", "x.abc=.y", "abc=def", "eyJh.c2ln_-+/==;", "a==b==", "==ab", "a-b_c+d/e", "q=1&r=Zm9v=="}
	defer func(b int64) { readerBudget = b }(readerBudget)
	for k, text := range texts {
		readerBudget = int64(k % 2) // past the budget (1): the scan
		x := newReaderIndex(nil)
		for i := 0; i < len(text); i++ {
			for j := i + 1; j <= len(text); j++ {
				tok := text[i:j]
				if !isB64Run(tok) {
					continue
				}
				held, read := x.text(text).holdsToken(tokenB64, tok)
				if want := scan(text, tok); held != want || !read {
					t.Errorf("%q in %q: held %v read %v, the scan holds it %v", tok, text, held, read, want)
				}
			}
		}
	}
}

// A call's index keeps its answers on the set's one budget, the one its
// tiers keep within (setBudget), never on a budget beside it: what it keeps
// is counted there while the call runs, it keeps none of what the set has
// no room for once the tiers that let go (words, payloads) have let go of
// theirs, and it gives all of it back when the call is over (release). Its
// answers are the same kept or not.
func TestReaderIndexKeepsOnTheSetsOneBudget(t *testing.T) {
	defer func(b, r int64) { learnBudget, readerBudget = b, r }(learnBudget, readerBudget)
	learnBudget, readerBudget = 1<<20, 0
	text := "id 7f3c " + strings.Repeat("word ", 20000)

	dc := NewDriftCache()
	dc.set = newSetDerived()
	x := newReaderIndex(dc)
	if held, read := x.text(text).holdsWord("7f3c"); !held || !read {
		t.Fatalf("held %v read %v; want the word held", held, read)
	}
	if x.used <= 0 || dc.set.budget.used != x.used {
		t.Fatalf("the index keeps %d bytes; the set's budget counts %d: want what the index keeps counted on the set's budget", x.used, dc.set.budget.used)
	}
	x.release()
	if dc.set.budget.used != 0 {
		t.Fatalf("the call is over and the set's budget still counts %d bytes of its index", dc.set.budget.used)
	}

	// The set's tiers that let none go keep all but 1 KiB of its budget.
	full := NewDriftCache()
	full.set = newSetDerived()
	full.set.budget.used = full.set.budget.limit - 1<<10
	y := newReaderIndex(full)
	if held, read := y.text(text).holdsWord("7f3c"); !held || !read {
		t.Fatalf("past the set's budget: held %v read %v; want the word held, as within it", held, read)
	}
	if y.used != 0 || full.set.budget.used > full.set.budget.limit {
		t.Fatalf("the set's budget has 1 KiB left; the index keeps %d bytes and the set counts %d of %d: want nothing kept past the set's one budget", y.used, full.set.budget.used, full.set.budget.limit)
	}
}

// The reader cuts a base64 token (tokenSpans) by the one rule it finds a
// token in a text by (holdsToken): a run of the base64 alphabets with all
// the padding after it, that no byte of the alphabets follows. So every
// base64 token it lists is held whole by its own text, and a run that a
// byte of the alphabets follows past its padding (`tok=` of
// `tok=ab+cd/ef==`) is no token: its words are judged one by one.
func TestReaderCutsABase64TokenAsItFindsOne(t *testing.T) {
	for _, c := range []struct {
		text string
		want []string
	}{
		{"abc==def", []string{"def"}},
		{"x=abc+/def==;y", []string{"abc+/def==", "y"}},
		{"QUJD=======x", []string{"x"}},
		{"tok=ab+cd/ef==", []string{"ab+cd/ef=="}},
		{"a-b_c", []string{"a-b_c"}},
		{"QUJDREVG+2026-10-31T10:50:13Z", []string{"QUJDREVG+"}},
		{"at 2026-10-31T10:50:13Z+QUJDREVG", []string{"at", "+QUJDREVG"}},
	} {
		var got []string
		for _, sp := range tokenSpans(c.text, instantSpans(c.text)).b64 {
			tok := c.text[sp[0]:sp[1]]
			got = append(got, tok)
			for _, budget := range []int64{0, 1} {
				readerBudget = budget
				if held, read := newReaderIndex(nil).text(c.text).holdsToken(tokenB64, tok); !held || !read {
					t.Errorf("%q (budget %d): the reader lists the base64 token %q, which its own text does not hold whole (held %v read %v)", c.text, budget, tok, held, read)
				}
			}
			readerBudget = 0
		}
		if fmt.Sprint(got) != fmt.Sprint(c.want) {
			t.Errorf("%q: base64 tokens %q, want %q", c.text, got, c.want)
		}
	}
}

// Past maxMovedCells, keptWords compares no table (main's movedWords'
// guard): every word between the common start and end has moved. Two
// drifted 498 KB answers hold about 60k words each, and their table would
// be (60k)² int32, gigabytes; here 600 words a side, past the cap, keep
// only the common start and end, and allocate far less than the table.
func TestReaderKeptWordsPastTheCapComparesNoTable(t *testing.T) {
	const n = 600
	if n*n <= maxMovedCells {
		t.Fatalf("fixture: %d x %d words are within the cap %d", n, n, maxMovedCells)
	}
	rec, live := []string{"a"}, []string{"a", "new"}
	for i := 0; i < n; i++ {
		rec = append(rec, fmt.Sprintf("x%d", i))
		if i < n-1 {
			live = append(live, fmt.Sprintf("x%d", i))
		}
	}
	rec, live = append(rec, "z"), append(live, "z")
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	kept := keptWords(rec, live)
	runtime.ReadMemStats(&after)
	if want := [][2]int{{0, 0}, {n + 1, n + 1}}; fmt.Sprint(kept) != fmt.Sprint(want) {
		t.Errorf("kept %d pairs (%v...); want only the common start and end %v", len(kept), kept[:min(len(kept), 4)], want)
	}
	if got, table := after.TotalAlloc-before.TotalAlloc, uint64((n+1)*(n+1)*4); got >= table/8 {
		t.Errorf("keptWords allocated %d bytes; the table it must not build is %d", got, table)
	}
	// Within the cap, the same shift keeps the words that moved along.
	if kept := keptWords(rec[:60], append([]string{"a", "new"}, rec[1:59]...)); len(kept) != 59 {
		t.Errorf("within the cap: kept %d pairs; want the 59 shared words", len(kept))
	}
}

// A recording's words are read field by field (fieldsWith), each field at
// its path, which round F's view of the recording shares, so a word is at
// the same place in both runs only where one field holds it in each
// (inOneField): one that moved to another field in F is not, though each
// text whole still holds it; one that stayed is, in a field's declared
// form too (a base64 of it). A text the caller hands is one field. Within
// the index's budget and past it.
func TestReaderFieldsOfARecordingHoldItsWordsWhereTheyStand(t *testing.T) {
	defer func(b int64) { readerBudget = b }(readerBudget)
	const w = "ord7Kq2Xw9"
	answer := func(body string) Raw { return RawOf(getAnswer("mock-1", "http://api/items/1", body, eRecAt), false) }
	rec := answer(`{"a":"` + w + `","b":"x"}`)
	moved := answer(`{"a":"tok","b":"` + w + `"}`)
	stayed := answer(`{"a":"` + w + `","b":"y"}`)
	encoded := answer(`{"a":"` + base64.StdEncoding.EncodeToString([]byte("id "+w)) + `","b":"y"}`)
	for _, budget := range []int64{0, 1} {
		readerBudget = budget
		x := newReaderIndex(nil)
		r := x.textOf(rec)
		at, read := r.fieldsWith(w)
		if len(at) != 1 || !read || !strings.HasSuffix(at[0].path, ".a") || at[0].key {
			t.Errorf("budget %d: %q is at %v (read %v); want the one field a", budget, w, at, read)
		}
		for _, c := range []struct {
			what string
			live Raw
			want bool
		}{{"moved to b", moved, false}, {"stayed at a", stayed, true}, {"at a, base64 there", encoded, true}} {
			l := x.textOf(c.live)
			if held, _ := l.holdsWord(w); !held {
				t.Fatalf("fixture %s: F's text does not hold %q", c.what, w)
			}
			if same, read := inOneField(r, w, l, w); same != c.want || !read {
				t.Errorf("budget %d, %s: in one field %v read %v; want %v", budget, c.what, same, read, c.want)
			}
		}
		if same, read := inOneField(x.text("id "+w), w, x.text(w+" ok"), w); !same || !read {
			t.Errorf("budget %d: two texts the caller hands, each holding %q: in one field %v read %v", budget, w, same, read)
		}
	}
}

// The index keeps an answer only where it and its text fit what is left of
// the call's cap: a text that fits alone, whose words do not, keeps none,
// and the index never holds past its cap.
func TestReaderIndexKeepsNoAnswerPastItsCap(t *testing.T) {
	defer func(b int64) { readerBudget = b }(readerBudget)
	var sb strings.Builder
	for i := 0; i < 64; i++ {
		fmt.Fprintf(&sb, "w%d ", i)
	}
	text := sb.String()
	readerBudget = int64(len(text)) + headerBytes + 1
	x := newReaderIndex(nil)
	tt := x.text(text)
	if held, read := tt.holdsWord("w17"); !held || !read {
		t.Fatalf("held %v read %v; want held", held, read)
	}
	if x.used > x.budget || tt.e.tokens.ok {
		t.Errorf("the index keeps %d bytes (words kept %v) within a cap of %d", x.used, tt.e.tokens.ok, x.budget)
	}
}

// Where the set's tiers that let go keep its budget full, the index has
// the set let go of what it is past (budget.letGo: the words, then the
// payloads, each read again where asked for) before it keeps an answer,
// as a tier keeps one: the index keeps its answer within the set's one
// budget, and the set has let go of what that needed.
func TestReaderIndexHasTheSetLetGoToKeep(t *testing.T) {
	defer func(b, r int64) { learnBudget, readerBudget = b, r }(learnBudget, readerBudget)
	learnBudget, readerBudget = 1<<20, 0
	text := "id 7f3c " + strings.Repeat("word ", 20000)
	dc := NewDriftCache()
	dc.set = newSetDerived()
	b := dc.set.budget
	b.used = b.limit - 1<<10
	var freed int64
	b.letGo = func(need int64) { freed += need; b.used -= need }
	x := newReaderIndex(dc)
	if held, read := x.text(text).holdsWord("7f3c"); !held || !read {
		t.Fatalf("held %v read %v; want the word held", held, read)
	}
	if x.used <= 0 || freed <= 0 || b.used > b.limit {
		t.Fatalf("the index keeps %d bytes, the set let go of %d and counts %d of %d: want the set to let go of what the index's answer needs, and the index to keep it within the set's budget", x.used, freed, b.used, b.limit)
	}
	x.release()
}
