package learn

import (
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/variables"
)

// A URL-safe base64 token whose words happen to pair between the recording
// and round F (a kept prefix, then one random run with no '-' or '_' in
// either run: api-server's "kepreq_" and 32 random bytes) is still one
// random value: the next run's '-' and '_' fall elsewhere, so a replay could
// never write it back word by word. Where an earlier place shows the token
// whole in both runs, a later call that sends it is one use of it whole,
// bound at that place whole, as a token whose '-' and '_' moved is
// (TestOutcomeAWholeURLSafeTokenIsOneValue). Bound at the random word, the
// replay of a run whose id holds a '-' put back the recorded word and kept
// the rest of the new id: the call matched no recording.
func TestOutcomeAPairedURLSafeTokenShownWholeIsOneUse(t *testing.T) {
	const (
		rec  = "kepreq_PnlPWeHYp6SxRRGMfFi0kUxyEdiumEmDVfOVZBrF68E"
		live = "kepreq_q4XtRm8LwZ2vNc7KbJ0sYh5GdP3fEa9TnU1oVi6WxQk"
	)
	ds := Decide(oInput("body.cmd.query._id", rec, live, oSent(rec, live), nil))
	if len(ds) != 1 {
		t.Fatalf("a later send of a token shown whole: %s; want one decision, the field whole", oDescribe(ds))
	}
	d := ds[0]
	if d.Outcome != OutcomeValue || d.At.Word != -1 || d.Recorded != rec || d.Full != live {
		t.Errorf("a later send of a token shown whole: %s; want a Value, the field whole", oDescribe(ds))
	}
	if d.From.Mock != "mock-0" || d.FromAt.Word != -1 || len(d.FromAt.Steps) != 0 {
		t.Errorf("a later send of a token shown whole: bound at %s %s; want mock-0's place whole", d.From.Mock, d.FromAt.String())
	}

	// The same token at a place of another test only binds nowhere whole:
	// its words decide it, refused as of another scope, never fresh.
	other := oSent(rec, live)
	other[0].Own, other[0].Run = false, true
	ds = Decide(oInput("body.cmd.query._id", rec, live, other, nil))
	if len(ds) == 0 {
		t.Fatal("a token shown whole in another test: no decision")
	}
	for _, d := range ds {
		oRefused(t, "a token shown whole in another test only", d, RefusalAnotherScope, "mock-0")
		if d.At.Word < 0 {
			t.Errorf("a token shown whole in another test only: %s refused whole; want its words to decide it", oDescribe([]Decision{d}))
		}
	}

	// A token shown whole only inside a longer text (a consent URL that
	// carries it): bound whole at its token there, which a replay reads
	// whole wherever the next run's '-' and '_' fall, never at the URL's
	// word or the URL whole.
	const url = "https://app.keploy.io/oauth/consent?request="
	ds = Decide(oInput("body.cmd.query._id", rec, live, oSent(url+rec, url+live), nil))
	if len(ds) != 1 || ds[0].Outcome != OutcomeValue || ds[0].At.Word != -1 || ds[0].From.Mock != "mock-0" || addrSuffix(ds[0].FromAt) != "|token.4" {
		t.Errorf("a token shown whole inside a longer text: %s; want one Value, the field whole, bound at mock-0 body.user|token.4", oDescribe(ds))
	}
}

