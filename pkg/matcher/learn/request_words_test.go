package learn

import (
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
