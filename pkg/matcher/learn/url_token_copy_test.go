package learn

import (
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
