package learn

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/variables"
)

// claimToken is a signed-looking JWT whose payload holds claims, as an
// application mints one: the header, the payload and a signature, each
// base64url with no padding, sig standing for the signature's bytes.
func claimToken(t *testing.T, claims map[string]any, sig string) string {
	t.Helper()
	enc := base64.RawURLEncoding
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	return enc.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`)) + "." + enc.EncodeToString(payload) + "." + enc.EncodeToString([]byte(sig))
}

// heartbeatCase is the request a client test sends as a cluster's proxy
// does: its login token, which the application minted with the cluster's id
// in its cluster_id claim, as the Authorization header, at at.
func heartbeatCase(token string, at time.Time) *models.HTTPReq {
	return &models.HTTPReq{Method: "POST", URL: "http://localhost:8083/cluster/status", Timestamp: at,
		Header: map[string]string{"Content-Type": "application/json", "Host": "localhost:8083", "Authorization": "Bearer " + token},
		Body:   `{"cluster_name":"e2e-old-proxy","status":"active"}`}
}

// TestAnIdAClientTestCarriesInAJWTClaimIsBoundAtTheClaim: a client test
// logs in as a cluster's proxy and sends the token the application minted
// (its cluster_id claim the cluster's id, made anew each run; cid the
// company's) in the Authorization header of its next request; the
// application reads the claim and finds the cluster by it (api-server's
// TestReplayVerdictReachesAnAppAdminWithoutClusterView). The id is a Value
// bound at that request, at the claim
// (header.Authorization|jwt.cluster_id#0): a replay reads the claim of the
// live token there, as it reads a word of any place, so the find binds this
// run's id. Bound at the header whole, no replay finds the id there, and the
// adapter refuses it as a value held in another form.
func TestAnIdAClientTestCarriesInAJWTClaimIsBoundAtTheClaim(t *testing.T) {
	const rID, fID = "6af47add8a9351dfdb7c457a", "6af47b1e0c22d4e8a91b03f6"
	rReq := time.Date(2026, 11, 11, 14, 24, 31, 630_000_000, time.UTC)
	fReq := rReq.Add(67 * time.Second)
	rCall, fCall := rReq.Add(2*time.Millisecond), fReq.Add(2*time.Millisecond)
	token := func(id string, iat int64, sig string) string {
		return claimToken(t, map[string]any{"cid": "e2e-free-company", "cluster_id": id, "role": "ADMIN", "iat": iat, "exp": iat + 3600}, sig)
	}
	rTok, fTok := token(rID, rReq.Unix(), "recorded-signature-bytes-0001"), token(fID, fReq.Unix(), "round-f-signature-bytes-00002")
	placed := clientPlaced(t, "post-cluster-status-3", heartbeatCase(rTok, rReq), heartbeatCase(fTok, fReq), fReq)
	const p = "body.cmd.filter._id"
	in := iCall(p, String, rID, fID, rCall, fCall, nil, placed, nil)
	in.Drifts = oClientDrifts()
	d := oWord(t, Decide(in), rID, fID)
	if d.Outcome != OutcomeValue || d.From.Mock != "test case post-cluster-status-3" || d.From.Path != "header.Authorization" || !d.From.Client {
		t.Fatalf("the cluster id the token's cluster_id claim carries: %s (client test's request: %v); want a Value of test case post-cluster-status-3's header.Authorization, a client test's", oDescribe([]Decision{d}), d.From.Client)
	}
	if got, want := d.FromAt.String(), "header.Authorization|jwt.cluster_id#0"; got != want {
		t.Fatalf("the cluster id is bound at %q; want %q, the claim that holds it", got, want)
	}
	if want := "Places: test case post-cluster-status-3 header.Authorization (its JWT's cluster_id claim)"; !strings.Contains(d.Line, want) {
		t.Errorf("line %q; want the claim named in its places (%q)", d.Line, want)
	}
	if want := "bound to test case post-cluster-status-3 header.Authorization, its JWT's cluster_id claim"; !strings.Contains(d.Line, want) {
		t.Fatalf("the learned line %q does not name the claim; want it to say %q", d.Line, want)
	}
	if v, ok := d.FromAt.WordOf("Bearer " + rTok); !ok || v != rID {
		t.Fatalf("the bind place reads %q (%v) in the recorded header; want the recorded id %q", v, ok, rID)
	}
	if v, ok := d.FromAt.WordOf("Bearer " + fTok); !ok || v != fID {
		t.Fatalf("the bind place reads %q (%v) in round F's header; want round F's id %q", v, ok, fID)
	}
}

// TestAClaimIsTheBindPlaceOnlyWhereItHoldsTheValueInBothRuns: the claim is
// the bind place only where it holds the recorded value and round F's at one
// word; else the leaf is named whole, as before, and the adapter refuses a
// place that holds the value only in another form. A leaf whose own words
// hold the value is bound at that word, a claim beside it or not; of two
// claims that hold it, the first by name.
func TestAClaimIsTheBindPlaceOnlyWhereItHoldsTheValueInBothRuns(t *testing.T) {
	const rID, fID = "6af47add8a9351dfdb7c457a", "6af47b1e0c22d4e8a91b03f6"
	rReq := time.Date(2026, 11, 11, 14, 24, 31, 630_000_000, time.UTC)
	fReq := rReq.Add(67 * time.Second)
	rCall, fCall := rReq.Add(2*time.Millisecond), fReq.Add(2*time.Millisecond)
	const p = "body.cmd.filter._id"
	decide := func(rHeader, fHeader string) Decision {
		t.Helper()
		rec, live := heartbeatCase("", rReq), heartbeatCase("", fReq)
		rec.Header["Authorization"], live.Header["Authorization"] = rHeader, fHeader
		in := iCall(p, String, rID, fID, rCall, fCall, nil, clientPlaced(t, "post-cluster-status-3", rec, live, fReq), nil)
		in.Drifts = oClientDrifts()
		return oWord(t, Decide(in), rID, fID)
	}
	tok := func(claims map[string]any, sig string) string { return claimToken(t, claims, sig) }
	for _, tc := range []struct {
		name             string
		rHeader, fHeader string
		want, claim      string
	}{
		{"the claim, a word of its text", "Bearer " + tok(map[string]any{"sub": "cluster:" + rID}, "sig-r"), "Bearer " + tok(map[string]any{"sub": "cluster:" + fID}, "sig-f"),
			"header.Authorization|jwt.sub#1", "sub"},
		{"of two claims, the first by name", "Bearer " + tok(map[string]any{"cid": rID, "aud": rID}, "sig-r"), "Bearer " + tok(map[string]any{"cid": fID, "aud": fID}, "sig-f"),
			"header.Authorization|jwt.aud#0", "aud"},
		{"a plain word of the leaf, before a claim", rID + " " + tok(map[string]any{"cid": rID}, "sig-r"), fID + " " + tok(map[string]any{"cid": fID}, "sig-f"),
			"header.Authorization#0", ""},
		{"round F's claim at another word: the leaf whole", "Bearer " + tok(map[string]any{"sub": "cluster:" + rID}, "sig-r"), "Bearer " + tok(map[string]any{"sub": "cluster:proxy:" + fID}, "sig-f"),
			"header.Authorization", ""},
		{"round F's claim under another name: the leaf whole", "Bearer " + tok(map[string]any{"cid": rID, "n": 1}, "sig-r"), "Bearer " + tok(map[string]any{"cluster": fID, "n": 2}, "sig-f"),
			"header.Authorization", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := decide(tc.rHeader, tc.fHeader)
			if d.Outcome != OutcomeValue || d.From.Mock != "test case post-cluster-status-3" || d.From.Path != "header.Authorization" {
				t.Fatalf("%s; want a Value of test case post-cluster-status-3's header.Authorization", oDescribe([]Decision{d}))
			}
			if got := d.FromAt.String(); got != tc.want {
				t.Fatalf("bound at %q; want %q", got, tc.want)
			}
			// The line names the claim the place reads, and no claim where
			// it reads none.
			if named := strings.Contains(d.Line, "JWT's"); tc.claim == "" && named || tc.claim != "" && !strings.Contains(d.Line, "header.Authorization, its JWT's "+tc.claim+" claim") {
				t.Fatalf("the learned line %q; want it to name the claim %q (none: no claim)", d.Line, tc.claim)
			}
		})
	}
}

// TestFromAtReadsTheTextItsAddressOpens: where an adapter names a leaf's
// place with a decode step (a header a client sends base64, as its PlaceOf
// says), the word or the claim that holds the value is read in the text
// that step opens, and the claim's step follows it: the bind place reads
// the value there in both runs. A leaf the step does not open is named
// whole, with its step, as before. A call's request (no client test's) is
// never addressed at a claim: its leaf is named whole, as before, since its
// replay puts the recorded values back and cannot write a claim.
func TestFromAtReadsTheTextItsAddressOpens(t *testing.T) {
	b64 := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
	tok := func(id string) string { return claimToken(t, map[string]any{"cid": id}, "sig") }
	dc := &decider{in: Input{PlaceOf: func(Leaf, bool) (string, bool) { return "header.X-Proxy|b64", true },
		Earlier: []Earlier{{Mock: "test case c"}, {Mock: "mock-0"}}}}
	dc.in.Earlier[0].incoming = true
	for _, tc := range []struct {
		name, mock string
		rec, live  string
		want       string
	}{
		{"a claim of the opened text", "test case c", b64("Bearer " + tok("a1")), b64("Bearer " + tok("b2")), "header.X-Proxy|b64|jwt.cid#0"},
		{"a claim of a call's request: whole", "mock-0", b64("Bearer " + tok("a1")), b64("Bearer " + tok("b2")), "header.X-Proxy|b64"},
		{"a word of the opened text", "test case c", b64("u-a1"), b64("u-b2"), "header.X-Proxy|b64#1"},
		{"a word of a call's request", "mock-0", b64("u-a1"), b64("u-b2"), "header.X-Proxy|b64#1"},
		{"round F's text is not opened: no claim", "test case c", b64("Bearer " + tok("a1")), "Bearer " + tok("b2"), "header.X-Proxy|b64"},
		{"the step does not open the leaf: whole", "test case c", "u-a1", "u-b2", "header.X-Proxy|b64"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			at := dc.fromAt(evPlace{mock: tc.mock, leaf: Leaf{Path: "header.X-Proxy", Type: String, Value: tc.rec}, live: tc.live}, "a1", "b2")
			if got := at.String(); got != tc.want {
				t.Fatalf("fromAt = %q; want %q", got, tc.want)
			}
			if at.Word < 0 {
				return
			}
			if v, ok := at.WordOf(tc.rec); !ok || v != "a1" {
				t.Fatalf("%q reads %q (%v) in the recorded leaf; want a1", tc.want, v, ok)
			}
			if v, ok := at.WordOf(tc.live); !ok || v != "b2" {
				t.Fatalf("%q reads %q (%v) in round F's leaf; want b2", tc.want, v, ok)
			}
		})
	}
}

// TestClaimNameIsTheClaimABindPlaceReads: a learned line names the JWT claim
// its bind place reads; a place that reads none (a plain place, a decode
// step only) names none.
func TestClaimNameIsTheClaimABindPlaceReads(t *testing.T) {
	for _, tc := range []struct {
		steps []variables.Step
		want  string
	}{
		{nil, ""},
		{[]variables.Step{variables.StepB64}, ""},
		{[]variables.Step{variables.JWTClaim("cluster_id")}, "cluster_id"},
		{[]variables.Step{variables.StepB64, variables.JWTClaim("cid")}, "cid"},
		// A token inside a claim of a token: the inner claim, the last read.
		{[]variables.Step{variables.JWTClaim("outer"), variables.JWTClaim("inner")}, "inner"},
	} {
		if got := ClaimName(variables.Address{Steps: tc.steps, Word: 0}); got != tc.want {
			t.Fatalf("ClaimName(%v) = %q; want %q", tc.steps, got, tc.want)
		}
	}
}

// TestClaimWordReadsTheTwoRunsOfOneClaim: claimWord names a claim only where
// both runs are a JWT and the claim holds the recorded value and round F's
// at one word; a word round F's claim does not hold is none, an empty one
// too.
func TestClaimWordReadsTheTwoRunsOfOneClaim(t *testing.T) {
	tok := func(claims map[string]any) string { return claimToken(t, claims, "sig") }
	for _, tc := range []struct {
		name      string
		rec, live string
		v, f      string
		step      variables.Step
		word      int
		ok        bool
	}{
		{"one claim, whole", tok(map[string]any{"cid": "a1"}), tok(map[string]any{"cid": "b2"}), "a1", "b2", variables.JWTClaim("cid"), 0, true},
		{"one claim, its second word", tok(map[string]any{"sub": "u-a1"}), tok(map[string]any{"sub": "u-b2"}), "a1", "b2", variables.JWTClaim("sub"), 1, true},
		{"the recorded run is no JWT", "a1", tok(map[string]any{"cid": "b2"}), "a1", "b2", "", 0, false},
		{"round F's run is no JWT", tok(map[string]any{"cid": "a1"}), "b2", "a1", "b2", "", 0, false},
		{"no claim holds the recorded value", tok(map[string]any{"cid": "a9"}), tok(map[string]any{"cid": "b2"}), "a1", "b2", "", 0, false},
		{"round F's claim holds another value", tok(map[string]any{"cid": "a1"}), tok(map[string]any{"cid": "b9"}), "a1", "b2", "", 0, false},
		{"round F's claim holds it at another word", tok(map[string]any{"sub": "u-a1"}), tok(map[string]any{"sub": "b2-u"}), "a1", "b2", "", 0, false},
		{"round F's claim has no such word, an empty value", tok(map[string]any{"sub": "u-a1"}), tok(map[string]any{"sub": "u"}), "a1", "", "", 0, false},
		{"one claim whole, an id of several words", tok(map[string]any{"sub": "a20b6921-24c6-4a03"}), tok(map[string]any{"sub": "b9c1d2e3-77aa-4b10"}), "a20b6921-24c6-4a03", "b9c1d2e3-77aa-4b10", variables.JWTClaim("sub"), -1, true},
		{"a claim at a word before another whole", tok(map[string]any{"a": "x-1a2b", "b": "1a2b"}), tok(map[string]any{"a": "x-9z8y", "b": "9z8y"}), "1a2b", "9z8y", variables.JWTClaim("a"), 1, true},
		{"round F's claim holds another value whole", tok(map[string]any{"sub": "a20b6921-24c6-4a03"}), tok(map[string]any{"sub": "b9c1d2e3-77aa-4b10"}), "a20b6921-24c6-4a03", "b9c1d2e3-77aa-4b11", "", 0, false},
		{"the recorded claim holds another value whole", tok(map[string]any{"sub": "a20b6921-24c6-4a04"}), tok(map[string]any{"sub": "b9c1d2e3-77aa-4b10"}), "a20b6921-24c6-4a03", "b9c1d2e3-77aa-4b10", "", 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, n, ok := claimWord(tc.rec, tc.live, tc.v, tc.f)
			if ok != tc.ok || st != tc.step || n != tc.word {
				t.Fatalf("claimWord = %q, %d, %v; want %q, %d, %v", st, n, ok, tc.step, tc.word, tc.ok)
			}
		})
	}
}

// TestClaimAtAddressesTheClaimAtTheLeafsVariablePlace: Classify's place of a
// value held inside a leaf (Place.Claim) is the leaf's variable place at the
// word of the claim that holds the two runs' values; none where the leaf
// has no variable place, its place already names a word or a step, or no
// claim holds them so.
func TestClaimAtAddressesTheClaimAtTheLeafsVariablePlace(t *testing.T) {
	rec := "Bearer " + claimToken(t, map[string]any{"cid": "e2e-free-company", "cluster_id": "6af47add8a9351dfdb7c457a"}, "recorded")
	live := "Bearer " + claimToken(t, map[string]any{"cid": "e2e-free-company", "cluster_id": "6af47b1e0c22d4e8a91b03f6"}, "this-run")
	for _, tc := range []struct {
		name, at, f, want string
	}{
		{"the header's claim", "header.Authorization", "6af47b1e0c22d4e8a91b03f6", "header.Authorization|jwt.cluster_id#0"},
		{"no variable place", "", "6af47b1e0c22d4e8a91b03f6", ""},
		{"a place keploy cannot read", "header.", "6af47b1e0c22d4e8a91b03f6", ""},
		{"a place of no part keploy reads", "nowhere", "6af47b1e0c22d4e8a91b03f6", ""},
		{"a place that names a word", "header.Authorization#1", "6af47b1e0c22d4e8a91b03f6", ""},
		{"a place that names a step", "header.Authorization|b64", "6af47b1e0c22d4e8a91b03f6", ""},
		{"round F's claim holds another value", "header.Authorization", "6af47b1e0c22d4e8a91b03f7", ""},
	} {
		if got := claimAt(Place{Var: tc.at, Client: true}, rec, live, "6af47add8a9351dfdb7c457a", tc.f); got != tc.want {
			t.Errorf("%s: claimAt(%q) = %q; want %q", tc.name, tc.at, got, tc.want)
		}
	}
	// A call's request is no place to bind a claim at: its replay cannot
	// write one into the token it sends.
	if got := claimAt(Place{Var: "header.Authorization"}, rec, live, "6af47add8a9351dfdb7c457a", "6af47b1e0c22d4e8a91b03f6"); got != "" {
		t.Errorf("claimAt of a call's request = %q; want none", got)
	}
	// An id of several words, the claim whole: no word.
	rec = "Bearer " + claimToken(t, map[string]any{"sub": "a20b6921-24c6-4a03-bcf9-b409842a88d0"}, "recorded")
	live = "Bearer " + claimToken(t, map[string]any{"sub": "0c9e1f7a-5b2d-4e8c-9a61-3d7f0b2c4e19"}, "this-run")
	if got := claimAt(Place{Var: "header.Authorization", Client: true}, rec, live, "a20b6921-24c6-4a03-bcf9-b409842a88d0", "0c9e1f7a-5b2d-4e8c-9a61-3d7f0b2c4e19"); got != "header.Authorization|jwt.sub" {
		t.Errorf("claimAt of a UUID claim = %q; want header.Authorization|jwt.sub", got)
	}
}

// TestBindAtIsTheWholePlaceOrItsClaimsWord: a place that held the value
// whole binds at its variable place, with the value F saw there; a client
// test's request that held it inside a JWT claim (Place.Claim), at the
// claim's word, with that word of what F saw; any other (a call's claim, a
// claim F's text does not open) is no place to bind at.
func TestBindAtIsTheWholePlaceOrItsClaimsWord(t *testing.T) {
	tok := "Bearer " + claimToken(t, map[string]any{"cluster_id": "6af47b1e0c22d4e8a91b03f6"}, "this-run")
	claim := "header.Authorization|jwt.cluster_id#0"
	for _, tc := range []struct {
		name     string
		p        Place
		at, live string
		ok       bool
	}{
		{"whole", Place{Var: "body.name", Live: "app-2", Whole: true}, "body.name", "app-2", true},
		{"whole, no variable place", Place{Live: "app-2", Whole: true}, "", "", false},
		{"inside a claim", Place{Var: "header.Authorization", Live: tok, Claim: claim, Client: true}, claim, "6af47b1e0c22d4e8a91b03f6", true},
		{"inside a claim of a call's request", Place{Var: "header.Authorization", Live: tok, Claim: claim}, "", "", false},
		{"inside a claim, no variable place", Place{Live: tok, Claim: claim, Client: true}, "", "", false},
		{"inside a longer value, no claim", Place{Var: "header.Authorization", Live: tok, Client: true}, "", "", false},
		{"a claim that is no place", Place{Var: "header.Authorization", Live: tok, Claim: "nowhere", Client: true}, "", "", false},
		{"a claim F's text does not open", Place{Var: "header.Authorization", Live: "Bearer opaque", Claim: claim, Client: true}, "", "", false},
	} {
		at, live, ok := tc.p.BindAt()
		if at != tc.at || live != tc.live || ok != tc.ok {
			t.Errorf("%s: BindAt = %q, %q, %v; want %q, %q, %v", tc.name, at, live, ok, tc.at, tc.live, tc.ok)
		}
	}
}

// TestClaimHoldingNamesTheClaimThatHoldsAValue: the claim of a token (bare or
// "Bearer <token>") whose value holds v whole or at a word, the first such
// by name; none where the text is no JWT, v is empty, or no claim holds v
// (a part of a word is no word).
func TestClaimHoldingNamesTheClaimThatHoldsAValue(t *testing.T) {
	tok := claimToken(t, map[string]any{"cid": "e2e-free-company", "cluster_id": "6af47add8a9351dfdb7c457a", "iat": 1793561053, "sub": "proxy:a20b6921-24c6-4a03"}, "sig")
	for _, tc := range []struct{ name, text, v, want string }{
		{"a claim whole, in a header", "Bearer " + tok, "6af47add8a9351dfdb7c457a", "cluster_id"},
		{"a bare token", tok, "6af47add8a9351dfdb7c457a", "cluster_id"},
		{"a number claim", tok, "1793561053", "iat"},
		{"a word of a claim", tok, "proxy", "sub"},
		{"several words of a claim", tok, "a20b6921-24c6-4a03", ""},
		{"a claim of several words whole", tok, "proxy:a20b6921-24c6-4a03", "sub"},
		{"the first claim by name", claimToken(t, map[string]any{"b": "x-1a2b", "a": "1a2b"}, "sig"), "1a2b", "a"},
		{"a part of a word", tok, "6af47add", ""},
		{"no claim holds it", tok, "6af47b1e0c22d4e8a91b03f6", ""},
		{"an empty value", tok, "", ""},
		{"an empty value, an empty claim", claimToken(t, map[string]any{"nonce": ""}, "sig"), "", ""},
		{"a claim that is no text, before the one that holds it", claimToken(t, map[string]any{"a": map[string]any{"x": 1}, "b": "1a2b"}, "sig"), "1a2b", "b"},
		{"no JWT", "Bearer opaque-6af47add8a9351dfdb7c457a", "6af47add8a9351dfdb7c457a", ""},
	} {
		if got := ClaimHolding(tc.text, tc.v); got != tc.want {
			t.Errorf("%s: ClaimHolding(%q) = %q; want %q", tc.name, tc.v, got, tc.want)
		}
	}
}

// TestDecideBindsAClaimOnlyInAClientTestsRequest: an earlier call of the
// test sent the token (an auth check, a forwarded token), then the call
// that carries the claim's id. A replay puts a call's recorded values back
// at its places and cannot write a claim into its signed token, so the
// call's claim is never the bind place: with no client test, Decide names
// the call's leaf whole (the adapter refuses it as held in another form);
// with a client test that sent the token, the id binds at the test case's
// claim, and the learned line annotates that place alone with the claim,
// not the call's that held the token too.
func TestDecideBindsAClaimOnlyInAClientTestsRequest(t *testing.T) {
	const rID, fID = "6af47add8a9351dfdb7c457a", "6af47b1e0c22d4e8a91b03f6"
	rReq := time.Date(2026, 11, 11, 14, 24, 31, 630_000_000, time.UTC)
	fReq := rReq.Add(67 * time.Second)
	rCall, fCall := rReq.Add(2*time.Millisecond), fReq.Add(2*time.Millisecond)
	token := func(id string, iat int64, sig string) string {
		return claimToken(t, map[string]any{"cid": "e2e-free-company", "cluster_id": id, "role": "ADMIN", "iat": iat, "exp": iat + 3600}, sig)
	}
	rTok, fTok := token(rID, rReq.Unix(), "recorded-signature-bytes-0001"), token(fID, fReq.Unix(), "round-f-signature-bytes-00002")
	sent := iSent("mock-0", rReq.Add(time.Millisecond), "header.Authorization", String, "Bearer "+rTok, "Bearer "+fTok)
	const p = "body.cmd.filter._id"
	for _, client := range []bool{false, true} {
		name := "no client test"
		if client {
			name = "a client test that sent the token"
		}
		t.Run(name, func(t *testing.T) {
			earlier := []Earlier{sent}
			if client {
				earlier = append(clientPlaced(t, "post-cluster-status-3", heartbeatCase(rTok, rReq), heartbeatCase(fTok, fReq), fReq), sent)
			}
			in := iCall(p, String, rID, fID, rCall, fCall, nil, earlier, nil)
			if client {
				in.Drifts = oClientDrifts()
			}
			d := oWord(t, Decide(in), rID, fID)
			if !client {
				if ClaimName(d.FromAt) != "" || d.From.Client {
					t.Fatalf("Decide binds at %s's %s (client %v); want no claim of a call's token", d.From.Mock, d.FromAt.String(), d.From.Client)
				}
				return
			}
			if d.Outcome != OutcomeValue || !d.From.Client || d.From.Mock != "test case post-cluster-status-3" || d.FromAt.String() != "header.Authorization|jwt.cluster_id#0" {
				t.Fatalf("outcome %v from %s (client %v) at %q; want a Value bound at the test case's header.Authorization|jwt.cluster_id#0", d.Outcome, d.From.Mock, d.From.Client, d.FromAt.String())
			}
			// Two places held the value; only the bind place reads the claim.
			_, places, ok := strings.Cut(d.Line, "Places: ")
			if !ok || strings.Count(places, "(its JWT's cluster_id claim)") != 1 || !strings.Contains(places, "test case post-cluster-status-3 header.Authorization (its JWT's cluster_id claim)") || !strings.Contains(places, "mock-0") {
				t.Fatalf("learned %q; want both places named, the test case's alone with its claim", d.Line)
			}
		})
	}
}

// TestClaimRefusedSaysWhyAClaimIsNoPlace: a refusal of a value held inside
// a JWT claim names the claim and the exact reason: a call's request, a
// client test's whose token in this run is no JWT, or one whose claims hold
// this run's value at no place where the recording's held it; none where
// the recording holds the value in no claim.
func TestClaimRefusedSaysWhyAClaimIsNoPlace(t *testing.T) {
	rec := "Bearer " + claimToken(t, map[string]any{"cluster_id": "6af47add8a9351dfdb7c457a"}, "recorded")
	live := "Bearer " + claimToken(t, map[string]any{"cluster_id": "c 6af47b1e0c22d4e8a91b03f6"}, "this-run")
	const head = "mock-0 sent this value in the cluster_id claim of the JWT at header.Authorization, "
	for _, tc := range []struct {
		name, rec, live, v string
		client             bool
		want               string
	}{
		{"a call's request", rec, live, "6af47add8a9351dfdb7c457a", false, head + "a call's request: keploy binds a variable at a JWT claim only in a client test's request, which a replay reads, never in a token a call sends, which a replay cannot write a claim into"},
		{"no JWT in this run", rec, "Bearer opaque", "6af47add8a9351dfdb7c457a", true, head + "and this run's text there holds no JWT: no variable can bind it there"},
		{"no claim holds this run's value there", rec, live, "6af47add8a9351dfdb7c457a", true, head + "and no claim of this run's token holds this run's value where the recording's claim held it, at that word or whole: no variable can bind it there"},
		{"the recording holds it in no claim", rec, live, "6af47add8a9351dfdb7c457b", true, ""},
		{"the recording is no JWT", "Bearer opaque", live, "opaque", false, ""},
	} {
		if got := ClaimRefused("mock-0", "header.Authorization", tc.rec, tc.live, tc.v, tc.client); got != tc.want {
			t.Errorf("%s: ClaimRefused = %q; want %q", tc.name, got, tc.want)
		}
	}
}

// TestACallListedBeforeTheClientCaseNeverHidesItsClaim: an earlier place
// that sent the same token and is no client test's request is listed before
// the client case: a run's call (the set's start-up, or an earlier test's)
// or another test's call 30s before the case, or the test's own call just
// before or after it. The call's claim is no place (a replay cannot write a
// claim into a call's token), and the call, which sent the id only inside a
// longer value, never hides the test case's claim, whenever it was sent:
// both Decide (the Mongo adapter's learn step) and ClassifyArrived (the HTTP
// adapter's) bind the id at test case post-cluster-status-3's
// header.Authorization|jwt.cluster_id#0.
func TestACallListedBeforeTheClientCaseNeverHidesItsClaim(t *testing.T) {
	const rID, fID = "6af47add8a9351dfdb7c457a", "6af47b1e0c22d4e8a91b03f6"
	rReq := time.Date(2026, 11, 11, 14, 24, 31, 630_000_000, time.UTC)
	fReq := rReq.Add(67 * time.Second)
	rCall, fCall := rReq.Add(2*time.Millisecond), fReq.Add(2*time.Millisecond)
	token := func(id string, iat int64, sig string) string {
		return claimToken(t, map[string]any{"cid": "e2e-free-company", "cluster_id": id, "role": "ADMIN", "iat": iat, "exp": iat + 3600}, sig)
	}
	rTok, fTok := token(rID, rReq.Unix(), "recorded-signature-bytes-0001"), token(fID, fReq.Unix(), "round-f-signature-bytes-00002")
	const p, claim = "body.cmd.filter._id", "header.Authorization|jwt.cluster_id#0"
	for _, tc := range []struct {
		name              string
		own, run, startUp bool
		before            time.Duration
	}{
		{"a run call 30s before the case", false, true, false, -30 * time.Second},
		{"a start-up call 30s before the case", false, true, true, -30 * time.Second},
		{"another test's call 30s before the case", false, false, false, -30 * time.Second},
		{"an own call 1ms after the case", true, false, false, time.Millisecond},
		{"an own call 1ms before the case", true, false, false, -time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			at := rReq.Add(tc.before)
			sent := Earlier{Mock: "mock-0", At: at, Own: tc.own, Run: tc.run, StartUp: tc.startUp, Payload: oPayload("mock-0", at), Seen: []Seen{{Side: Request,
				Leaf: Leaf{Path: "header.Authorization", Noise: "header.Authorization", Type: String, Value: "Bearer " + rTok}, Live: "Bearer " + fTok, Place: "header.Authorization"}}}
			earlier := append([]Earlier{sent}, clientPlaced(t, "post-cluster-status-3", heartbeatCase(rTok, rReq), heartbeatCase(fTok, fReq), fReq)...)
			in := iCall(p, String, rID, fID, rCall, fCall, nil, earlier, nil)
			in.Drifts = oClientDrifts()
			d := oWord(t, Decide(in), rID, fID)
			if d.Outcome != OutcomeValue || !d.From.Client || d.From.Mock != "test case post-cluster-status-3" || d.FromAt.String() != claim {
				t.Errorf("Decide: outcome %v from %s (client %v) at %q; want a Value bound at the test case's %s", d.Outcome, d.From.Mock, d.From.Client, d.FromAt.String(), claim)
			}
			sh := func(v string) Shape {
				return Shape{Leaves: map[string]Leaf{p: {Path: p, Noise: p, Type: String, Value: v}}}
			}
			ln := ClassifyArrived(oClientDrifts(), []string{p}, sh(fID), sh(rID), rCall, fCall, earlier, nil, nil, nil, nil)[0]
			if ln.Kind != KindVariable || ln.From.Mock != "test case post-cluster-status-3" || !ln.From.Client || ln.From.Claim != claim {
				t.Errorf("ClassifyArrived: %s (%s) from %s (client %v, claim %q); want a variable of the test case bound at its %s", ln.Kind, ln.Why, ln.From.Mock, ln.From.Client, ln.From.Claim, claim)
			}
		})
	}
}

// TestACallsClaimNeverHidesAWholePlace: a call's request whose header the
// run's variable is used at (the run's token, sent by every call) carries,
// in a claim of that token, the id the find carries. A place where the run's
// variable is used is no place of the id (placeRank), and its claim is none
// either (claimAt: no client test's request): the id is found where it was
// sent or given whole, a client test's request body or a call's answer, and
// never refused as a claim of the call's token.
func TestACallsClaimNeverHidesAWholePlace(t *testing.T) {
	const rID, fID = "6af47add8a9351dfdb7c457a", "6af47b1e0c22d4e8a91b03f6"
	rReq := time.Date(2026, 11, 11, 14, 24, 31, 630_000_000, time.UTC)
	fReq := rReq.Add(67 * time.Second)
	rCall, fCall := rReq.Add(2*time.Millisecond), fReq.Add(2*time.Millisecond)
	token := func(id string, sig string) string {
		return "Bearer " + claimToken(t, map[string]any{"cid": "e2e-free-company", "cluster_id": id}, sig)
	}
	sent := iSent("mock-0", rReq.Add(time.Millisecond), "header.Authorization", String, token(rID, "recorded-signature"), token(fID, "this-run-signature"))
	sent.Seen[0].Place, sent.Seen[0].Name = "header.Authorization", models.RunVariablePrefix+"token"
	const p = "body.cmd.filter._id"
	sh := func(v string) Shape {
		return Shape{Leaves: map[string]Leaf{p: {Path: p, Noise: p, Type: String, Value: v}}}
	}
	clientCase := func(id string, at time.Time) *models.HTTPReq {
		return &models.HTTPReq{Method: "POST", URL: "http://localhost:8083/cluster/status", Timestamp: at,
			Header: map[string]string{"Content-Type": "application/json", "Host": "localhost:8083"}, Body: `{"cluster_id":"` + id + `","status":"active"}`}
	}
	answered := Earlier{Mock: "mock-1", At: rReq.Add(time.Millisecond), Own: true, Payload: oPayload("mock-1", rReq.Add(time.Millisecond)), Seen: []Seen{{Side: Response,
		Leaf: Leaf{Path: "body.id", Noise: "body.id", Type: String, Value: rID}, Live: fID}}}
	for _, tc := range []struct {
		name       string
		earlier    func(t *testing.T) []Earlier
		dc         *DriftCache
		mock, path string
	}{
		{"a client test's request body", func(t *testing.T) []Earlier {
			return append([]Earlier{sent}, clientPlaced(t, "post-cluster-status-3", clientCase(rID, rReq), clientCase(fID, fReq), fReq)...)
		}, oClientDrifts(), "test case post-cluster-status-3", "body.cluster_id"},
		{"a call's answer", func(*testing.T) []Earlier { return []Earlier{sent, answered} }, oDrifts(), "mock-1", "body.id"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ln := ClassifyArrived(tc.dc, []string{p}, sh(fID), sh(rID), rCall, fCall, tc.earlier(t), nil, nil, nil, nil)[0]
			if ln.From.Mock != tc.mock || ln.From.Path != tc.path || !ln.From.Whole || ln.From.Claim != "" {
				t.Fatalf("%s (%s) from %s %s (whole %v, claim %q); want the id found whole at %s %s, not at a claim of mock-0's token", ln.Kind, ln.Why, ln.From.Mock, ln.From.Path, ln.From.Whole, ln.From.Claim, tc.mock, tc.path)
			}
		})
	}
}
