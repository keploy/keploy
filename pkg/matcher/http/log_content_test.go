package http

import (
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils/logcontent/logtest"
)

// canary is the app's: in a test case's request and response.
const canary = "kp-canary-matcher-5b1f"

func canaryCase(name, v string) *models.TestCase {
	return &models.TestCase{
		Name: name, Kind: models.HTTP,
		HTTPReq: models.HTTPReq{Method: "POST", URL: "http://app/notes?ref=" + v, URLParams: map[string]string{"ref": v},
			Header: map[string]string{"Authorization": "Bearer " + v}, Body: `{"note":"` + v + `"}`},
		HTTPResp: models.HTTPResp{StatusCode: 200, Header: map[string]string{"X-Note": v}, Body: `{"note":"` + v + `"}`},
	}
}

// Two test cases compared field by field are logged by what differs, not the
// values that differ, unless KEPLOY_LOG_CONTENT is set.
func TestAbsMatch_TheLinesCarryNoRequestOrResponse(t *testing.T) {
	logtest.Each(t, func(t *testing.T, on bool) {
		logger, logs := logtest.Observe()
		if pass, _, _, _ := AbsMatch(canaryCase("a", canary+"-a"), canaryCase("b", canary+"-b"), nil, false, logger); pass {
			t.Fatal("two test cases that differ matched")
		}
		logtest.Check(t, logs, canary, on,
			"test case http req url is not equal",
			"test case http req url params are not equal",
			"test case http req headers are not equal",
			"test case http resp headers are not equal",
			"cleanExp", "cleanAct",
			"test case http req is not equal",
			"test case http resp is not equal")
	})
}

// A test case's url params of another count are logged by their names.
func TestAbsMatch_AnotherCountOfURLParamsIsLoggedByName(t *testing.T) {
	logtest.Each(t, func(t *testing.T, on bool) {
		logger, logs := logtest.Observe()
		b := canaryCase("b", canary+"-a")
		b.HTTPReq.URLParams["page"] = canary + "-page"
		_, _, _, _ = AbsMatch(canaryCase("a", canary+"-a"), b, nil, false, logger)
		logtest.Check(t, logs, canary, on, "test case http req url params are not equal")
	})
}

// The app's answer is compared without its body in the lines, both by value
// and, for a body recorded by its size alone, by schema, unless
// KEPLOY_LOG_CONTENT is set.
func TestMatch_TheAnswersBodyIsNotLogged(t *testing.T) {
	t.Run("by value", func(t *testing.T) {
		logtest.Each(t, func(t *testing.T, on bool) {
			logger, logs := logtest.Observe()
			tc := canaryCase("a", canary+"-recorded")
			actual := canaryCase("a", canary+"-live").HTTPResp
			if ok, _ := Match(tc, &actual, nil, false, false, logger, false); ok {
				t.Fatal("another body matched")
			}
			logtest.Check(t, logs, canary, on, "cleanExp", "cleanAct")
		})
	})
	t.Run("recorded by its size", func(t *testing.T) {
		logtest.Each(t, func(t *testing.T, on bool) {
			logger, logs := logtest.Observe()
			tc := canaryCase("a", canary)
			tc.HTTPResp.Body, tc.HTTPResp.BodySkipped, tc.HTTPResp.BodySize = "", true, 2
			actual := canaryCase("a", canary).HTTPResp
			_, _ = Match(tc, &actual, nil, false, false, logger, false)
			logtest.Check(t, logs, canary, on, "actual response body (skipped during recording)")
		})
	})
	t.Run("by schema, recorded by its size", func(t *testing.T) {
		logtest.Each(t, func(t *testing.T, on bool) {
			logger, logs := logtest.Observe()
			tc := canaryCase("a", canary)
			tc.HTTPResp.Body, tc.HTTPResp.BodySkipped, tc.HTTPResp.BodySize = "", true, 2
			actual := canaryCase("a", canary).HTTPResp
			_, _ = MatchSchema(tc, &actual, logger)
			logtest.Check(t, logs, canary, on, "actual response body (skipped during recording)")
		})
	})
}

// A test case compared with no other is logged by which side is missing, not
// the test case that is there, unless KEPLOY_LOG_CONTENT is set.
func TestAbsMatch_AMissingTestCaseIsLoggedWithoutTheOther(t *testing.T) {
	for name, firstMissing := range map[string]bool{"the second missing": false, "the first missing": true} {
		t.Run(name, func(t *testing.T) {
			logtest.Each(t, func(t *testing.T, on bool) {
				logger, logs := logtest.Observe()
				tcs1, tcs2 := canaryCase("a", canary), (*models.TestCase)(nil)
				if firstMissing {
					tcs1, tcs2 = tcs2, tcs1
				}
				if pass, _, _, _ := AbsMatch(tcs1, tcs2, nil, false, logger); pass {
					t.Fatal("a test case matched no test case")
				}
				logtest.Check(t, logs, canary, on, "test case is nil")
			})
		})
	}
}

// A response body compared by size, because the other side's was recorded by
// its size alone, is logged by its size, unless KEPLOY_LOG_CONTENT is set.
func TestAbsMatch_TheBodyComparedBySizeIsNotLogged(t *testing.T) {
	for kept, tcs1Skipped := range map[string]bool{
		"tcs2 response body (other side was skipped)": true,
		"tcs1 response body (other side was skipped)": false,
	} {
		t.Run(kept, func(t *testing.T) {
			logtest.Each(t, func(t *testing.T, on bool) {
				logger, logs := logtest.Observe()
				skipped, other := canaryCase("a", canary), canaryCase("b", canary)
				skipped.HTTPResp.Body, skipped.HTTPResp.BodySkipped, skipped.HTTPResp.BodySize = "", true, 2
				tcs1, tcs2 := skipped, other
				if !tcs1Skipped {
					tcs1, tcs2 = other, skipped
				}
				_, _, _, _ = AbsMatch(tcs1, tcs2, nil, false, logger)
				logtest.Check(t, logs, canary, on, kept)
			})
		})
	}
}

// An assertion that fails is logged by its name and the header it judged,
// not the values it compared, unless KEPLOY_LOG_CONTENT is set.
func TestAssertionMatch_AFailedAssertionIsLoggedWithoutTheValuesItCompared(t *testing.T) {
	for want, assertion := range map[string]map[models.AssertionType]interface{}{
		"json_equal assertion failed":      {models.JsonEqual: true},
		"json_contains assertion failed":   {models.JsonContains: map[string]interface{}{"note": canary + "-expected"}},
		"header_equal assertion failed":    {models.HeaderEqual: map[string]interface{}{"X-Note": canary + "-expected"}},
		"header_contains assertion failed": {models.HeaderContains: map[string]interface{}{"X-Note": canary + "-expected"}},
		"header_matches assertion failed":  {models.HeaderMatches: map[string]interface{}{"X-Note": "^" + canary + "-expected$"}},
	} {
		t.Run(want, func(t *testing.T) {
			logtest.Each(t, func(t *testing.T, on bool) {
				logger, logs := logtest.Observe()
				tc := canaryCase("a", canary+"-recorded")
				tc.Assertions = assertion
				actual := canaryCase("a", canary+"-live").HTTPResp
				if pass, _ := AssertionMatch(tc, &actual, logger); pass {
					t.Fatal("an answer that differs passed the assertion")
				}
				logtest.Check(t, logs, canary, on, want)
			})
		})
	}
}
