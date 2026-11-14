package http

import (
	"testing"

	"go.keploy.io/server/v3/internal/logleak"
	"go.keploy.io/server/v3/pkg/models"
)

// The matcher logs which assertion failed, the header's or the field's name
// and sizes, never a header's value or a body: a recorded and a live answer
// hold the app's session cookies, tokens and users' data, and the lines reach
// CI logs and keploy-logs.txt. The test report says how they differ. It
// logged both values of a failed header or JSON assertion at ERROR, and
// whole bodies at debug level.
func TestTheMatcherLogsNoneOfTheAnswers(t *testing.T) {
	const s = logleak.Canary
	recorded := models.HTTPResp{StatusCode: 200, Header: map[string]string{"Set-Cookie": "session=" + s, "Content-Type": "application/json"},
		Body: `{"token":"` + s + `","user":"app"}`}
	live := &models.HTTPResp{StatusCode: 200, Header: map[string]string{"Set-Cookie": "session=x" + s, "Content-Type": "application/json"},
		Body: `{"token":"x` + s + `","user":"app"}`}
	tc := func(assertions map[models.AssertionType]interface{}) *models.TestCase {
		return &models.TestCase{Name: "test-1", Kind: models.HTTP,
			HTTPReq:  models.HTTPReq{Method: "GET", URL: "http://api.example.com/v1/me?access_token=" + s, Header: map[string]string{"Authorization": "Bearer " + s}},
			HTTPResp: recorded, Assertions: assertions}
	}
	for _, c := range []struct {
		name string
		run  func(*testing.T) int
	}{
		{"a failed comparison", func(t *testing.T) int {
			logger, logs := logleak.Observe()
			Match(tc(nil), live, nil, false, false, logger, true)
			logleak.Check(t, logs, s, "Match")
			if logs.FilterMessage("compared bodies").Len() == 0 {
				t.Error("precondition: the compared bodies' line was not logged")
			}
			return logs.Len()
		}},
		{"failed assertions", func(t *testing.T) int {
			logger, logs := logleak.Observe()
			AssertionMatch(tc(map[models.AssertionType]interface{}{
				models.HeaderEqual:    map[string]interface{}{"Set-Cookie": "session=" + s},
				models.HeaderContains: map[string]interface{}{"Set-Cookie": s + "zz"},
				models.HeaderMatches:  map[string]interface{}{"Set-Cookie": "^session=" + s + "$"},
				models.JsonEqual:      true,
				models.JsonContains:   map[string]interface{}{"token": s},
			}), live, logger)
			logleak.Check(t, logs, s, "AssertionMatch")
			for _, line := range []string{"header_equal assertion failed", "header_contains assertion failed", "header_matches assertion failed", "json_equal assertion failed", "json_contains assertion failed"} {
				if logs.FilterMessage(line).Len() == 0 {
					t.Errorf("precondition: %q was not logged; the test drove nothing there", line)
				}
			}
			return logs.Len()
		}},
		{"an answer recorded over 1MB", func(t *testing.T) int {
			logger, logs := logleak.Observe()
			big := tc(nil)
			big.HTTPResp.BodySkipped, big.HTTPResp.BodySize, big.HTTPResp.Body = true, 2<<20, ""
			Match(big, live, nil, false, false, logger, true)
			logleak.Check(t, logs, s, "Match of a skipped body")
			if logs.FilterMessage("actual response body (skipped during recording)").Len() == 0 {
				t.Error("precondition: the skipped body's line was not logged")
			}
			return logs.Len()
		}},
		{"two test cases that differ", func(t *testing.T) int {
			logger, logs := logleak.Observe()
			other := tc(nil)
			other.HTTPReq.URL = "http://api.example.com/v1/me?access_token=y" + s
			other.HTTPReq.Header = map[string]string{"Authorization": "Bearer y" + s}
			other.HTTPResp = *live
			AbsMatch(tc(nil), other, nil, false, logger)
			logleak.Check(t, logs, s, "AbsMatch")
			return logs.Len()
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			if c.run(t) == 0 {
				t.Fatal("precondition: nothing was logged; the test drove none of the lines")
			}
		})
	}
}