// A token an earlier place shows only inside a longer text (a consent URL,
// a Location header) is bound at its own token there (|token.<n>), paired
// or not: a replay reads that token whole, so the next run's id, whose '-'
// and '_' fall elsewhere, is held to the id the earlier call carried. Bound
// at a word of the URL, the next run's split id missed; bound at the URL
// whole, the call was held equal to the whole URL and always missed. Where
// no token of one index reads it in both runs, nothing names it there: the
// unpaired token is refused, never bound at the URL whole.
func TestOutcomeATokenShownOnlyInsideALongerTextIsBoundAtItsToken(t *testing.T) {
	const (
		url = "https://app.keploy.io/oauth/consent?request="
		pr  = "kepreq_PnlPWeHYp6SxRRGMfFi0kUxyEdiumEmDVfOVZBrF68E"
		pf  = "kepreq_q4XtRm8LwZ2vNc7KbJ0sYh5GdP3fEa9TnU1oVi6WxQk"
		ur  = "kepreq_9fsSK78UnjdjTSFj51vJvm_CkQdnzdp6aRCUCH0PP5g"
		uf  = "kepreq_WWEEXXKJO_kzw3aZ8Wxfl5wsJUE3V7QGpBKzyTlYA_0"
	)
	seen := func(side Side, path, r, f string) Seen {
		return Seen{Side: side, Leaf: oText(path, r), Live: f, Place: path}
	}
	sent := func(mock string, before time.Duration, ss ...Seen) Earlier {
		at := oAt.Add(-before)
		return Earlier{Mock: mock, At: at, Own: true, Payload: oPayload(mock, at), Seen: ss}
	}
	for _, tc := range []struct {
		name, rec, live string
		earlier         []Earlier
		mock, from      string
	}{
		{"an unpaired token inside a URL alone", ur, uf, oSent(url+ur, url+uf), "mock-0", "body.user|token.4"},
		{"a paired token in an answer's Location, then whole", pr, pf, []Earlier{
			sent("mock-0", 2*time.Second, seen(Response, "header.Location", url+pr, url+pf)),
			sent("mock-1", time.Second, seen(Request, "body.docs.msgs.0._id", pr, pf)),
		}, "mock-0", "header.Location|token.4"},
		{"an unpaired token in an answer's Location, then whole", ur, uf, []Earlier{
			sent("mock-0", 2*time.Second, seen(Response, "header.Location", url+ur, url+uf)),
			sent("mock-1", time.Second, seen(Request, "body.docs.msgs.0._id", ur, uf)),
		}, "mock-0", "header.Location|token.4"},
		{"an unpaired key after Bearer", ur, uf, []Earlier{
			sent("mock-0", time.Second, seen(Request, "header.Authorization", "Bearer "+ur, "Bearer "+uf)),
		}, "mock-0", "header.Authorization|token.1"},
	} {
		ds := Decide(oInput("body.cmd.query._id", tc.rec, tc.live, tc.earlier, nil))
		if len(ds) != 1 || ds[0].Outcome != OutcomeValue || ds[0].At.Word != -1 || ds[0].Recorded != tc.rec || ds[0].Full != tc.live {
			t.Errorf("%s: %s; want one Value, the field whole", tc.name, oDescribe(ds))
			continue
		}
		if d := ds[0]; d.From.Mock != tc.mock || d.From.Path+addrSuffix(d.FromAt) != tc.from {
			t.Errorf("%s: bound at %s %s; want %s %s", tc.name, d.From.Mock, d.FromAt.String(), tc.mock, tc.from)
		}
	}

	// The URL holds the token at another token in each run (a part of it
	// split in round F): no token step reads it in both, so the unpaired
	// token is refused, never bound at the URL whole.
	ds := Decide(oInput("body.cmd.query._id", ur, uf, oSent(url+ur, url+"x."+uf), nil))
	if len(ds) == 0 {
		t.Fatal("an unpaired token at another token in each run: no decision")
	}
	for _, d := range ds {
		if d.Outcome == OutcomeValue {
			t.Errorf("an unpaired token at another token in each run: %s bound at %s %s; want it refused", oDescribe(ds), d.From.Mock, d.FromAt.String())
		}
	}
}

