package learn

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/variables"
)

// api-server's OAuth client_id is its registration, sealed: "kepclient_",
// base64url of a version byte and the client's JSON metadata, '.', and
// base64url of a 16-byte HMAC tag of that payload. A client test registers
// a client it names by its own clock, gets the client_id back, and sends it
// whole at the authorize endpoint (url.query.client_id); the application
// writes it whole to oauth_authorization_requests. The base64url '-' and '_'
// of the two runs' client_ids fall at other places, so keploy's words of
// the two runs do not pair: the field was refused as unknown, "a word was
// added or dropped beside one it holds", naming the very request that holds
// it whole in both runs.

// sealedClientID is a client_id as api-server seals it (clientid.go
// sealClientID), its client named name, under key.
func sealedClientID(name, key string) string {
	payload := append([]byte{1}, `{"n":"`+name+`","r":["http://127.0.0.1:45679/callback"],"g":["authorization_code","refresh_token"]}`...)
	m := hmac.New(sha256.New, []byte(key))
	m.Write(payload)
	return "kepclient_" + base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(m.Sum(nil)[:16])
}

// authorizeCase is the client test's authorize request, the client_id in its
// query, sent at at.
func authorizeCase(clientID string, at time.Time) *models.HTTPReq {
	return &models.HTTPReq{Method: "GET", URL: "http://localhost:8083/oauth/authorize?client_id=" + clientID + "&response_type=code&scope=read",
		Timestamp: at, Header: map[string]string{"Host": "localhost:8083"}}
}

// sealedPair is a recorded and a round F client_id of a client named by
// two UnixNano reads, sealed under one key, whose reading (readLeaf) is of
// kind: "unpaired" (a word one run lacks), "shape" (every word paired, a
// separator changed that no whole token holds: ChangedShape) or "paired"
// (every word paired, the shape kept), searched from rNanos.
func sealedPair(t *testing.T, rNanos int64, kind string) (rec, live string) {
	t.Helper()
	name := func(n int64) string { return "E2E Blocked Agent " + strconv.FormatInt(n, 10) }
	rec = sealedClientID(name(rNanos), "e2e-secret")
	for f := rNanos + int64(67*time.Second); f < rNanos+int64(67*time.Second)+200000*7919; f += 7919 {
		live = sealedClientID(name(f), "e2e-secret")
		rd := readLeaf(oClientDrifts(), variables.Address{Word: -1}, rec, live)[0]
		unpaired := slices.ContainsFunc(rd.Words, func(w readWord) bool { return w.RecIdx < 0 || w.LiveIdx < 0 })
		if kind == "unpaired" && unpaired || kind == "shape" && !unpaired && rd.ChangedShape || kind == "paired" && !unpaired && !rd.ChangedShape {
			return rec, live
		}
	}
	t.Fatalf("no round F client_id whose reading is %s", kind)
	return
}

// staffBlockCall is the insert of the client_id rec (live in round F) after
// the client test's authorize request, which sent rq (fq in round F).
func staffBlockCall(t *testing.T, rec, live, rq, fq string) Input {
	t.Helper()
	rReq := time.Unix(0, 1794446576090003375).Add(time.Millisecond)
	fReq := rReq.Add(67 * time.Second)
	rCall, fCall := rReq.Add(2*time.Millisecond), fReq.Add(2*time.Millisecond)
	in := iCall("body.docs.msgs.{}.client_id", String, rec, live, rCall, fCall, nil,
		clientPlaced(t, "get-oauth-authorize-2", authorizeCase(rq, rReq), authorizeCase(fq, fReq), fReq), nil)
	in.Drifts = oClientDrifts()
	return in
}

// TestAClientIDTheClientTestSentWholeIsBoundAtItsRequest: the insert's
// client_id is the authorize request's url.query.client_id whole, in both
// runs: a Value bound there whole, its words paired or not.
func TestAClientIDTheClientTestSentWholeIsBoundAtItsRequest(t *testing.T) {
	for _, kind := range []string{"unpaired", "shape"} {
		t.Run(kind, func(t *testing.T) {
			rec, live := sealedPair(t, 1794446576090003375, kind)
			d := oWord(t, Decide(staffBlockCall(t, rec, live, rec, live)), rec, live)
			if d.Outcome != OutcomeValue || !d.From.Client || d.From.Path != "url.query.client_id" || d.At.Word >= 0 || d.FromAt.String() != "url.query.client_id" {
				t.Fatalf("outcome %v from %s %s (client %v) at word %d, bound at %q: %s; want a Value of the request's client_id whole",
					d.Outcome, d.From.Mock, d.From.Path, d.From.Client, d.At.Word, d.FromAt.String(), d.Line)
			}
		})
	}
}

