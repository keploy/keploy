package learn

import (
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// TestACallMadeWhileAClientRequestsBodyComesReadsWhatPassedOfIt: a call
// that reaches keploy while a client test's request still sends its body
// (an auth middleware's write of the clock during a 2 MB upload) reads the
// request's URL and headers, which keploy has, and of its body the words
// that passed to the application by the time the call is judged: a value
// they cannot hold is learned as after a request of those headers and no
// body, one they may hold is refused for the body still coming, and a value
// a header carries binds at that header. Where keploy cannot tell what
// passed (no taker tells it, a body sent encoded, a run it could not read)
// every value that changed is refused, the header's too.
func TestACallMadeWhileAClientRequestsBodyComesReadsWhatPassedOfIt(t *testing.T) {
	const p = "body.token"
	rAt := time.Date(2026, 11, 9, 7, 1, 42, 0, time.UTC)
	fAt := rAt.Add(69 * time.Second)
	arrived := fAt.Add(-time.Second)
	rec := func(body string) *models.HTTPReq {
		return &models.HTTPReq{Method: "POST", URL: "http://api/upload", Header: map[string]string{"X-Run": "run-recorded7"}, Body: body, Timestamp: rAt.Add(-time.Second)}
	}
	live := func() *models.HTTPReq {
		return &models.HTTPReq{Method: "POST", URL: "http://api/upload", Header: map[string]string{"X-Run": "run-sent9"}}
	}
	// coming are the places a call read while the request's body came,
	// passed telling the words that passed of it.
	coming := func(passed func() *RequestWords) []Earlier {
		tr := NewTracker()
		tr.Begin()
		tr.SetCases([]ClientCase{{Name: "post-upload-1", Req: rec(`{"file":"recorded upload"}`)}})
		if tr.IncomingComing(live(), arrived, passed) == nil {
			t.Fatal("the coming request did not pair")
		}
		got := tr.Placed().Earlier
		if len(got) != 1 || got[0].Unread != ComingUnread {
			t.Fatalf("placed while coming %+v", got)
		}
		return got
	}
	// head are the places of a call after the same request sent with no
	// body: its URL and headers alone.
	tr := NewTracker()
	tr.Begin()
	tr.SetCases([]ClientCase{{Name: "post-upload-1", Req: rec("")}})
	if !tr.Incoming(live(), "", arrived) {
		t.Fatal("the request with no body did not pair")
	}
	head := tr.Placed().Earlier
	read := func(typ Type, rec, live string, earlier []Earlier) (Learned, string) {
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
		return ln, oDescribe(d)
	}
	asHead := func(typ Type, rec, live string, earlier []Earlier) Learned {
		t.Helper()
		ln, d := read(typ, rec, live, earlier)
		ln0, d0 := read(typ, rec, live, head)
		if ln.Kind != ln0.Kind || ln.Why != ln0.Why || d != d0 {
			t.Errorf("%s -> %s while a body that cannot hold it came: %s (%s), %s; want as after its headers alone: %s (%s), %s", rec, live, ln.Kind, ln.Why, d, ln0.Kind, ln0.Why, d0)
		}
		return ln
	}
	refuses := func(typ Type, rec, live string, earlier []Earlier) {
		t.Helper()
		ln, d := read(typ, rec, live, earlier)
		if ln.Kind != KindUnaccounted || !strings.Contains(ln.Why, ComingUnread) || !strings.Contains(d, ComingUnread) || strings.Contains(d, "learned") {
			t.Errorf("%s -> %s while a body that may hold it came: %s (%s), %s; want refused for the body still coming", rec, live, ln.Kind, ln.Why, d)
		}
	}

	// A 2 MB upload, half of it passed: the word the second half holds has
	// not reached the application.
	var first strings.Builder
	first.WriteString(`{"file":"passedword4x `)
	for first.Len() < 1<<20 {
		first.WriteString("rowvalue n17 ")
	}
	w := NewRequestWords()
	_, _ = w.Write([]byte(first.String() + "cutwor"))
	passed := func() *RequestWords { return w.Passed() }
	earlier := coming(passed)

	// The middleware's clock write, and a token no word of which passed.
	asHead(Instant, "2026-11-09T07:01:42Z", "2026-11-09T07:02:51Z", earlier)
	asHead(String, "tok-1a2b3c4d5e6f", "tok-9f8e7d6c5b4a", earlier)
	asHead(String, "tok-1a2b3c4d5e6f", "tok-laterword8z", earlier)
	// One the passed half holds, and the text the body was cut in.
	refuses(String, "tok-1a2b3c4d5e6f", "tok-passedword4x", earlier)
	refuses(String, "tok-1a2b3c4d5e6f", "tok-cutwor", earlier)
	// The header's value binds at the header, as after the headers alone.
	if ln := asHead(String, "run-recorded7", "run-sent9", earlier); ln.Kind == KindUnaccounted {
		t.Errorf("the value a header of the coming request carries is %s (%s); want bound at the header", ln.Kind, ln.Why)
	}

	// The same read of the places, judged once more of the body passed.
	_, _ = w.Write([]byte(`d laterword8z "]}`))
	refuses(String, "tok-1a2b3c4d5e6f", "tok-laterword8z", earlier)
	asHead(String, "tok-1a2b3c4d5e6f", "tok-9f8e7d6c5b4a", earlier)
	// Passed read the text as if it ended, and left the words taking it:
	// the word it was cut in is whole now, and the copy holds no later one.
	then := NewRequestWords()
	_, _ = then.Write([]byte("early1word cutwor"))
	cut := then.Passed()
	_, _ = then.Write([]byte("d9 late2word"))
	then.End()
	for word, want := range map[string][2]bool{"early1word": {true, true}, "cutwor": {true, false}, "cutword9": {false, true}, "late2word": {false, true}} {
		if got := [2]bool{cut.MayHoldChange("x", word), then.MayHoldChange("x", word)}; got != want {
			t.Errorf("%q: held by what passed and by the whole %v, want %v", word, got, want)
		}
	}
	if (*RequestWords)(nil).Passed() != nil {
		t.Error("no words passed as some")
	}

	// Keploy cannot tell what passed: every changed value is refused.
	long := NewRequestWords()
	_, _ = long.Write([]byte(strings.Repeat("a", requestWordsWindow+1)))
	if !long.MayHoldAny() {
		t.Fatal("a run longer than a window was read")
	}
	for name, passed := range map[string]func() *RequestWords{
		"no taker tells it":       nil,
		"a body sent encoded":     func() *RequestWords { return nil },
		"a run it could not read": func() *RequestWords { return long.Passed() },
	} {
		earlier := coming(passed)
		if got := arrivedBefore(earlier, rAt, fAt); len(got) != 1 || got[0].Unread != ComingUnread || got[0].unreadWords != nil || got[0].coming != nil {
			t.Errorf("%s: read as %+v, want one place keploy could not read", name, got)
		}
		refuses(String, "tok-1a2b3c4d5e6f", "tok-9f8e7d6c5b4a", earlier)
		refuses(String, "run-recorded7", "run-sent9", earlier)
	}

	// The places a call reads it as: its headers, then its body with the
	// words that passed; the read places themselves stay as they were, and
	// a request that came after the call is left out whole.
	got := arrivedBefore(earlier, rAt, fAt)
	if len(got) != 2 || got[0].Unread != "" || !holdsSeen(got[0], "header.X-Run", "run-recorded7", "run-sent9") || holdsSeen(got[0], "body", `{"file":"recorded upload"}`, "") ||
		!got[0].incoming || !got[0].arrivedRec.Equal(rAt.Add(-time.Second)) || !got[0].arrivedLive.Equal(arrived) ||
		got[1].Unread != ComingUnread || got[1].unreadWords == nil || got[1].coming != nil || got[1].Mock != got[0].Mock {
		t.Fatalf("a coming request is read as %+v", got)
	}
	if earlier[0].coming == nil || earlier[0].unreadWords != nil {
		t.Fatalf("reading a coming place changed it: %+v", earlier[0])
	}
	if got := arrivedBefore(earlier, rAt, arrived.Add(-time.Nanosecond)); len(got) != 0 {
		t.Fatalf("a request that came after the call is read as %+v", got)
	}
	if w := earlier[0].Words(); w == nil || !w.MayHoldChange("x", "passedword4x") {
		t.Fatal("the place does not tell the words that passed")
	}
}
