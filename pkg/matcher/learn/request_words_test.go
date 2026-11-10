package learn

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// mayHoldWord reports whether the request may hold v as a token
// (ContainsToken), as the filter reads a value's words (mayHold): each of
// v's runs of letters and digits. A v of no letter or digit, and any v of a
// nil or undecoded RequestWords, may.
func (w *RequestWords) mayHoldWord(v string) bool {
	if w == nil || w.undecoded {
		return true
	}
	return mayHold(w.f, wordsBy(v, isWord))
}

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
			if !w.mayHoldWord(word) {
				t.Fatalf("cut every %d bytes: the filter lacks %q, which the body holds", cut, word)
			}
		}
		if !w.mayHoldWord(`Word17X "n"`) || !w.mayHoldWord("-") {
			t.Fatalf("cut every %d bytes: a value the body holds as a token, or one of no word, is said absent", cut)
		}
		held := 0
		for i := 0; i < 2000; i++ {
			if w.mayHoldWord(fmt.Sprintf("absent%dq", i)) {
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
		if w.mayHoldWord(word) != want {
			t.Errorf("mayHoldWord(%q) = %v, want %v", word, !want, want)
		}
	}
	w = NewRequestWords()
	_, _ = w.Write([]byte("abc"))
	w.WriteText("def")
	_, _ = w.Write([]byte("ghi"))
	w.End()
	for word, want := range map[string]bool{"abc": true, "def": true, "ghi": true, "abcdef": false, "defghi": false, "abcdefghi": false} {
		if w.mayHoldWord(word) != want {
			t.Errorf("mayHoldWord(%q) = %v, want %v", word, !want, want)
		}
	}
	var none *RequestWords
	if !none.mayHoldWord("anything") {
		t.Error("no filter says a word is absent")
	}
}

