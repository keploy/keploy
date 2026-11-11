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
// whole, with its step, as before.
func TestFromAtReadsTheTextItsAddressOpens(t *testing.T) {
	b64 := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
	tok := func(id string) string { return claimToken(t, map[string]any{"cid": id}, "sig") }
	dc := &decider{in: Input{PlaceOf: func(Leaf, bool) (string, bool) { return "header.X-Proxy|b64", true }}}
	for _, tc := range []struct {
		name      string
		rec, live string
		want      string
	}{
		{"a claim of the opened text", b64("Bearer " + tok("a1")), b64("Bearer " + tok("b2")), "header.X-Proxy|b64|jwt.cid#0"},
		{"a word of the opened text", b64("u-a1"), b64("u-b2"), "header.X-Proxy|b64#1"},
		{"round F's text is not opened: no claim", b64("Bearer " + tok("a1")), "Bearer " + tok("b2"), "header.X-Proxy|b64"},
		{"the step does not open the leaf: whole", "u-a1", "u-b2", "header.X-Proxy|b64"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			at := dc.fromAt(evPlace{leaf: Leaf{Path: "header.X-Proxy", Type: String, Value: tc.rec}, live: tc.live}, "a1", "b2")
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
		if got := claimName(variables.Address{Steps: tc.steps, Word: 0}); got != tc.want {
			t.Fatalf("claimName(%v) = %q; want %q", tc.steps, got, tc.want)
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
		{"a place that names a word", "header.Authorization#1", "6af47b1e0c22d4e8a91b03f6", ""},
		{"a place that names a step", "header.Authorization|b64", "6af47b1e0c22d4e8a91b03f6", ""},
		{"round F's claim holds another value", "header.Authorization", "6af47b1e0c22d4e8a91b03f7", ""},
	} {
		if got := claimAt(tc.at, rec, live, "6af47add8a9351dfdb7c457a", tc.f); got != tc.want {
			t.Errorf("%s: claimAt(%q) = %q; want %q", tc.name, tc.at, got, tc.want)
		}
	}
	// An id of several words, the claim whole: no word.
	rec = "Bearer " + claimToken(t, map[string]any{"sub": "a20b6921-24c6-4a03-bcf9-b409842a88d0"}, "recorded")
	live = "Bearer " + claimToken(t, map[string]any{"sub": "0c9e1f7a-5b2d-4e8c-9a61-3d7f0b2c4e19"}, "this-run")
	if got := claimAt("header.Authorization", rec, live, "a20b6921-24c6-4a03-bcf9-b409842a88d0", "0c9e1f7a-5b2d-4e8c-9a61-3d7f0b2c4e19"); got != "header.Authorization|jwt.sub" {
		t.Errorf("claimAt of a UUID claim = %q; want header.Authorization|jwt.sub", got)
	}
}

// TestBindAtIsTheWholePlaceOrItsClaimsWord: a place that held the value
// whole binds at its variable place, with the value F saw there; one that
// held it inside a JWT claim (Place.Claim), at the claim's word, with that
// word of what F saw; any other, or one whose claim F's text does not open,
// is no place to bind at.
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
		{"inside a claim", Place{Var: "header.Authorization", Live: tok, Claim: claim}, claim, "6af47b1e0c22d4e8a91b03f6", true},
		{"inside a claim, no variable place", Place{Live: tok, Claim: claim}, "", "", false},
		{"inside a longer value, no claim", Place{Var: "header.Authorization", Live: tok}, "", "", false},
		{"a claim that is no place", Place{Var: "header.Authorization", Live: tok, Claim: "nowhere"}, "", "", false},
		{"a claim F's text does not open", Place{Var: "header.Authorization", Live: "Bearer opaque", Claim: claim}, "", "", false},
	} {
		at, live, ok := tc.p.BindAt()
		if at != tc.at || live != tc.live || ok != tc.ok {
			t.Errorf("%s: BindAt = %q, %q, %v; want %q, %q, %v", tc.name, at, live, ok, tc.at, tc.live, tc.ok)
		}
	}
}