// A token an earlier place shows only in a URL's path (a consent URL
// "/oauth/consent/<id>", a REST answer's "Location: /v1/requests/<id>") or
// behind escapes (a standard key passed through url.QueryEscape) is bound
// whole at its own token there, paired or not: |urltoken.<n> for a URL-safe
// token, whose '/' a run of the base64 alphabets would take in, and |pct
// before the token step for an escaped one. A replay reads that token whole,
// wherever the next run's '-' and '_' (or '+' and '/') fall. Bound at the
// random word, the next run's split id missed; refused, the unpaired one was
// a red keploy could have placed. Where no token of one index holds it in
// both runs, or the place is another test's, nothing binds it whole there:
// it is refused, never bound at the URL whole or at a word of it.
func TestOutcomeATokenInAURLPathOrBehindEscapesBindsAtItsToken(t *testing.T) {
	const (
		path = "https://app.keploy.io/oauth/consent/"
		loc  = "/v1/oauth/requests/"
		cb   = "https://app.keploy.io/cb?code="
		pr   = "kepreq_PnlPWeHYp6SxRRGMfFi0kUxyEdiumEmDVfOVZBrF68E"
		pf   = "kepreq_q4XtRm8LwZ2vNc7KbJ0sYh5GdP3fEa9TnU1oVi6WxQk"
		ur   = "kepreq_9fsSK78UnjdjTSFj51vJvm_CkQdnzdp6aRCUCH0PP5g"
		uf   = "kepreq_WWEEXXKJO_kzw3aZ8Wxfl5wsJUE3V7QGpBKzyTlYA_0"
		ar   = "V1StGXR8Z5jdHi6BmyTaQ2"
		af   = "Pq7LmZx4Rt9Wv1Bc3Df5Gh"
		kr   = "q3Zx+9Lk/2Wm8Np4Rt6Yv1Bc5Df7Gh0Jk3Lm6Np9QrA="
		kf   = "Hk2/Pq7Lm+Zx4Rt9Wv1Bc3Df5Gh8Jk0Lm2Np6Qr4StA="
	)
	seen := func(side Side, path, r, f string) Seen {
		return Seen{Side: side, Leaf: oText(path, r), Live: f, Place: path}
	}
	sent := func(mock string, before time.Duration, ss ...Seen) Earlier {
		at := oAt.Add(-before)
		return Earlier{Mock: mock, At: at, Own: true, Payload: oPayload(mock, at), Seen: ss}
	}
	locThenWhole := func(r, f string) []Earlier {
		return []Earlier{
			sent("mock-0", 2*time.Second, seen(Response, "header.Location", loc+r, loc+f)),
			sent("mock-1", time.Second, seen(Request, "body.docs.msgs.0._id", r, f)),
		}
	}
	for _, tc := range []struct {
		name, rec, live string
		earlier         []Earlier
		from            string
	}{
		{"a paired token in a URL's path", pr, pf, oSent(path+pr, path+pf), "body.user|urltoken.6"},
		{"an unpaired token in a URL's path", ur, uf, oSent(path+ur, path+uf), "body.user|urltoken.6"},
		{"a paired token in an answer's Location path, then whole", pr, pf, locThenWhole(pr, pf), "header.Location|urltoken.3"},
		{"an unpaired token in an answer's Location path, then whole", ur, uf, locThenWhole(ur, uf), "header.Location|urltoken.3"},
		{"an id of letters and digits in a Location path, then whole", ar, af, locThenWhole(ar, af), "header.Location|urltoken.3"},
		{"a standard key escaped in a URL's query", kr, kf, oSent(cb+url.QueryEscape(kr), cb+url.QueryEscape(kf)), "body.user|pct|token.4"},
		// A client may escape any byte (RFC 3986 2.1): a letter of the id's
		// longest word escaped ("%55" for 'U') hides that word from the
		// text, which still shows the id behind |pct.
		{"an unpaired id with a letter of its longest word escaped", ur, uf, oSent(cb+strings.Replace(ur, "U", "%55", 1), cb+strings.Replace(uf, "Z", "%5A", 1)), "body.user|pct|token.4"},
	} {
		ds := Decide(oInput("body.cmd.query._id", tc.rec, tc.live, tc.earlier, nil))
		if len(ds) != 1 || ds[0].Outcome != OutcomeValue || ds[0].At.Word != -1 || ds[0].Recorded != tc.rec || ds[0].Full != tc.live {
			t.Errorf("%s: %s; want one Value, the field whole", tc.name, oDescribe(ds))
			continue
		}
		if d := ds[0]; d.From.Mock != "mock-0" || d.From.Path+addrSuffix(d.FromAt) != tc.from {
			t.Errorf("%s: bound at %s %s%s; want mock-0 %s", tc.name, d.From.Mock, d.From.Path, addrSuffix(d.FromAt), tc.from)
		}
	}

	// An earlier place that holds the escaped key at another token in
	// round F reads it whole at no address: the key binds whole at the
	// later place that does, never at the earlier one's words.
	twoPlaces := []Earlier{
		sent("mock-0", 2*time.Second, seen(Request, "body.user", cb+url.QueryEscape(kr), "https://app.keploy.io/cb?state=1&code="+url.QueryEscape(kf))),
		sent("mock-1", time.Second, seen(Request, "body.user", cb+url.QueryEscape(kr), cb+url.QueryEscape(kf))),
	}
	ds := Decide(oInput("body.cmd.query._id", kr, kf, twoPlaces, nil))
	if len(ds) != 1 || ds[0].Outcome != OutcomeValue || ds[0].At.Word != -1 || ds[0].From.Mock != "mock-1" || ds[0].From.Path+addrSuffix(ds[0].FromAt) != "body.user|pct|token.4" {
		t.Errorf("an escaped key at another token of an earlier place in round F, then readable: %s; want one Value, the field whole, bound at mock-1 body.user|pct|token.4", oDescribe(ds))
	}

	// The other way: round F's text holds the token at another token, or
	// the place is another test's. No address reads it whole there in both
	// runs, so nothing binds it whole, and no word of it is bound at the
	// URL whole.
	other := func(e []Earlier) []Earlier { e[0].Own, e[0].Run = false, true; return e }
	after := func(e []Earlier) []Earlier {
		e[0].At = oAt.Add(time.Second)
		e[0].Payload = oPayload(e[0].Mock, e[0].At)
		return e
	}
	for _, tc := range []struct {
		name, rec, live string
		earlier         []Earlier
	}{
		{"an unpaired token at another token of the path in round F", ur, uf, oSent(path+ur, path+"x/"+uf)},
		{"a paired token at another token of the path in round F", pr, pf, oSent(path+pr, path+"x/"+pf)},
		{"an unpaired token in another test's URL path", ur, uf, other(oSent(path+ur, path+uf))},
		{"a standard key escaped at another token in round F", kr, kf, oSent(cb+url.QueryEscape(kr), "https://app.keploy.io/cb?state=1&code="+url.QueryEscape(kf))},
		{"a text of the standard alphabet that is no encoder's output, escaped", kr[:42] + "=", kf[:42] + "=", oSent(cb+url.QueryEscape(kr[:42]+"="), cb+url.QueryEscape(kf[:42]+"="))},
		{"a standard key escaped in another test's URL", kr, kf, other(oSent(cb+url.QueryEscape(kr), cb+url.QueryEscape(kf)))},
		{"an unpaired token in the path of a URL sent after the call", ur, uf, after(oSent(path+ur, path+uf))},
		{"a standard key escaped in a URL sent after the call", kr, kf, after(oSent(cb+url.QueryEscape(kr), cb+url.QueryEscape(kf)))},
	} {
		ds := Decide(oInput("body.cmd.query._id", tc.rec, tc.live, tc.earlier, nil))
		if len(ds) == 0 {
			t.Errorf("%s: no decision", tc.name)
		}
		for _, d := range ds {
			if (d.Outcome == OutcomeValue || d.Outcome == OutcomeFresh) && d.At.Word == -1 {
				t.Errorf("%s: %s whole (bound at %s %s%s); want it refused, never bound or fresh", tc.name, oDescribe([]Decision{d}), d.From.Mock, d.From.Path, addrSuffix(d.FromAt))
			}
			if d.Outcome == OutcomeValue && d.FromAt.Whole() {
				t.Errorf("%s: %s bound at %s %s whole; want no word bound at the URL whole", tc.name, oDescribe([]Decision{d}), d.From.Mock, d.From.Path)
			}
		}
	}

	// A text of the standard alphabet that is no encoder's output (43
	// characters) is read word by word: a word the escapes hide from the
	// text's own words ("9Lk" of "%2B9Lk") is bound through |pct, never at
	// the URL whole, which no word of the next run could equal.
	ds = Decide(oInput("body.cmd.query._id", kr[:42]+"=", kf[:42]+"=", oSent(cb+url.QueryEscape(kr[:42]+"="), cb+url.QueryEscape(kf[:42]+"=")), nil))
	got := map[string]string{}
	for _, d := range ds {
		if d.Outcome == OutcomeValue {
			got[d.Recorded] = d.From.Path + addrSuffix(d.FromAt)
		}
	}
	for w, at := range map[string]string{"q3Zx": "body.user#6", "9Lk": "body.user|pct#7", kr[9:42]: "body.user|pct#8"} {
		if got[w] != at {
			t.Errorf("an escaped text read word by word: %q bound at %q; want %s (%s)", w, got[w], at, oDescribe(ds))
		}
	}
}