// TestRequestWordsReadAFormAWindowEndsInsideWhole: a word that only a
// decoded form of the body holds (a percent-escape, a backslash escape, a
// quoted-printable soft break inside it) stays one word where the window
// the filter reads ends just past its escape: the window is cut before the
// word, never inside its escape. A text of exactly requestWordsMaxBytes is
// read; one byte past it, the filter may hold any word.
func TestRequestWordsReadAFormAWindowEndsInsideWhole(t *testing.T) {
	for _, word := range []string{`tok%33n`, `tok\u0033n`, "tok=\r\n3n"} {
		// The window ends inside a run of a letter no word holds (é),
		// after the word, so its last byte to cut before is in the word
		// unless the escape's bytes are kept from the cut.
		lead := requestWordsWindow - len(word) - 100
		body := strings.Repeat("x ", lead/2) + strings.Repeat(" ", lead%2) + word + strings.Repeat("é", 100) + " tail"
		w := NewRequestWords()
		_, _ = w.Write([]byte(body))
		w.End()
		if !w.mayHoldWord("tok3n") || !w.mayHoldWord("tail") {
			t.Errorf("%q: a window that ends past its escape split the word it decodes to", word)
		}
		if w.mayHoldWord("zz9absent") {
			t.Errorf("%q: the filter holds a word the body lacks", word)
		}
	}
	w := NewRequestWords()
	spaces := []byte(strings.Repeat(" ", 1<<20))
	for i := 0; i < requestWordsMaxBytes>>20; i++ {
		_, _ = w.Write(spaces)
	}
	w.End()
	if w.undecoded || w.mayHoldWord("zz9absent") {
		t.Fatal("a text of the most bytes read is not read")
	}
	_, _ = w.Write([]byte("omega"))
	w.End()
	if !w.mayHoldWord("zz9absent") || !w.MayHoldChange("a-1", "a-2") {
		t.Fatal("past the most bytes read, the filter says a word is absent")
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
		if !got[0].unreadWords.mayHoldWord(word) {
			t.Errorf("the place's words lack %q", word)
		}
	}
	done(live(), "its body is larger than keploy reads", body())
	if got := tr.Placed().Earlier; got[1].unreadWords == nil || !got[1].unreadWords.mayHoldWord("bodyword") || got[1].Unread == "" {
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
// token shaped as an instant (an ISO 8601 date, a time of day, a run of
// nine digits or more), and every value where it held a run longer than a
// window. An upload of random bytes holds digits but no such token: a
// later read of the clock is kept, as with no request. A window's cut never
// splits a time of day so neither side holds it.
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
	// read is the value's change from rec to live as classify reads it and
	// as the decider decides it, word by word where its words changed.
	read := func(typ Type, rec, live string, earlier []Earlier) (Learned, []Decision) {
		sh := func(v string) Shape {
			return Shape{Leaves: map[string]Leaf{p: {Path: p, Noise: p, Type: typ, Value: v}}}
		}
		ln := ClassifyArrived(clockedDrifts(), []string{p}, sh(live), sh(rec), rAt, fAt, earlier, nil, nil, nil, nil)[0]
		in := iCall(p, typ, rec, live, rAt, fAt, nil, earlier, nil)
		in.Drifts = oClientDrifts()
		d := Decide(in)
		if len(d) == 0 {
			t.Fatalf("%s -> %s: no decision", rec, live)
		}
		return ln, d
	}
	decidedUnread := func(ds []Decision) bool {
		for _, d := range ds {
			if d.Outcome != OutcomeRefused || d.Refusal == nil || d.Refusal.Kind != RefusalUnread || !strings.Contains(d.Refusal.Why, why) {
				return false
			}
		}
		return true
	}
	refusedFor := func(ln Learned, ds []Decision) bool {
		return ln.Kind == KindUnaccounted && strings.Contains(ln.Why, why) && decidedUnread(ds)
	}
	keeps := func(typ Type, rec, live string, earlier []Earlier) {
		t.Helper()
		ln, d := read(typ, rec, live, earlier)
		ln0, d0 := read(typ, rec, live, nil)
		if ln.Kind != ln0.Kind || ln.Why != ln0.Why || oDescribe(d) != oDescribe(d0) {
			t.Errorf("%s -> %s after a request that cannot hold it: %s (%s), %s; want as with no request: %s (%s), %s", rec, live, ln.Kind, ln.Why, oDescribe(d), ln0.Kind, ln0.Why, oDescribe(d0))
		}
	}
	refuses := func(typ Type, rec, live string, earlier []Earlier) {
		t.Helper()
		if ln, d := read(typ, rec, live, earlier); !refusedFor(ln, d) {
			t.Errorf("%s -> %s after a request that may hold it: %s (%s), %s; want refused for %q", rec, live, ln.Kind, ln.Why, oDescribe(d), why)
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
	// What changed is read by where each word stands and by each form of
	// the value: two words that swap places changed, though the recorded
	// value held both, and so did a word that moved, the body holding it
	// in another case or not; a word that stayed in its place is no
	// change; a word only the value's base64 run holds changed in it.
	keeps(String, "aaa1q-bbb2q", "bbb2q-aaa1q", earlier)
	refuses(String, "rowvalue-aaa1q", "aaa1q-rowvalue", earlier)
	refuses(String, "ROWVALUE-aaa1q", "aaa1q-ROWVALUE", earlier)
	keeps(String, "rowvalue-1a2b3c4d5e6f", "rowvalue-9f8e7d6c5b4a", earlier)
	// A change of no word (a mark between two): the words cannot tell it
	// apart from one the request made.
	refuses(String, "tok-1a2b3c4d5e6f", "tok_1a2b3c4d5e6f", earlier)
	b64 := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
	refuses(String, b64(`{"k":"tok-1a2b3c4d5e6f"}`), b64(`{"k":"tok-q7w8e9r0t1y2"}`), earlier)
	keeps(String, b64(`{"k":"tok-1a2b3c4d5e6f"}`), b64(`{"k":"tok-9f8e7d6c5b4a"}`), earlier)
	// An instant read of the application's clock: it moved by the lead.
	r, l := iMs(rAt.Add(-120*time.Millisecond)), iMs(fAt.Add(-120*time.Millisecond))
	rt, lt := rAt.Add(-120*time.Millisecond).Format(time.RFC3339Nano), fAt.Add(-120*time.Millisecond).Format(time.RFC3339Nano)
	// A value that holds a moment beside a word: the request is asked for
	// the word that changed, whatever the moment did (it changed, it stayed,
	// it is a time of day, an epoch or a date), and is no source of a read
	// of the clock beside a word that stayed, though it holds that word, nor
	// beside one that changed to a word it lacks.
	for _, v := range [][2]string{
		{"app tok-1a2b3c4d5e6f at " + rt, "app tok-q7w8e9r0t1y2 at " + lt},
		{"app tok-1a2b3c4d5e6f at " + rt, "app tok-q7w8e9r0t1y2 at " + rt},
		{"tok-1a2b3c4d5e6f 07:01:41", "tok-q7w8e9r0t1y2 07:02:50"},
		{"tok-1a2b3c4d5e6f-1762671701", "tok-q7w8e9r0t1y2-1762671770"},
		{"tok-1a2b3c4d5e6f 2026-11-09", "tok-q7w8e9r0t1y2 2026-11-09"},
		{"app tok-1a2b3c4d5e6f at " + rt, "app TOK-ZQ5UPPER at " + lt},
	} {
		refuses(String, v[0], v[1], earlier)
	}
	keeps(String, "app tok-q7w8e9r0t1y2 at "+rt, "app tok-q7w8e9r0t1y2 at "+lt, earlier)
	keeps(String, "app tok-1a2b3c4d5e6f at "+rt, "app tok-9f8e7d6c5b4a at "+lt, earlier)
	keeps(String, "tok-q7w8e9r0t1y2-1762671701", "tok-q7w8e9r0t1y2-1762671770", earlier)
	// A mark that changed beside the moment, and a word the recorded value
	// held and the live one lost: no word tells either apart from a change
	// the request made.
	refuses(String, "booked at "+rt, "booked_at "+lt, earlier)
	refuses(String, "booked at noon", "booked at "+lt, earlier)
	// A moment's own words are not asked: a request that holds the words
	// the clock's instant changed to ("02", "50"), none of them shaped as an
	// instant, is no source of it, inside a base64 run of the value too.
	// It is the source of a value those words changed in with no moment.
	ownWords := placed(big(`"rowvalue","n17",`, `50","02","88Z","2026`))
	keeps(String, "booked at "+rt, "booked at "+lt, ownWords)
	keeps(String, b64(`{"k":"tok","at":"`+rt+`"}`), b64(`{"k":"tok","at":"`+lt+`"}`), ownWords)
	refuses(String, "row 41 of 01", "row 50 of 02", ownWords)
	// A time-ordered UUID names no moment: a request that holds an instant
	// and none of the UUID's words is no source of it.
	dated := placed(big(`"rowvalue","n17",`, "2026-11-09T10:00:00Z"))
	keeps(String, "id 018f6a1e-7c3b-7a10-8f21-3c5d7e9a1b2c", "id 019a6b2f-8d4c-7b21-9a32-4d6e8fab2c3d", dated)
	keeps(String, "tok-1a2b3c4d5e6f", "tok-9f8e7d6c5b4a", dated)
	if ln, _ := read(Instant, r, l, nil); ln.Kind == KindUnaccounted {
		t.Fatalf("%s -> %s with no request: %s (%s); want learned, a read of the clock", r, l, ln.Kind, ln.Why)
	}
	tod := func(at time.Time) string {
		return TimeOfDayLeaf(p, p, at.Sub(at.Truncate(24*time.Hour)), time.Millisecond).Value
	}
	rT, lT := tod(rAt.Add(-120*time.Millisecond)), tod(fAt.Add(-120*time.Millisecond))
	// A moment the recorded value held and the live one does not, and an
	// instant leaf whose text names no moment a text is read for (a time
	// of day), are changes of an instant too: a request that holds a token
	// shaped as an instant may be their source.
	momentsRefused := func(body string, earlier []Earlier) {
		t.Helper()
		refuses(Instant, r, l, earlier)
		refuses(String, "booked at "+rt, "booked at "+lt, earlier)
		refuses(String, "booked at "+rt, "booked at noon", earlier)
		refuses(String, "booked at noon", "booked at noon "+lt, earlier)
		// Classify reads a time of day as no read of the clock, whatever
		// came before it (unaccounted); the decider refuses it for the
		// request.
		if ln, d := read(Instant, rT, lT, earlier); ln.Kind != KindUnaccounted || !decidedUnread(d) {
			t.Errorf("%s: %s -> %s after a request that may hold it: %s (%s), %s; want refused for %q", body, rT, lT, ln.Kind, ln.Why, oDescribe(d), why)
		}
	}
	// One that holds none is no source of them.
	momentsKept := func(earlier []Earlier) {
		t.Helper()
		keeps(Instant, r, l, earlier)
		keeps(String, "booked at "+rt, "booked at "+lt, earlier)
		keeps(String, "booked at "+rt, "booked at noon", earlier)
		keeps(String, "booked at noon", "booked at noon "+lt, earlier)
		keeps(String, "booked for noon", "booked "+lt+" for noon", earlier)
		keeps(Instant, rT, lT, earlier)
	}
	// The body's digits ("n17", "q7w8e9r0t1y2") are no instant's.
	momentsKept(earlier)
	// A token only a decoded form holds counts too: a sign-in time inside
	// a base64 run, a time of day percent-escaped.
	iat := base64.RawURLEncoding.EncodeToString([]byte(`{"iat":1762682400}`))
	for _, at := range []string{"2026-11-09T10:00:00Z", "1762682400", "999999999", "booked 10:00:41", iat, "at 10%3A41"} {
		momentsRefused(at, placed(big(`"rowvalue","n17",`, at)))
	}
	momentsKept(placed(big(`"rowvalue","n17",`, "12345678")))
	letters := placed(big(`"rowvalue","nseventeen",`, "abc"))
	momentsKept(letters)
	keeps(String, "tok-1a2b3c4d5e6f", "tok-9f8e7d6c5b4a", letters)
	// An upload of random bytes, no digit of it beside another, so it holds
	// no instant's shape, but digits: a later read of the clock is kept.
	// One that holds an instant among them may be its source.
	noise := make([]byte, 2<<20)
	rnd := rand.New(rand.NewPCG(11, 9))
	for i := range noise {
		noise[i] = byte(rnd.Uint32())
		if i > 0 && isDigit(noise[i]) && isDigit(noise[i-1]) {
			noise[i] = 'x'
		}
	}
	if !bytes.ContainsAny(noise, "0123456789") {
		t.Fatal("the random bytes hold no digit")
	}
	momentsKept(placed(string(noise)))
	in := func(token string) string {
		return string(noise[:1<<20]) + " " + token + " " + string(noise[1<<20:])
	}
	momentsKept(placed(in("12345678")))
	for _, at := range []string{"2026-11-09T10:00:00Z", "1762682400"} {
		momentsRefused("random bytes and "+at, placed(in(at)))
	}
	// A time of day to the minute that a window ends inside: cut before
	// its ':', neither side would hold it.
	split := strings.Repeat("abc ", (requestWordsWindow-4)/4) + "10:00" + strings.Repeat(" abc", 1000)
	if split[requestWordsWindow-2] != ':' {
		t.Fatalf("the window ends at %q, not inside the time of day", split[requestWordsWindow-4:requestWordsWindow])
	}
	momentsRefused("a time of day a window ends inside", placed(split))
	oneRun := placed(`{"file":"` + strings.Repeat("QUJD", 20<<10) + `"}`)
	refuses(String, "tok-1a2b3c4d5e6f", "tok-9f8e7d6c5b4a", oneRun)
}

// Words may hold any word where there are none (nil) or they could not read
// their text (a window with no place to cut it), never fresh ones.
func TestRequestWordsMayHoldAnyWordOnlyWhereTheyCouldNotReadTheirText(t *testing.T) {
	if NewRequestWords().MayHoldAny() {
		t.Fatal("fresh words may hold any word")
	}
	if !(*RequestWords)(nil).MayHoldAny() {
		t.Fatal("no words hold no word")
	}
	w := NewRequestWords()
	_, _ = w.Write(make([]byte, requestWordsWindow))
	if !w.MayHoldAny() {
		t.Fatal("words that could not read a window may hold no word")
	}
}
