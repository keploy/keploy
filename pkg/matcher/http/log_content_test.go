package http

import (
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/agent/logleak"
	"go.keploy.io/server/v3/pkg/models"
)

// Planted content: a query value, a header value, a body and a URL
// parameter of each side, each in no log line.
const (
	// No two share a run of logleak.Window bytes, so a finding names the
	// one that leaked.
	queryRec, queryLive   = "alphaQueryRec1", "bravoQueryLive2"
	headerRec, headerLive = "charlieTokenRec", "deltaTokenLive"
	bodyRec, bodyLive     = "echoBodyRecord", "foxtrotBodyLive"
	// noiseKey is a map key that is a value (an account number), as a
	// learned noise path can hold one.
	noiseKey = "8812734455"
)

func assertNoPlant(t *testing.T, log string) {
	t.Helper()
	for _, v := range []string{queryRec, queryLive, headerRec, headerLive, bodyRec, bodyLive, noiseKey} {
		if found := logleak.Find(t, log, []byte(v)); len(found) > 0 {
			t.Errorf("the log holds %q: %v\n%s", v, found, log)
		}
	}
}

func plantedCase(name, query, header, body string) *models.TestCase {
	return &models.TestCase{
		Name: name,
		Kind: models.HTTP,
		HTTPReq: models.HTTPReq{
			Method:    "POST",
			URL:       "http://app.local/users?token=" + query,
			URLParams: map[string]string{"token": query},
			Header:    map[string]string{"Authorization": header, "Content-Type": "application/json"},
			Body:      `{"v":"` + body + `"}`,
		},
		HTTPResp: models.HTTPResp{
			StatusCode: 200,
			Header:     map[string]string{"Set-Cookie": header, "Content-Type": "application/json"},
			Body:       `{"v":"` + body + `"}`,
		},
		Noise: map[string][]string{"body.accounts." + noiseKey + "." + name: {}},
	}
}

// AbsMatch, comparing two test cases, logs where they differ (the URL's
// shape, the names of the differing headers and parameters, the noise's
// field shapes, a body's length), never a value of either.
func TestAbsMatch_LogsNoValueOfEitherTestCase(t *testing.T) {
	logger, out := logleak.Logger(nil)
	pass, _, _, _ := AbsMatch(plantedCase("rec", queryRec, headerRec, bodyRec), plantedCase("live", queryLive, headerLive, bodyLive), nil, false, logger)
	if pass {
		t.Fatal("two test cases that differ everywhere matched")
	}
	for _, line := range []string{"url is not equal", "url params are not equal", "headers are not equal", "noise is not equal"} {
		if !strings.Contains(out.String(), line) {
			t.Errorf("no %q line, so the row checks nothing of it:\n%s", line, out.String())
		}
	}
	assertNoPlant(t, out.String())

	// A body one side skipped (over 1 MB), either side: the other's is
	// logged by its length.
	for _, recSkipped := range []bool{true, false} {
		rec, live := plantedCase("rec", queryRec, headerRec, bodyRec), plantedCase("live", queryLive, headerLive, bodyLive)
		skipped := map[bool]*models.TestCase{true: rec, false: live}[recSkipped]
		skipped.HTTPResp.BodySkipped, skipped.HTTPResp.BodySize = true, 2<<20
		logger, out = logleak.Logger(nil)
		AbsMatch(rec, live, nil, false, logger)
		if !strings.Contains(out.String(), "other side was skipped") {
			t.Errorf("rec skipped=%v: no skipped-body line:\n%s", recSkipped, out.String())
		}
		assertNoPlant(t, out.String())
	}

	logger, out = logleak.Logger(nil)
	AbsMatch(nil, plantedCase("live", queryLive, headerLive, bodyLive), nil, false, logger)
	assertNoPlant(t, out.String())
}

// A failed assertion names the assertion, the header and the lengths, never
// the answer's body or header value.
func TestMatch_AssertionLinesLogNoAnswerContent(t *testing.T) {
	for _, a := range []map[models.AssertionType]interface{}{
		{models.JsonEqual: true},
		{models.HeaderMatches: map[string]interface{}{"Set-Cookie": "^nomatch$"}},
		{models.HeaderEqual: map[string]interface{}{"Set-Cookie": "other"}},
		{models.HeaderContains: map[string]interface{}{"Set-Cookie": "nomatch"}},
	} {
		tc := plantedCase("rec", queryRec, headerRec, bodyRec)
		// Match reads a test case's noise in pkg/matcher, which logs a noise
		// entry that matches nothing as it is (not this package's line).
		tc.Noise = nil
		tc.Assertions = a
		logger, out := logleak.Logger(nil)
		live := plantedCase("live", queryLive, headerLive, bodyLive).HTTPResp
		if pass, _ := Match(tc, &live, nil, false, false, logger, true); pass {
			t.Fatalf("%v passed an answer it fails", a)
		}
		if !strings.Contains(out.String(), "assertion failed") {
			t.Errorf("%v: no failed-assertion line:\n%s", a, out.String())
		}
		assertNoPlant(t, out.String())
	}

	// A recording that kept only its body's size: the answer's body is
	// logged by its length.
	tc := plantedCase("rec", queryRec, headerRec, bodyRec)
	tc.Noise = nil
	tc.HTTPResp.Body, tc.HTTPResp.BodySkipped, tc.HTTPResp.BodySize = "", true, 2<<20
	live := plantedCase("live", queryLive, headerLive, bodyLive).HTTPResp
	logger, out := logleak.Logger(nil)
	Match(tc, &live, nil, false, false, logger, true)
	// Match clears the answer's body it compared by size: MatchSchema is
	// given an answer of its own.
	live = plantedCase("live", queryLive, headerLive, bodyLive).HTTPResp
	logger2, out2 := logleak.Logger(nil)
	MatchSchema(tc, &live, logger2)
	for _, log := range []string{out.String(), out2.String()} {
		if !strings.Contains(log, "skipped during recording") {
			t.Errorf("no skipped-body line:\n%s", log)
		}
		assertNoPlant(t, log)
	}
}
