package http

import (
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// pingCase reproduces checkr's get-ping-1 verbatim: a kubelet probe of /ping
// whose status and body match exactly and whose ONLY difference is the
// X-Request-Id Rails minted for that call. Values are the real ones from bundle
// 17125508-9a11-4a5a-b780-2c8c8e11b58f.
func pingCase() (*models.TestCase, *models.HTTPResp) {
	tc := &models.TestCase{
		Name: "get-ping-1",
		HTTPResp: models.HTTPResp{
			StatusCode: 200,
			Header: map[string]string{
				"Content-Type":    "text/plain; charset=utf-8",
				"X-Frame-Options": "SAMEORIGIN",
				"X-Request-Id":    "1aa063751102fc7793c0074ea388ecce",
			},
			Body: "200 OK",
		},
	}
	actual := &models.HTTPResp{
		StatusCode: 200,
		Header: map[string]string{
			"Content-Type":    "text/plain; charset=utf-8",
			"X-Frame-Options": "SAMEORIGIN",
			"X-Request-Id":    "e61200457948f19656cab129c18310c9",
		},
		Body: "200 OK",
	}
	return tc, actual
}

// THE DEFECT. Keploy already classifies X-Request-Id as per-request volatile and
// auto-noises it for egress mock matching; nothing applied that knowledge to the
// response assertion, so this case failed on a value the app mints per call.
func TestMatch_AutoHeaderNoise_IgnoresPerRequestId(t *testing.T) {
	tc, actual := pingCase()
	pass, _ := Match(tc, actual, nil, false, false, zap.NewNop(), false, WithAutoHeaderNoise(true))
	if !pass {
		t.Fatal("a differing X-Request-Id must not fail a case whose status and body match")
	}
}

// Default OFF: a caller that says nothing keeps the old, strict behaviour.
func TestMatch_AutoHeaderNoise_OffByDefault(t *testing.T) {
	tc, actual := pingCase()
	if pass, _ := Match(tc, actual, nil, false, false, zap.NewNop(), false); pass {
		t.Fatal("without the option the differing header must still fail")
	}
	if pass, _ := Match(tc, actual, nil, false, false, zap.NewNop(), false, WithAutoHeaderNoise(false)); pass {
		t.Fatal("WithAutoHeaderNoise(false) must keep the old behaviour (--disableAutoHeaderNoise)")
	}
}

// NO OVER-SUPPRESSION. headerNoise is resolved with strings.Contains, so seeding
// the raw list would let "date" swallow "X-Candidate-Id" — a real header shape
// for this very customer. Only exact names are seeded, so a genuine regression
// in a lookalike header still fails.
func TestMatch_AutoHeaderNoise_DoesNotSuppressLookalikeHeaders(t *testing.T) {
	for _, victim := range []string{"X-Candidate-Id", "X-Validate-Token", "X-Web3-Addr"} {
		// The Date header MUST be present: it is the trigger. An earlier version
		// of this test omitted it, so the "date" key that does the damage was
		// never in play and the test passed while the property it names was
		// broken.
		tc := &models.TestCase{
			Name: "lookalike",
			HTTPResp: models.HTTPResp{StatusCode: 200, Body: "ok", Header: map[string]string{
				"Date": "Mon, 01 Jan 2026 00:00:00 GMT", victim: "expected",
			}},
		}
		actual := &models.HTTPResp{StatusCode: 200, Body: "ok", Header: map[string]string{
			"Date": "Tue, 02 Jan 2026 00:00:00 GMT", victim: "DRIFTED",
		}}
		if pass, _ := Match(tc, actual, nil, false, false, zap.NewNop(), false, WithAutoHeaderNoise(true)); pass {
			t.Fatalf("%s is not a flaky header and must still be asserted", victim)
		}
	}
}

// A non-flaky header drifting alongside a flaky one must still fail the case.
func TestMatch_AutoHeaderNoise_StillFailsOnRealHeaderDrift(t *testing.T) {
	tc, actual := pingCase()
	tc.HTTPResp.Header["X-Frame-Options"] = "SAMEORIGIN"
	actual.Header["X-Frame-Options"] = "DENY"
	if pass, _ := Match(tc, actual, nil, false, false, zap.NewNop(), false, WithAutoHeaderNoise(true)); pass {
		t.Fatal("a real header change must still fail even with auto header noise on")
	}
}

// An explicit user entry wins: auto-noise seeds only what nobody has spoken for.
func TestMatch_AutoHeaderNoise_UserPatternWins(t *testing.T) {
	tc, actual := pingCase()
	// A pattern the replayed value does NOT satisfy: the user constrained this
	// header, so the case must fail rather than be blanket-ignored.
	tc.Noise = map[string][]string{"header.X-Request-Id": {"^1aa063"}}
	if pass, _ := Match(tc, actual, nil, false, false, zap.NewNop(), false, WithAutoHeaderNoise(true)); pass {
		t.Fatal("an explicit test-case pattern must not be widened into a blanket ignore")
	}
}

// Presence is still asserted. A server that STOPS emitting a volatile header, or
// STARTS emitting one it never did, is a real change — the forgiveness is about
// values only, and applies solely when both sides carried the header.
func TestMatch_AutoHeaderNoise_PresenceStillAsserted(t *testing.T) {
	base := func(h map[string]string) *models.HTTPResp {
		return &models.HTTPResp{StatusCode: 200, Header: h, Body: "ok"}
	}
	withDate := "Mon, 01 Jan 2026 00:00:00 GMT"

	// dropped by the server
	tc := &models.TestCase{Name: "dropped", HTTPResp: *base(map[string]string{
		"Date": withDate, "X-Request-Id": "aaa"})}
	if pass, _ := Match(tc, base(map[string]string{"Date": withDate}), nil, false, false,
		zap.NewNop(), false, WithAutoHeaderNoise(true)); pass {
		t.Fatal("a volatile header the server stopped emitting must still fail")
	}

	// appeared only in the actual response
	tc2 := &models.TestCase{Name: "extra", HTTPResp: *base(map[string]string{"Date": withDate})}
	if pass, _ := Match(tc2, base(map[string]string{"Date": withDate, "X-Request-Id": "zzz"}),
		nil, false, false, zap.NewNop(), false, WithAutoHeaderNoise(true)); pass {
		t.Fatal("a volatile header that appeared only on replay must still fail")
	}
}

// Headers that are meaningful on a response are NOT in the volatile set, even
// though they are in FlakyHeaders (which is curated for request volatility).
func TestMatch_AutoHeaderNoise_KeepsMeaningfulResponseHeadersAsserted(t *testing.T) {
	for _, hdr := range []string{"Authorization", "X-Csrf-Token", "X-Xsrf-Token", "Idempotency-Key"} {
		tc := &models.TestCase{Name: hdr, HTTPResp: models.HTTPResp{StatusCode: 200, Body: "ok",
			Header: map[string]string{"Date": "Mon, 01 Jan 2026 00:00:00 GMT", hdr: "expected"}}}
		act := &models.HTTPResp{StatusCode: 200, Body: "ok",
			Header: map[string]string{"Date": "Tue, 02 Jan 2026 00:00:00 GMT", hdr: "DRIFTED"}}
		if pass, _ := Match(tc, act, nil, false, false, zap.NewNop(), false, WithAutoHeaderNoise(true)); pass {
			t.Fatalf("%s is meaningful on a response and must stay asserted", hdr)
		}
	}
}