// TestAClientIDNoEarlierLeafHoldsWholeInBothRunsStaysRefused: where no leaf
// before the call holds the field whole in both runs (the request sent
// another client_id in one run, or it is another test's), or the runs'
// words pair, the whole-copy rule decides nothing: the field is refused as
// before, or read word by word.
func TestAClientIDNoEarlierLeafHoldsWholeInBothRunsStaysRefused(t *testing.T) {
	rec, live := sealedPair(t, 1794446576090003375, "unpaired")
	other, _ := sealedPair(t, 1794446576090003375+int64(time.Hour), "unpaired")
	for _, tc := range []struct{ name, rq, fq string }{
		{"round F's request sent another client_id", rec, other},
		{"the recorded request sent another client_id", other, live},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := oWord(t, Decide(staffBlockCall(t, rec, live, tc.rq, tc.fq)), rec, live)
			if d.Outcome != OutcomeRefused {
				t.Fatalf("outcome %v from %s %s: %s; want it refused", d.Outcome, d.From.Mock, d.From.Path, d.Line)
			}
		})
	}
	t.Run("another test's request", func(t *testing.T) {
		in := staffBlockCall(t, rec, live, rec, live)
		e := iSent("mock-9", in.At.Add(-time.Millisecond), "url.query.client_id", String, rec, live)
		e.Own, e.Run = false, true
		in.Earlier = []Earlier{e}
		d := oWord(t, Decide(in), rec, live)
		if d.Outcome == OutcomeValue && d.At.Word < 0 {
			t.Fatalf("bound whole to %s %s: %s; want no whole bind to another test's place", d.From.Mock, d.From.Path, d.Line)
		}
	})
	t.Run("the words pair", func(t *testing.T) {
		rec, live := sealedPair(t, 1794446576090003375, "paired")
		for _, d := range Decide(staffBlockCall(t, rec, live, rec, live)) {
			if d.At.Word < 0 {
				t.Fatalf("decided whole: %s; want its changed words read one by one", d.Line)
			}
		}
	})
}

// TestAClientIDIsBoundWholeOnlyAtAPlaceBeforeTheCallInBothRuns: an exchange
// of the call's own test that sent the client_id whole in both runs, before
// the call, is the value's source, bound there whole. One round F served only
// after the call reached keploy is no place before the call (round F's
// application gave it nothing by then): the field is not bound whole there.
func TestAClientIDIsBoundWholeOnlyAtAPlaceBeforeTheCallInBothRuns(t *testing.T) {
	rec, live := sealedPair(t, 1794446576090003375, "unpaired")
	for _, tc := range []struct {
		name  string
		after bool
	}{{"sent before the call", false}, {"round F served it only after the call", true}} {
		t.Run(tc.name, func(t *testing.T) {
			in := staffBlockCall(t, rec, live, rec, live)
			e := iSent("mock-9", in.At.Add(-time.Millisecond), "url.query.client_id", String, rec, live)
			e.Payload.servedAfter = tc.after
			in.Earlier = []Earlier{e}
			d := oWord(t, Decide(in), rec, live)
			whole := d.Outcome == OutcomeValue && d.At.Word < 0
			if tc.after && whole {
				t.Fatalf("bound whole to %s %s: %s; want no whole bind at a place round F served after the call", d.From.Mock, d.From.Path, d.Line)
			}
			if !tc.after && (!whole || d.From.Mock != "mock-9" || d.From.Path != "url.query.client_id") {
				t.Fatalf("outcome %v from %s %s at word %d: %s; want a Value of mock-9's client_id whole", d.Outcome, d.From.Mock, d.From.Path, d.At.Word, d.Line)
			}
		})
	}
}

// TestAClientIDOfACallThatLandedElsewhereReadsEveryTestsPlace: a call round
// F landed in another test (Mode Elsewhere) reads every earlier place of the
// run as its scope, so another test's request that sent the client_id whole
// in both runs is the value's source; the timer rule then refuses it (it
// teaches clock reads only), naming that place.
func TestAClientIDOfACallThatLandedElsewhereReadsEveryTestsPlace(t *testing.T) {
	rec, live := sealedPair(t, 1794446576090003375, "unpaired")
	in := staffBlockCall(t, rec, live, rec, live)
	e := iSent("mock-9", in.At.Add(-time.Millisecond), "url.query.client_id", String, rec, live)
	e.Own, e.Run = false, true
	in.Earlier = []Earlier{e}
	in.Mode, in.Landed = ModeElsewhere, Landed{Where: LandedOther, Test: "test-5"}
	d := oWord(t, Decide(in), rec, live)
	if d.Outcome != OutcomeRefused || d.Refusal == nil || d.Refusal.Kind != RefusalLandedElsewhere || !strings.Contains(d.Line, "a value an earlier place sent (mock-9 url.query.client_id)") {
		t.Fatalf("outcome %v: %s; want the timer rule's refusal of a value mock-9's client_id sent", d.Outcome, d.Line)
	}
}
