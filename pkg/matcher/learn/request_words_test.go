package learn

import (
	"encoding/base64"
	"fmt"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// TestRequestWordsHoldEveryWordHoweverTheBodyIsCut: the filter of a
// request's words holds each word of letters and digits written to it, and
// its lower case, however the writes cut the text (a word cut between two
// writes is one word), a word that s begins or ends with is not joined to
// the words beside it (WriteText), and it holds a word never written about
// once in a hundred at most for a body of seventy-five thousand distinct
// words and lower-case forms.
func TestRequestWordsHoldEveryWordHoweverTheBodyIsCut(t *testing.T) {
	var body strings.Builder
	for i := 0; i < 25000; i++ {
		fmt.Fprintf(&body, `{"name":"Word%dX","n":%d},`, i, i*7)
	}
	text := []byte(body.String())
	for _, cut := range []int{1, 3, 7, 64, 4096, len(text)} {
		w := NewRequestWords()
		for i := 0; i < len(text); i += cut {
			_, _ = w.Write(text[i:min(i+cut, len(text))])
		}
		w.End()
		for _, word := range []string{"name", "Word0X", "word0x", "Word24999X", "word24999x", "174993", "n"} {
			if !w.MayHold(word) {
				t.Fatalf("cut every %d bytes: the filter lacks %q, which the body holds", cut, word)
			}
		}
		if !w.MayHold(`Word17X "n"`) || !w.MayHold("-") {
			t.Fatalf("cut every %d bytes: a value the body holds as a token, or one of no word, is said absent", cut)
		}
		held := 0
		for i := 0; i < 2000; i++ {
			if w.MayHold(fmt.Sprintf("absent%dq", i)) {
				held++
			}
		}
		if held > 20 {
			t.Fatalf("cut every %d bytes: %d of 2000 absent words held", cut, held)
		}
	}
	w := NewRequestWords()
	w.End()
	_, _ = w.Write([]byte(" ,;{}\n"))
	w.WriteText("")
	for _, bits := range w.f {
		if bits != 0 {
			t.Fatal("a text of no word set a bit")
		}
	}
	w.WriteText("Alpha ZULU az")
	for word, want := range map[string]bool{"alpha": true, "zulu": true, "Alpha": true, "ZULU": true, "az": true, "AZ": false} {
		if w.MayHold(word) != want {
			t.Errorf("MayHold(%q) = %v, want %v", word, !want, want)
		}
	}
	w = NewRequestWords()
	_, _ = w.Write([]byte("abc"))
	w.WriteText("def")
	_, _ = w.Write([]byte("ghi"))
	w.End()
	for word, want := range map[string]bool{"abc": true, "def": true, "ghi": true, "abcdef": false, "defghi": false, "abcdefghi": false} {
		if w.MayHold(word) != want {
			t.Errorf("MayHold(%q) = %v, want %v", word, !want, want)
		}
	}
	var none *RequestWords
	if !none.MayHold("anything") {
		t.Error("no filter says a word is absent")
	}
}

// TestAnUnreadClientRequestKeepsItsWordsAndItsURLAndHeaders: a client
// test's request keploy could not read keeps the words of its body that
// the ingress read as they passed, and those of its URL and header values,
// as its place's (Earlier.unreadWords), whether it was taken whole
// (IncomingWords) or once its body ended (IncomingComing's done); a test
// keeps maxRequestWords requests' words, the next test as many again.
func TestAnUnreadClientRequestKeepsItsWordsAndItsURLAndHeaders(t *testing.T) {
	at := time.Date(2026, 11, 10, 3, 0, 0, 0, time.UTC)
	rec := &models.HTTPReq{Method: "POST", URL: "http://api/uploads", Timestamp: at.Add(-time.Hour)}
	live := func() *models.HTTPReq {
		return &models.HTTPReq{Method: "POST", URL: "http://api/uploads?tag=urlword", Header: map[string]string{"X-Note": "headerword"}}
	}
	body := func() *RequestWords {
		w := NewRequestWords()
		w.WriteText("bodyword")
		return w
	}
	cases := make([]ClientCase, maxRequestWords+2)
	for i := range cases {
		cases[i] = ClientCase{Name: fmt.Sprintf("post-uploads-%d", i+1), Req: rec}
	}
	tr := NewTracker()
	tr.Begin()
	tr.SetCases(cases)
	if !tr.IncomingWords(live(), "its body is larger than keploy reads", body(), at) {
		t.Fatal("the request did not pair")
	}
	done := tr.IncomingComing(live(), at)
	got := tr.Placed().Earlier
	if len(got) != 2 || got[0].unreadWords == nil || got[1].unreadWords != nil {
		t.Fatalf("placed %+v", got)
	}
	for _, word := range []string{"bodyword", "urlword", "headerword", "uploads"} {
		if !got[0].unreadWords.MayHold(word) {
			t.Errorf("the place's words lack %q", word)
		}
	}
	done(live(), "its body is larger than keploy reads", body())
	if got := tr.Placed().Earlier; got[1].unreadWords == nil || !got[1].unreadWords.MayHold("bodyword") || got[1].Unread == "" {
		t.Fatalf("once its body ended the coming request keeps no words: %+v", got[1])
	}
	if !tr.IncomingWords(live(), "", body(), at) {
		t.Fatal("a read request did not pair")
	}
	if got := tr.Placed().Earlier; got[2].unreadWords != nil {
		t.Fatal("a request keploy read keeps words")
	}
	for i := 3; i < len(cases); i++ {
		tr.IncomingWords(live(), "its body is larger than keploy reads", body(), at)
	}
	got = tr.Placed().Earlier
	kept := 0
	for _, e := range got {
		if e.unreadWords != nil {
			kept++
		}
	}
	if len(got) != len(cases) || kept != maxRequestWords || got[len(got)-1].unreadWords != nil {
		t.Fatalf("a test kept %d requests' words of %d places, want %d", kept, len(got), maxRequestWords)
	}
	tr.Begin()
	tr.SetCases(cases[:1])
	tr.IncomingWords(live(), "its body is larger than keploy reads", body(), at)
	if got := tr.Placed().Earlier; len(got) != 1 || got[0].unreadWords == nil {
		t.Fatalf("the next test kept no words: %+v", got)
	}
	tr.NewSet()
	tr.Begin()
	tr.SetCases(cases[:1])
	tr.IncomingWords(live(), "its body is larger than keploy reads", body(), at)
	if got := tr.Placed().Earlier; len(got) != 1 || got[0].unreadWords == nil {
		t.Fatalf("a new set's test kept no words: %+v", got)
	}
}

// TestAClientRequestKeployCouldNotReadIsNoSourceOfAValueItsWordsLack: a
// client test's request keploy could not read (a body of 2 MB, past what
// the ingress reads) that keeps its words is a source of no value whose
// change it cannot hold: a later call's changed value no word of which it
// holds is read as with no such request, in both the learn step's
// classify (ClassifyArrived) and its decider (Decide). A value whose
// changed word it holds, as written, case-folded or inside a base64 run of
// it, is refused for it, and so is an instant where a form of it holds a
// digit, and every value where it held a run longer than a window.
func TestAClientRequestKeployCouldNotReadIsNoSourceOfAValueItsWordsLack(t *testing.T) {
	const why = "its body is larger than keploy reads"
	const p = "body.token"
	rAt := time.Date(2026, 11, 9, 7, 1, 42, 0, time.UTC)
	fAt := rAt.Add(69 * time.Second)
	hidden := base64.StdEncoding.EncodeToString([]byte(`{"k":"bsixfourhid9x"}`))
	big := func(filler, extra string) string {
		var b strings.Builder
		b.WriteString(`{"file":"` + extra + `","rows":[`)
		for b.Len() < 2<<20 {
			b.WriteString(filler)
		}
		b.WriteString(`""]}`)
		return b.String()
	}
	numbered := big(`"rowvalue","n17",`, `q7w8e9r0t1y2 ZQ5UPPER `+hidden)
	placed := func(body string) []Earlier {
		w := NewRequestWords()
		for i := 0; i < len(body); i += 4093 {
			_, _ = w.Write([]byte(body[i:min(i+4093, len(body))]))
		}
		w.End()
		rec := &models.HTTPReq{Method: "POST", URL: "http://api/upload", Timestamp: rAt.Add(-time.Second)}
		tr := NewTracker()
		tr.Begin()
		tr.SetCases([]ClientCase{{Name: "post-upload-1", Req: rec}})
		if !tr.IncomingWords(&models.HTTPReq{Method: "POST", URL: "http://api/upload"}, why, w, fAt.Add(-time.Second)) {
			t.Fatal("the unread request did not pair")
		}
		return tr.Placed().Earlier
	}
	read := func(typ Type, rec, live string, earlier []Earlier) (Learned, Decision) {
		sh := func(v string) Shape {
			return Shape{Leaves: map[string]Leaf{p: {Path: p, Noise: p, Type: typ, Value: v}}}
		}
		ln := ClassifyArrived(clockedDrifts(), []string{p}, sh(live), sh(rec), rAt, fAt, earlier, nil, nil, nil, nil)[0]
		in := iCall(p, typ, rec, live, rAt, fAt, nil, earlier, nil)
		in.Drifts = oClientDrifts()
		d := Decide(in)
		if len(d) != 1 {
			t.Fatalf("%s -> %s: %d decisions", rec, live, len(d))
		}
		return ln, d[0]
	}
	refusedFor := func(ln Learned, d Decision) bool {
		return ln.Kind == KindUnaccounted && strings.Contains(ln.Why, why) &&
			d.Outcome == OutcomeRefused && d.Refusal != nil && d.Refusal.Kind == RefusalUnread && strings.Contains(d.Refusal.Why, why)
	}
	keeps := func(typ Type, rec, live string, earlier []Earlier) {
		t.Helper()
		ln, d := read(typ, rec, live, earlier)
		ln0, d0 := read(typ, rec, live, nil)
		if ln.Kind != ln0.Kind || ln.Why != ln0.Why || d.Outcome != d0.Outcome || (d.Refusal == nil) != (d0.Refusal == nil) || d.Refusal != nil && d.Refusal.Why != d0.Refusal.Why {
			t.Errorf("%s -> %s after a request that cannot hold it: %s (%s), %s; want as with no request: %s (%s), %s", rec, live, ln.Kind, ln.Why, oDescribe([]Decision{d}), ln0.Kind, ln0.Why, oDescribe([]Decision{d0}))
		}
	}
	refuses := func(typ Type, rec, live string, earlier []Earlier) {
		t.Helper()
		if ln, d := read(typ, rec, live, earlier); !refusedFor(ln, d) {
			t.Errorf("%s -> %s after a request that may hold it: %s (%s), %s; want refused for %q", rec, live, ln.Kind, ln.Why, oDescribe([]Decision{d}), why)
		}
	}
	earlier := placed(numbered)
	if len(earlier) != 1 || earlier[0].unreadWords == nil {
		t.Fatalf("placed %+v; want the unread request with its words", earlier)
	}
	keeps(String, "tok-1a2b3c4d5e6f", "tok-9f8e7d6c5b4a", earlier)
	refuses(String, "tok-1a2b3c4d5e6f", "tok-q7w8e9r0t1y2", earlier)
	refuses(String, "tok-1a2b3c4d5e6f", "tok-zq5upper", earlier)
	refuses(String, "tok-1a2b3c4d5e6f", "TOK-ZQ5UPPER", earlier)
	refuses(String, "tok-1a2b3c4d5e6f", "tok-bsixfourhid9x", earlier)
	// An instant read of the application's clock: it moved by the lead.
	r, l := iMs(rAt.Add(-120*time.Millisecond)), iMs(fAt.Add(-120*time.Millisecond))
	rt, lt := rAt.Add(-120*time.Millisecond).Format(time.RFC3339Nano), fAt.Add(-120*time.Millisecond).Format(time.RFC3339Nano)
	if ln, _ := read(Instant, r, l, nil); ln.Kind == KindUnaccounted {
		t.Fatalf("%s -> %s with no request: %s (%s); want learned, a read of the clock", r, l, ln.Kind, ln.Why)
	}
	refuses(Instant, r, l, earlier)
	refuses(String, "booked at "+rt, "booked at "+lt, earlier)
	letters := placed(big(`"rowvalue","nseventeen",`, "abc"))
	keeps(Instant, r, l, letters)
	keeps(String, "booked at "+rt, "booked at "+lt, letters)
	keeps(String, "tok-1a2b3c4d5e6f", "tok-9f8e7d6c5b4a", letters)
	oneRun := placed(`{"file":"` + strings.Repeat("QUJD", 20<<10) + `"}`)
	refuses(String, "tok-1a2b3c4d5e6f", "tok-9f8e7d6c5b4a", oneRun)
}