// A token a recording's place shows whole in both runs (a row an earlier
// query answered, which round F served with its own token there) binds
// there whole, paired or not: the adapter names the place of that
// recording that holds it, so the whole token is held to it. Read word by
// word, the paired token's next run, whose '-' and '_' fall elsewhere,
// could not be written back; dropped, the unpaired one bound nowhere.
func TestOutcomeATokenShownWholeAtARecordingBindsThere(t *testing.T) {
	const (
		pr = "kepreq_PnlPWeHYp6SxRRGMfFi0kUxyEdiumEmDVfOVZBrF68E"
		pf = "kepreq_q4XtRm8LwZ2vNc7KbJ0sYh5GdP3fEa9TnU1oVi6WxQk"
		ur = "kepreq_9fsSK78UnjdjTSFj51vJvm_CkQdnzdp6aRCUCH0PP5g"
		uf = "kepreq_WWEEXXKJO_kzw3aZ8Wxfl5wsJUE3V7QGpBKzyTlYA_0"
	)
	for _, tc := range []struct{ name, rec, live string }{
		{"a paired token", pr, pf},
		{"an unpaired token", ur, uf},
	} {
		raw := oAnswer("mock-5", time.Second, tc.rec).ServedAs(1).ServedLive(oAnswer("mock-5", time.Second, tc.live))
		ds := Decide(oInput("body.cmd.query._id", tc.rec, tc.live, nil, []Raw{raw}))
		if len(ds) != 1 || ds[0].Outcome != OutcomeValue || ds[0].At.Word != -1 || ds[0].Recorded != tc.rec || ds[0].Full != tc.live {
			t.Errorf("%s a recording shows whole: %s; want one Value, the field whole", tc.name, oDescribe(ds))
			continue
		}
		if d := ds[0]; d.From.Mock != "mock-5" || d.FromAt.Word != -1 || len(d.FromAt.Steps) != 0 {
			t.Errorf("%s a recording shows whole: bound at %s %s; want mock-5's recording", tc.name, d.From.Mock, d.FromAt.String())
		}
	}

	// An exchange's place that names no leaf is no recording: it binds the
	// token only where what it holds is the token, never as a recording's
	// place does. Its text here is "Bearer <token>", so the unpaired token
	// binds nowhere whole and is refused.
	e := oSent("Bearer "+ur, "Bearer "+uf)
	e[0].Seen[0].Leaf.Path = ""
	ds := Decide(oInput("body.cmd.query._id", ur, uf, e, nil))
	if len(ds) == 0 {
		t.Fatal("an unpaired token at a place that names no leaf: no decision")
	}
	for _, d := range ds {
		if d.Outcome == OutcomeValue {
			t.Errorf("an unpaired token at a place that names no leaf: %s bound at %s %s; want it refused", oDescribe(ds), d.From.Mock, d.FromAt.String())
		}
	}
}

