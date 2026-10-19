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
