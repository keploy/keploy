package http

import (
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// A response header that is a DIGEST of the body must inherit the body's noise
// verdict. When the body passes only BECAUSE a field was noised, the bytes still
// differ, so the server's ETag differs too — and asserting it byte-for-byte
// silently re-imposes the exact equality the noise deliberately waived.
//
// Values are the real ones from the node-jwt lane (enterprise pipeline 9012): a
// signin whose JWT `iat` landed either side of a UNIX second boundary between
// record and replay, so the token — and therefore Express's ETag over the body —
// changed. Both bodies are 227 bytes, which is the `e3` in each ETag.
func signinCase(expBody, actBody, expETag, actETag string) (*models.TestCase, *models.HTTPResp) {
	tc := &models.TestCase{
		Name: "post-api-auth-signin-2",
		HTTPResp: models.HTTPResp{
			StatusCode: 200,
			Header: map[string]string{
				"Content-Type": "application/json; charset=utf-8",
				"ETag":         expETag,
			},
			Body: expBody,
		},
	}
	actual := &models.HTTPResp{
		StatusCode: 200,
		Header: map[string]string{
			"Content-Type": "application/json; charset=utf-8",
			"ETag":         actETag,
		},
		Body: actBody,
	}
	return tc, actual
}

const (
	// Same payload, one second apart in the token's `iat`.
	signinBodyA    = `{"id":1,"username":"user","email":"user@keploy.io","accessToken":"eyJhbGciOiJIUzI1NiJ9.eyJpZCI6MSwiaWF0IjoxNzg5MzgyNjM2fQ.AAAA"}`
	signinBodyB    = `{"id":1,"username":"user","email":"user@keploy.io","accessToken":"eyJhbGciOiJIUzI1NiJ9.eyJpZCI6MSwiaWF0IjoxNzg5MzgyNjM3fQ.BBBB"}`
	etagForBodyA   = `W/"e3-rAVXWPLCh7N5cMZxSH8LrmsLGqI"`
	etagForBodyB   = `W/"e3-bDuVHhQeaquvS7cRl79wlVde4Ig"`
	accessTokNoise = "accessToken"
)

func bodyNoise() map[string]map[string][]string {
	return map[string]map[string][]string{"body": {accessTokNoise: {}}}
}

// THE DEFECT. The user noised accessToken, the body comparison passes, and the
// test still fails on a header they never configured and cannot reasonably know
// to configure.
func TestMatch_BodyDigestHeader_ForgivenWhenBodyPassedViaNoise(t *testing.T) {
	tc, actual := signinCase(signinBodyA, signinBodyB, etagForBodyA, etagForBodyB)
	pass, res := Match(tc, actual, bodyNoise(), false, false, zap.NewNop(), false, WithAutoHeaderNoise(true))
	if !pass {
		t.Fatalf("body passed via noise but the ETag over that same body still failed the test; "+
			"noise on a body field is being defeated by a header nobody configured.\nresult: %+v", res.HeadersResult)
	}
}

// The narrowing that keeps this from being a blanket ignore: if the body bytes
// are IDENTICAL, a differing digest means the server is minting a
// non-deterministic ETag. That is a real defect and must still fail — this is
// exactly what adding "etag" to the volatile-header list would have hidden.
func TestMatch_BodyDigestHeader_StillFailsWhenBodyIsIdentical(t *testing.T) {
	tc, actual := signinCase(signinBodyA, signinBodyA, etagForBodyA, etagForBodyB)
	if pass, _ := Match(tc, actual, bodyNoise(), false, false, zap.NewNop(), false, WithAutoHeaderNoise(true)); pass {
		t.Fatal("identical bodies with different ETags passed; a non-deterministic digest " +
			"is a real defect and must not be forgiven")
	}
}

// If the BODY fails, everything still fails. Asserting only on the overall
// verdict would pin nothing here -- a failing body already sets pass=false on its
// own, independently of anything the header block does -- so this asserts the
// ETag entry ITSELF was left failed.
func TestMatch_BodyDigestHeader_StillFailsWhenBodyFails(t *testing.T) {
	// `email` is NOT noised, so the body genuinely differs.
	brokenBody := `{"id":1,"username":"user","email":"someone-else@keploy.io","accessToken":"eyJhbGciOiJIUzI1NiJ9.eyJpZCI6MSwiaWF0IjoxNzg5MzgyNjM3fQ.BBBB"}`
	tc, actual := signinCase(signinBodyA, brokenBody, etagForBodyA, etagForBodyB)
	pass, res := Match(tc, actual, bodyNoise(), false, false, zap.NewNop(), false, WithAutoHeaderNoise(true))
	if pass {
		t.Fatal("a genuinely different body passed; the digest rule must not rescue a failing body")
	}
	if forgiven, found := etagVerdict(res); found && forgiven {
		t.Fatal("the ETag was forgiven even though the body FAILED; the rule must be " +
			"conditional on the body having passed")
	}
}

// etagVerdict reports whether an ETag entry exists in the header results and
// whether it was marked normal (i.e. forgiven).
func etagVerdict(res *models.Result) (forgiven, found bool) {
	for _, hr := range res.HeadersResult {
		name := hr.Expected.Key
		if name == "" {
			name = hr.Actual.Key
		}
		if strings.EqualFold(name, "etag") {
			return hr.Normal, true
		}
	}
	return false, false
}

// Only headers that are a digest OF THE BODY are covered. Content-Type relates
// to the body but is not computed from its bytes, so a change in it is a real
// change and must still fail even when the body passed via noise.
func TestMatch_BodyDigestHeader_DoesNotForgiveNonDigestHeaders(t *testing.T) {
	tc, actual := signinCase(signinBodyA, signinBodyB, etagForBodyA, etagForBodyA)
	actual.Header["Content-Type"] = "text/plain; charset=utf-8"
	if pass, _ := Match(tc, actual, bodyNoise(), false, false, zap.NewNop(), false, WithAutoHeaderNoise(true)); pass {
		t.Fatal("a Content-Type change was forgiven; only body DIGEST headers may inherit " +
			"the body's noise verdict")
	}
}

func TestIsBodyDigestHeader(t *testing.T) {
	for _, h := range []string{"etag", "content-md5", "digest", "content-digest", "repr-digest"} {
		if !isBodyDigestHeader(h) {
			t.Errorf("%q should be treated as a body digest", h)
		}
	}
	// Content-Length is handled by its own rule above; the rest are not digests.
	for _, h := range []string{"content-type", "content-encoding", "content-length", "date", "server", ""} {
		if isBodyDigestHeader(h) {
			t.Errorf("%q must not be treated as a body digest", h)
		}
	}
}

// ── The forgiveness must NOT extend to a body that was never compared ─────────
//
// `pass` starts true and stays true on every path that skips the body, so
// BodyResult[0].Normal alone does NOT mean "the body passed". Gating on it
// turned this rule into a blanket ignore: each case below has NO noise
// configured and a completely different body, and each must still fail. For a
// non-JSON response the digest is the ONLY remaining signal that the
// representation changed, so forgiving it there is strictly worse than not
// having the rule at all.

func TestMatch_BodyDigestHeader_NotForgivenWhenBodyComparisonWasSkipped(t *testing.T) {
	// Non-JSON on both sides and compareAll=false -> keploy never compares the body.
	tc, actual := signinCase(
		"<html><body>the recorded page</body></html>",
		"<html><body>something else entirely</body></html>",
		etagForBodyA, etagForBodyB)
	tc.HTTPResp.Header["Content-Type"] = "text/html; charset=utf-8"
	actual.Header["Content-Type"] = "text/html; charset=utf-8"

	// nil noise: nothing was noised, so there is no verdict for the ETag to inherit.
	if pass, _ := Match(tc, actual, nil, false, false, zap.NewNop(), false, WithAutoHeaderNoise(true)); pass {
		t.Fatal("a non-JSON body that was never compared had its ETag forgiven; the digest " +
			"was the only signal left that the representation changed")
	}
}

func TestMatch_BodyDigestHeader_NotForgivenWhenActualRegressedToNonJSON(t *testing.T) {
	// bodyType is computed from the ACTUAL body, so a JSON endpoint that regresses
	// to an HTML error page takes the skip path.
	tc, actual := signinCase(signinBodyA, "<html>500 Internal Server Error</html>", etagForBodyA, etagForBodyB)
	if pass, _ := Match(tc, actual, nil, false, false, zap.NewNop(), false, WithAutoHeaderNoise(true)); pass {
		t.Fatal("a JSON endpoint that regressed to an HTML error page passed; its ETag must " +
			"not be forgiven when no body comparison ran")
	}
}

func TestMatch_BodyDigestHeader_NotForgivenUnderWildcardBodyNoise(t *testing.T) {
	tc, actual := signinCase(signinBodyA, `{"totally":"different"}`, etagForBodyA, etagForBodyB)
	wildcard := map[string]map[string][]string{"body": {"*": {"*"}}}
	if pass, _ := Match(tc, actual, wildcard, false, false, zap.NewNop(), false, WithAutoHeaderNoise(true)); pass {
		t.Fatal("wildcard body noise skips the comparison entirely; the ETag must not be " +
			"forgiven on the strength of a comparison that never happened")
	}
}

func TestMatch_BodyDigestHeader_NotForgivenWhenBodyWasSkippedForSize(t *testing.T) {
	// The >1MB path clears the actual body, so the raw bytes trivially "differ".
	// bodyActuallyCompared is what rejects this (the cleared body makes bodyType
	// Plain, so no comparison runs); this pins the BEHAVIOUR rather than any one
	// clause, which is why an explicit !BodySkipped guard was dropped as dead.
	tc, actual := signinCase(signinBodyA, "", etagForBodyA, etagForBodyB)
	tc.HTTPResp.BodySkipped = true
	tc.HTTPResp.BodySize = int64(len(signinBodyA))
	if pass, _ := Match(tc, actual, bodyNoise(), false, false, zap.NewNop(), false, WithAutoHeaderNoise(true)); pass {
		t.Fatal("a >1MB body that was never compared had its ETag forgiven")
	}
}

// ── Presence and explicit user noise, the two guards the sibling block has ────

func TestMatch_BodyDigestHeader_PresenceStillAsserted(t *testing.T) {
	t.Run("server stops emitting it", func(t *testing.T) {
		tc, actual := signinCase(signinBodyA, signinBodyB, etagForBodyA, "")
		delete(actual.Header, "ETag")
		if pass, _ := Match(tc, actual, bodyNoise(), false, false, zap.NewNop(), false, WithAutoHeaderNoise(true)); pass {
			t.Fatal("the server stopped emitting ETag entirely and it was forgiven; that is a " +
				"real change, not a value difference")
		}
	})
	t.Run("server starts emitting one it never did", func(t *testing.T) {
		tc, actual := signinCase(signinBodyA, signinBodyB, etagForBodyA, etagForBodyB)
		delete(tc.HTTPResp.Header, "ETag")
		actual.Header["Digest"] = "sha-256=:deadbeef:"
		if pass, _ := Match(tc, actual, bodyNoise(), false, false, zap.NewNop(), false, WithAutoHeaderNoise(true)); pass {
			t.Fatal("the server started emitting a Digest it never recorded and it was forgiven")
		}
	})
}

func TestMatch_BodyDigestHeader_ExplicitUserPatternWins(t *testing.T) {
	tc, actual := signinCase(signinBodyA, signinBodyB, etagForBodyA, etagForBodyB)
	noise := bodyNoise()
	// The user narrowed ETag on purpose to one exact value; the replayed value does
	// not match it, so CompareHeaders fails it and that verdict must stand.
	noise["header"] = map[string][]string{"ETag": {`^W/"e3-ONLY-THIS-ONE"$`}}
	if pass, _ := Match(tc, actual, noise, false, false, zap.NewNop(), false, WithAutoHeaderNoise(true)); pass {
		t.Fatal("an explicit user regex on ETag was silently overridden; a deliberate " +
			"narrowing must outrank this rule")
	}
}

// The rule is opt-in, exactly like the volatile-header forgiveness it sits
// beside. A user who passed --disableAutoHeaderNoise, and the enterprise and
// k8s-proxy callers that invoke Match with no MatchOption, must keep the strict
// behaviour -- silently changing it for them is what MatchOption exists to stop.
func TestMatch_BodyDigestHeader_IsOptIn(t *testing.T) {
	tc, actual := signinCase(signinBodyA, signinBodyB, etagForBodyA, etagForBodyB)

	// No option at all: the cross-repo default.
	if pass, _ := Match(tc, actual, bodyNoise(), false, false, zap.NewNop(), false); pass {
		t.Fatal("body-digest forgiveness applied without WithAutoHeaderNoise; that is a " +
			"silent behaviour change for every caller that passes no MatchOption")
	}
	// Explicitly disabled.
	tc2, actual2 := signinCase(signinBodyA, signinBodyB, etagForBodyA, etagForBodyB)
	if pass, _ := Match(tc2, actual2, bodyNoise(), false, false, zap.NewNop(), false, WithAutoHeaderNoise(false)); pass {
		t.Fatal("body-digest forgiveness applied despite WithAutoHeaderNoise(false)")
	}
}