// tokenStepAt names a token of a longer text only where that token is the
// value in both runs, at one index: never one only the recording's text
// holds there, nor one only round F's does, nor a text that is the value
// whole (named whole). A URL-safe token in a URL's path, which a run of the
// base64 alphabets joins to the path before it, is named by the URL-safe
// cut (|urltoken.<n>); a token behind escapes through |pct.
func TestTokenStepAtNamesATokenOnlyWhereBothRunsHoldTheValue(t *testing.T) {
	const url = "https://app.keploy.io/oauth/consent?request="
	const path = "https://app.keploy.io/oauth/consent/"
	const v, f = "kepreq_9fsSK78UnjdjTSFj51vJvm_CkQdnzdp6aRCUCH0PP5g", "kepreq_WWEEXXKJO_kzw3aZ8Wxfl5wsJUE3V7QGpBKzyTlYA_0"
	for _, tc := range []struct {
		name, rec, live, want string
	}{
		{"one index in both runs", url + v, url + f, "|token.4"},
		{"the first index that holds it in both", "a " + v + " " + v, "a " + f + " " + f, "|token.1"},
		{"the value whole", v, f, ""},
		{"round F's token there is another", url + v, url + "x." + f, ""},
		{"the recording's token there is another", url + "x." + v, url + f, ""},
		{"round F's text lacks the token", url + v, url, ""},
		{"the first token of a longer text", v + "&x=1", f + "&x=1", "|token.0"},
		{"the first pair of a cookie", v + "; Path=/", f + "; Path=/", "|token.0"},
		{"the recording's text whole, round F's longer", v, f + "&x=1", "|token.0"},
		{"round F's text whole, the recording's longer", v + "&x=1", f, "|token.0"},
		{"the recording's text whole, round F's token 0 another", v, "x." + f, ""},
		{"a URL's path", path + v, path + f, "|urltoken.6"},
		{"a URL's path, then a query", "/v1/requests/" + v + "?x=1", "/v1/requests/" + f + "?x=1", "|urltoken.2"},
		{"a URL's path in one run, another token in the other", path + v, path + "x." + f, ""},
		{"an escaped path", "/next%2F" + v, "/next%2F" + f, "|pct|urltoken.1"},
		{"an escaped standard key", "code=q3Zx%2B9Lk%2F2W%3D", "code=Hk2%2FPq7L%2Bm0%3D", "|pct|token.0"},
		{"an escaped key whole", "q3Zx%2B9Lk%2F2W%3D", "Hk2%2FPq7L%2Bm0%3D", "|pct"},
		{"an escape in one run alone", "code=q3Zx%2B9Lk%2F2W%3D", "code=Hk2/Pq7L+m0=", "|pct|token.0"},
		{"a bad escape", "code=q3Zx%2B9Lk%2F2W%3D%", "code=Hk2%2FPq7L%2Bm0%3D", ""},
		{"an escaped key whole in one run, the first token of a longer text in the other", "q3Zx%2B9Lk%2F2W%3D", "Hk2%2FPq7L%2Bm0%3D&x=1", "|pct|token.0"},
		{"an escaped key the first token of a longer text in one run, whole in the other", "q3Zx%2B9Lk%2F2W%3D&x=1", "Hk2%2FPq7L%2Bm0%3D", "|pct|token.0"},
	} {
		key, kf := v, f
		if strings.Contains(tc.rec, "%2B") || strings.Contains(tc.live, "+") {
			key, kf = "q3Zx+9Lk/2W=", "Hk2/Pq7L+m0="
		}
		st, ok := tokenStepAt(tc.rec, tc.live, key, kf)
		if got := addrSuffix(variables.Address{Steps: st, Word: -1}); ok != (tc.want != "") || got != tc.want {
			t.Errorf("%s: tokenStepAt = %q, %v; want %q", tc.name, got, ok, tc.want)
		}
	}

	// Past the last token of either text no token of that index is in both:
	// a value round F gave as no token, or one the recording held as none,
	// names none.
	for _, tc := range []struct{ name, rec, live, v, f string }{
		{"round F's text has no token of that index", "a " + v, "a", v, ""},
		{"the recording's text has no token of that index", "a", "a " + f, "", f},
		{"a bad escape in the recording, whose value is empty", "q%zz", "x%2Fy", "", "x/y"},
	} {
		if st, ok := tokenStepAt(tc.rec, tc.live, tc.v, tc.f); ok {
			t.Errorf("%s: tokenStepAt = %v, true; want none", tc.name, st)
		}
	}
}

