package http

import (
	"strconv"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// noMock is the body keploy's proxy answers an app's call with when no mock
// matched it: plain text.
const noMock = "keploy: no matching mock found for this HTTP request\n"

// resp is a 200 response with body, its Content-Type and its Content-Length.
func resp(body, contentType string) models.HTTPResp {
	h := map[string]string{"Content-Length": strconv.Itoa(len(body))}
	if contentType != "" {
		h["Content-Type"] = contentType
	}
	return models.HTTPResp{StatusCode: 200, Header: h, Body: body}
}

// A body is never left uncompared when the recorded and the live one differ
// in kind: JSON against a body that is no JSON, or an empty body against a
// non-empty one. The app's 200 with keploy's plain-text "no matching mock"
// where the test recorded {"n":7} passed as a non-JSON body that is not
// compared, its Content-Length (7 against 53) excused as the length of a body
// that "matched": a false pass, with the switch on or off.
func TestMatch_ABodyOfAnotherKindIsCompared(t *testing.T) {
	for _, tc := range []struct {
		name             string
		expected, actual models.HTTPResp
	}{
		{"JSON answered with plain text", resp(`{"n":7}`, "application/json"), resp(noMock, "text/plain; charset=utf-8")},
		{"JSON answered with nothing", resp(`{"n":7}`, "application/json"), resp("", "application/json")},
		{"nothing answered with plain text", resp("", ""), resp(noMock, "text/plain; charset=utf-8")},
		{"plain text answered with nothing", resp("ok", "text/plain"), resp("", "text/plain")},
	} {
		for _, proven := range []string{"", "1"} {
			t.Run(tc.name+"/proven="+proven, func(t *testing.T) {
				t.Setenv(provenswitch.Env, proven)
				exp, act := tc.expected, tc.actual
				ok, res := Match(&models.TestCase{Name: tc.name, HTTPResp: exp}, &act, nil, true, false, zap.NewNop(), false)
				if ok {
					t.Fatal("the test passed")
				}
				if res.BodyResult[0].Normal {
					t.Fatal("the body is reported as matching")
				}
				for _, h := range res.HeadersResult {
					if h.Expected.Key == "Content-Length" && h.Normal {
						t.Fatal("a Content-Length that differs was excused as the length of a matching body")
					}
				}
			})
		}
	}
}

// Under the proven switch a body that is no JSON is compared byte for byte by
// default, as every other part of a response is; without the switch it is
// compared only with --compare-all, as before.
func TestMatch_ProvenSwitchComparesANonJSONBody(t *testing.T) {
	exp := resp("<p>total: 7</p>", "text/html")
	other := resp("<p>total: 8</p>", "text/html")
	same := resp("<p>total: 7</p>", "text/html")
	match := func(actual models.HTTPResp, compareAll bool) bool {
		ok, _ := Match(&models.TestCase{Name: "html", HTTPResp: exp}, &actual, nil, true, compareAll, zap.NewNop(), false)
		return ok
	}

	t.Setenv(provenswitch.Env, "1")
	if match(other, false) {
		t.Fatal("KEPLOY_PROVEN=1: a non-JSON body that differs matched")
	}
	if !match(same, false) {
		t.Fatal("KEPLOY_PROVEN=1: the recorded non-JSON body did not match")
	}

	t.Setenv(provenswitch.Env, "")
	if !match(other, false) {
		t.Fatal("control: with the switch off and no --compare-all a non-JSON body is not compared, as before")
	}
	if match(other, true) {
		t.Fatal("control: with --compare-all a non-JSON body that differs must fail, as before")
	}
}

// A body the test case's noise excludes whole is not compared, whatever its
// kind: the user's own exclusion.
func TestMatch_ANoisedBodyStaysExcludedWhateverItsKind(t *testing.T) {
	for _, proven := range []string{"", "1"} {
		t.Setenv(provenswitch.Env, proven)
		exp := resp(`{"n":7}`, "application/json")
		exp.Header["Content-Length"] = ""
		act := resp(noMock, "application/json")
		act.Header["Content-Length"] = ""
		tc := &models.TestCase{Name: "noised", HTTPResp: exp, Noise: map[string][]string{"body": {}}}
		if ok, res := Match(tc, &act, nil, true, false, zap.NewNop(), false); !ok {
			t.Fatalf("proven=%q: a body the test case noised whole was compared: %+v", proven, res.BodyResult)
		}
	}
}

// A text body of the recorded kind that changed: with the switch off and no
// --compare-all it is not compared, and its Content-Length is excused, as
// keploy documents and does today (a text or HTML body that legitimately
// varies). Under the switch it is compared byte for byte and fails, and its
// length is not excused. A body the test case's noise excludes whole
// excuses its length either way.
func TestMatch_ATextBodyThatChangedFailsOnlyUnderTheSwitch(t *testing.T) {
	exp := resp("count: 7", "text/plain")
	match := func(tc *models.TestCase) (bool, *models.Result) {
		act := resp(noMock, "text/plain")
		return Match(tc, &act, nil, true, false, zap.NewNop(), false)
	}
	contentLengthNormal := func(res *models.Result) bool {
		for _, h := range res.HeadersResult {
			if h.Expected.Key == "Content-Length" {
				return h.Normal
			}
		}
		t.Fatal("the result does not hold the Content-Length")
		return false
	}

	t.Setenv(provenswitch.Env, "")
	if ok, res := match(&models.TestCase{Name: "text", HTTPResp: exp}); !ok || !contentLengthNormal(res) {
		t.Fatalf("switch off: a text body of the recorded kind must stay uncompared with its length excused, as today: %+v", res)
	}

	t.Setenv(provenswitch.Env, "1")
	ok, res := match(&models.TestCase{Name: "text", HTTPResp: exp})
	if ok || res.BodyResult[0].Normal {
		t.Fatal("KEPLOY_PROVEN=1: a text body that changed passed")
	}
	if contentLengthNormal(res) {
		t.Fatal("KEPLOY_PROVEN=1: the Content-Length of a text body that changed was excused")
	}

	for _, proven := range []string{"", "1"} {
		t.Setenv(provenswitch.Env, proven)
		noised := &models.TestCase{Name: "noised", HTTPResp: exp, Noise: map[string][]string{"body": {}}}
		if ok, res := match(noised); !ok {
			t.Fatalf("proven=%q: a body the test case noised whole did not excuse its Content-Length: %+v", proven, res.HeadersResult)
		}
	}
}
