package learn

import (
	"encoding/base64"
	"encoding/json"
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
		t.Fatalf("the cluster id the token's cid claim carries: %s (client test's request: %v); want a Value of test case post-cluster-status-3's header.Authorization, a client test's", oDescribe([]Decision{d}), d.From.Client)
	}
	if got, want := d.FromAt.String(), "header.Authorization|jwt.cluster_id#0"; got != want {
		t.Fatalf("the cluster id is bound at %q; want %q, the claim that holds it", got, want)
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
		want             string
	}{
		{"the claim, a word of its text", "Bearer " + tok(map[string]any{"sub": "cluster:" + rID}, "sig-r"), "Bearer " + tok(map[string]any{"sub": "cluster:" + fID}, "sig-f"),
			"header.Authorization|jwt.sub#1"},
		{"of two claims, the first by name", "Bearer " + tok(map[string]any{"cid": rID, "aud": rID}, "sig-r"), "Bearer " + tok(map[string]any{"cid": fID, "aud": fID}, "sig-f"),
			"header.Authorization|jwt.aud#0"},
		{"a plain word of the leaf, before a claim", rID + " " + tok(map[string]any{"cid": rID}, "sig-r"), fID + " " + tok(map[string]any{"cid": fID}, "sig-f"),
			"header.Authorization#0"},
		{"round F's claim at another word: the leaf whole", "Bearer " + tok(map[string]any{"sub": "cluster:" + rID}, "sig-r"), "Bearer " + tok(map[string]any{"sub": "cluster:proxy:" + fID}, "sig-f"),
			"header.Authorization"},
		{"round F's claim under another name: the leaf whole", "Bearer " + tok(map[string]any{"cid": rID, "n": 1}, "sig-r"), "Bearer " + tok(map[string]any{"cluster": fID, "n": 2}, "sig-f"),
			"header.Authorization"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := decide(tc.rHeader, tc.fHeader)
			if d.Outcome != OutcomeValue || d.From.Mock != "test case post-cluster-status-3" || d.From.Path != "header.Authorization" {
				t.Fatalf("%s; want a Value of test case post-cluster-status-3's header.Authorization", oDescribe([]Decision{d}))
			}
			if got := d.FromAt.String(); got != tc.want {
				t.Fatalf("bound at %q; want %q", got, tc.want)
			}
		})
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
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, n, ok := claimWord(tc.rec, tc.live, tc.v, tc.f)
			if ok != tc.ok || st != tc.step || n != tc.word {
				t.Fatalf("claimWord = %q, %d, %v; want %q, %d, %v", st, n, ok, tc.step, tc.word, tc.ok)
			}
		})
	}
}