// tokenStepAt reads each text once for each cut, however many tokens it
// holds: a leaf of 256 KB or 1 MB (an HTML mail that carries a reset link, a
// SOAP body) costs a pass over rec and live for each of tokenCuts, where
// reading token n afresh from the text's start for each n took seconds a
// decision at 256 KB and minutes at 1 MB. The row counts the bytes the cuts
// scan, so it holds on a slow box as on a fast one; past twice the texts'
// length for each cut they read nothing more, so a form that scans more
// stops at once and fails here.
func TestTokenStepAtReadsALongTextOnce(t *testing.T) {
	const v, f = "kepreq_9fsSK78UnjdjTSFj51vJvm_CkQdnzdp6aRCUCH0PP5g", "kepreq_WWEEXXKJO_kzw3aZ8Wxfl5wsJUE3V7QGpBKzyTlYA_0"
	cuts := tokenCuts
	t.Cleanup(func() { tokenCuts = cuts })
	var scanned, budget int
	tokenCuts = slices.Clone(cuts)
	for i := range tokenCuts {
		cut := cuts[i].next
		tokenCuts[i].next = func(s string, from, to int) (int, int, bool) {
			if scanned > budget {
				return 0, 0, false
			}
			start, end, ok := cut(s, from, to)
			if !ok {
				end = to
			}
			scanned += end - from
			return start, end, ok
		}
	}
	passes := len(cuts)
	for _, size := range []int{256 << 10, 1 << 20} {
		var b strings.Builder
		for b.Len() < size {
			b.WriteString("lorem ipsum dolor sit amet, ")
		}
		body := b.String()
		for _, tc := range []struct {
			name, rec, live, v, f string
			ok                    bool
		}{
			{"the last token", body + v, body + f, v, f, true},
			{"a value that is no token", body + v, body + f, "no such token", "nor here", false},
			{"a text with no token", strings.Repeat(" ", size), strings.Repeat(" ", size), v, f, false},
		} {
			scanned, budget = 0, 2*passes*(len(tc.rec)+len(tc.live))
			st, ok := tokenStepAt(tc.rec, tc.live, tc.v, tc.f)
			if ok != tc.ok || ok && addrSuffix(variables.Address{Steps: st, Word: -1}) != "|token."+strconv.Itoa(strings.Count(body, " ")) {
				t.Errorf("%d KB, %s: tokenStepAt = %v, %v; want %v", size>>10, tc.name, st, ok, tc.ok)
			}
			if scanned == 0 || scanned > passes*(len(tc.rec)+len(tc.live)) {
				t.Errorf("%d KB, %s: tokenStepAt scanned %d bytes of texts of %d; want each text read once for each of %d cuts", size>>10, tc.name, scanned, len(tc.rec)+len(tc.live), passes)
			}
		}
	}

	// Through Decide: one Value bound at an earlier leaf of 1 MB, the token
	// at its end, reads the leaf's two runs once each for the bind.
	var b strings.Builder
	for b.Len() < 1<<20 {
		b.WriteString("lorem ipsum dolor sit amet, ")
	}
	rec, live := b.String()+v, b.String()+f
	scanned, budget = 0, 2*passes*(len(rec)+len(live))
	ds := Decide(oInput("body.cmd.query._id", v, f, oSent(rec, live), nil))
	if len(ds) != 1 || ds[0].Outcome != OutcomeValue || ds[0].From.Mock != "mock-0" || addrSuffix(ds[0].FromAt) != "|token."+strconv.Itoa(strings.Count(b.String(), " ")) {
		t.Errorf("a token at the end of a 1 MB leaf: %s; want one Value bound at mock-0 body.user at its last token", oDescribe(ds))
	}
	if scanned == 0 || scanned > passes*(len(rec)+len(live)) {
		t.Errorf("a token at the end of a 1 MB leaf: Decide scanned %d bytes of texts of %d; want each read once for each cut", scanned, len(rec)+len(live))
	}
}

