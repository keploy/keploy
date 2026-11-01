package learn

import (
	"fmt"
	"runtime"
	"strings"
	"testing"
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
// finds each id's 4-hex group as a word; past its budget it reads nothing
// and says so.
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
	if held, read := small.holdsWord(answer, strings.Split(ids[0], "-")[1]); held || read {
		t.Errorf("past the budget: held %v read %v; want unread", held, read)
	}
	if !small.unread() {
		t.Errorf("past the budget: the index does not report unread")
	}
	if held, read := small.holdsWord("a short text 42", "42"); !held || !read {
		t.Errorf("a text within what is left of the budget: held %v read %v", held, read)
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