// addrSuffix is a's steps and word as an address writes them after its
// place ("|token.4", "#1"): "" for a place named whole.
func addrSuffix(a variables.Address) string {
	a.Place = variables.Place{Part: variables.PartHeader, Name: "X"}
	return strings.TrimPrefix(a.String(), a.Place.String())
}

// A JWT's signature whose words happen to pair (one '-' in each run, at
// another spot) that a later call sends whole, copied from an earlier
// call's "Bearer <jwt>": one Value, the field whole, bound at the JWT's
// signature (|jwtsig), a place whose text at that address is the signature
// in both runs though the leaf is the longer header. Bound at its words,
// the next run's signature, whose '-' and '_' fall elsewhere, missed.
func TestOutcomeAPairedSignatureCopiedFromAJWTIsOneUse(t *testing.T) {
	const p, from = "body.docs.msgs.0.key", "header.Authorization"
	const rSig, fSig = "vWyN5YYptqCYhsLsN8P3-41Rrq8a4ycfLtS541165V4", "Q1xYp9vTq3LmN0Rr4sWk-Hd2cJ7aBfE5gUoZiXyPq7Q"
	tok, rAt, fAt := sigCallFrom(from, rSig, hs256, fSig, nil)
	rec, live := tok.Rec.Leaves[from].Value, tok.Live.Leaves[from].Value
	sent := iSent("mock-870", rAt.Add(-5*time.Millisecond), from, String, "Bearer "+rec, "Bearer "+live)
	ds := Decide(iCall(p, String, rSig, fSig, rAt, fAt, iWin(rAt, fAt, 30*time.Millisecond), []Earlier{sent}, nil))
	if len(ds) != 1 || ds[0].Outcome != OutcomeValue || ds[0].At.Word != -1 || ds[0].From.Mock != "mock-870" || addrSuffix(ds[0].FromAt) != "|jwtsig" {
		t.Errorf("a paired signature copied from a JWT: %s; want one Value, the field whole, bound at mock-870 %s|jwtsig", oDescribe(ds), from)
	}
}

// The rows the whole reading must not loosen. A paired URL-safe token no
// earlier place shows whole is read word by word as before: its kept prefix
// stays checked, and a random word an earlier place holds alone is bound
// there (an id carried inside a longer value). An earlier place that held
// the recorded token whole where round F gave another value there disproves
// it: refused, never made anew.
func TestOutcomeAPairedURLSafeTokenNotShownWholeKeepsItsWords(t *testing.T) {
	const (
		rw   = "PnlPWeHYp6SxRRGMfFi0kUxyEdiumEmDVfOVZBrF68E"
		lw   = "q4XtRm8LwZ2vNc7KbJ0sYh5GdP3fEa9TnU1oVi6WxQk"
		rec  = "kepreq_" + rw
		live = "kepreq_" + lw
	)
	// First send: nothing before it. The random word is made anew, the
	// prefix stays compared.
	ds := Decide(oInput("body.docs.msgs.0._id", rec, live, nil, nil))
	if len(ds) != 1 || ds[0].Outcome != OutcomeFresh || ds[0].At.Word != 1 || ds[0].Recorded != rw {
		t.Errorf("a first send: %s; want its random word fresh, the prefix compared", oDescribe(ds))
	}
	// An earlier place holds the random word alone, not the token.
	d := oWord(t, Decide(oInput("body.cmd.query._id", rec, live, oSent(rw, lw), nil)), rw, lw)
	if d.Outcome != OutcomeValue || d.At.Word != 1 || d.From.Mock != "mock-0" {
		t.Errorf("an earlier place holds the random word alone: %s; want word 1 bound at mock-0", oDescribe([]Decision{d}))
	}
	// An earlier place held the recorded token whole, and round F gave
	// another one there.
	moved := oSent(rec, "kepreq_ZZ0aYb1Xc2Wd3Ve4Uf5Tg6Sh7Ri8Qj9Pk0Ol1Nm2L")
	ds = Decide(oInput("body.cmd.query._id", rec, live, moved, nil))
	if len(ds) == 0 {
		t.Fatalf("an earlier place disproves the token: no decision")
	}
	for _, d := range ds {
		if d.Outcome == OutcomeFresh || d.Outcome == OutcomeValue {
			t.Errorf("FALSE GREEN? an earlier place held the recorded token whole and round F gave another there: %s; want refused", oDescribe(ds))
		}
	}
	// The earlier place only in the recording, after the call: no source.
	late := oSent(rec, live)
	late[0].At = oAt.Add(time.Second)
	ds = Decide(oInput("body.cmd.query._id", rec, live, late, nil))
	for _, d := range ds {
		if d.Outcome == OutcomeValue {
			t.Errorf("a place after the call: %s; want no Value bound there", oDescribe(ds))
		}
	}
}

// A paired token read whole binds whole only at a place whose text at the
// bind address is the token in both runs: the place its bind names, by
// mock, field and side. Where the earliest place that shows it holds it
// inside a longer text that no address reads it whole in (at another token
// in each run, or in one run alone), and a place beside it that
// does hold it whole (another mock's, another field of the same exchange,
// the exchange's other side) is no excuse: its words decide it, as before,
// never a bind of the longer text whole.
func TestOutcomeAPairedURLSafeTokenBindsWholeOnlyAtALeafThatHoldsIt(t *testing.T) {
	const (
		rw   = "PnlPWeHYp6SxRRGMfFi0kUxyEdiumEmDVfOVZBrF68E"
		lw   = "q4XtRm8LwZ2vNc7KbJ0sYh5GdP3fEa9TnU1oVi6WxQk"
		rec  = "kepreq_" + rw
		live = "kepreq_" + lw
		url  = "https://app.keploy.io/oauth/consent?request="
		// split puts round F's token at the next token of the URL: no
		// token step reads it in both runs.
		split = "x."
	)
	seen := func(side Side, path, r, f string) Seen {
		return Seen{Side: side, Leaf: oText(path, r), Live: f, Place: path}
	}
	sent := func(mock string, before time.Duration, ss ...Seen) Earlier {
		at := oAt.Add(-before)
		return Earlier{Mock: mock, At: at, Own: true, Payload: oPayload(mock, at), Seen: ss}
	}
	for _, tc := range []struct {
		name    string
		earlier []Earlier
	}{
		{"another mock holds it whole after the one inside a URL", []Earlier{
			sent("mock-0", 2*time.Second, seen(Request, "body.user", url+rec, url+split+live)),
			sent("mock-1", time.Second, seen(Request, "body.user", rec, live)),
		}},
		{"another field of the same exchange holds it whole", []Earlier{
			sent("mock-0", time.Second, seen(Request, "body.url", url+rec, url+split+live), seen(Request, "body.id", rec, live)),
		}},
		{"the exchange's answer holds it whole", []Earlier{
			sent("mock-0", time.Second, seen(Request, "body.user", url+rec, url+split+live), seen(Response, "body.user", rec, live)),
		}},
		{"the place holds it inside a URL in the recording alone", []Earlier{
			sent("mock-0", time.Second, seen(Request, "body.user", url+rec, live)),
		}},
		{"the place holds it inside a URL in round F alone", []Earlier{
			sent("mock-0", time.Second, seen(Request, "body.user", rec, url+live)),
		}},
	} {
		ds := Decide(oInput("body.cmd.query._id", rec, live, tc.earlier, nil))
		if len(ds) == 0 {
			t.Errorf("%s: no decision", tc.name)
		}
		for _, d := range ds {
			if d.At.Word == -1 && d.Outcome == OutcomeValue {
				t.Errorf("%s: %s bound at %s; want its words to decide it, never the token whole at a longer text", tc.name, oDescribe(ds), d.FromAt.String())
			}
		}
	}
}

// A paired token no earlier place shows whole is decided by its words
// alone: never read whole first, so a set of such ids costs a decision a
// changed word, as before.
func TestOutcomeAPairedURLSafeTokenNotShownIsDecidedOnce(t *testing.T) {
	const rec, live = "kepreq_PnlPWeHYp6SxRRGMfFi0kUxyEdiumEmDVfOVZBrF68E", "kepreq_q4XtRm8LwZ2vNc7KbJ0sYh5GdP3fEa9TnU1oVi6WxQk"
	before := wordPasses.Load()
	ds := Decide(oInput("body.docs.msgs.0._id", rec, live, nil, nil))
	if n := wordPasses.Load() - before; n != int64(len(ds)) || len(ds) != 1 {
		t.Errorf("a paired token no place shows: %d word passes for %s; want one, its changed word's", n, oDescribe(ds))
	}
}
