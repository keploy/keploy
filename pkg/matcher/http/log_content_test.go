package http

import (
	"fmt"
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
	const unreadable = " assertion cannot be read, so it fails the test case: its value is not "
	for _, tc := range []struct {
		name, want string
		assertion  map[models.AssertionType]interface{}
	}{
		{"json_equal", "json_equal assertion failed", map[models.AssertionType]interface{}{models.JsonEqual: true}},
		{"json_contains", "json_contains assertion failed", map[models.AssertionType]interface{}{models.JsonContains: map[string]interface{}{"note": canary + "-expected"}}},
		{"header_equal", "header_equal assertion failed", map[models.AssertionType]interface{}{models.HeaderEqual: map[string]interface{}{"X-Note": canary + "-expected"}}},
		{"header_contains", "header_contains assertion failed", map[models.AssertionType]interface{}{models.HeaderContains: map[string]interface{}{"X-Note": canary + "-expected"}}},
		{"header_matches", "header_matches assertion failed", map[models.AssertionType]interface{}{models.HeaderMatches: map[string]interface{}{"X-Note": "^" + canary + "-expected$"}}},
		// regexp's error quotes the pattern it cannot compile.
		{"header_matches, a pattern that does not compile", "header_matches assertion failed",
			map[models.AssertionType]interface{}{models.HeaderMatches: map[string]interface{}{"X-Note": "(" + canary + "-expected"}}},
		// A value of a shape the assertion does not take is still the
		// expected body or header value: the line names its type.
		{"status_code_in, a string", "status_code_in" + unreadable + "a list of status codes",
			map[models.AssertionType]interface{}{models.StatusCodeIn: canary + "-expected"}},
		{"header_equal, a list", "header_equal" + unreadable + "a map of header names to values",
			map[models.AssertionType]interface{}{models.HeaderEqual: []interface{}{"X-Note: " + canary + "-expected"}}},
		{"header_equal, a string", "header_equal" + unreadable + "a map of header names to values",
			map[models.AssertionType]interface{}{models.HeaderEqual: canary + "-expected"}},
		{"header_contains, a list", "header_contains" + unreadable + "a map of header names to substrings",
			map[models.AssertionType]interface{}{models.HeaderContains: []interface{}{"X-Note: " + canary + "-expected"}}},
		{"header_matches, a list", "header_matches" + unreadable + "a map of header names to patterns",
			map[models.AssertionType]interface{}{models.HeaderMatches: []interface{}{"X-Note: ^" + canary + "-expected$"}}},
		{"json_contains, a list", "json_contains" + unreadable + "a JSON object, as a map or a string",
			map[models.AssertionType]interface{}{models.JsonContains: []interface{}{canary + "-expected"}}},
		{"json_contains, a JSON array", "json_contains" + unreadable + "a JSON object, as a map or a string",
			map[models.AssertionType]interface{}{models.JsonContains: `["` + canary + `-expected"]`}},
		{"header_exists, a string", "header_exists: unsupported format, expected slice or map",
			map[models.AssertionType]interface{}{models.HeaderExists: canary + "-expected"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logtest.Each(t, func(t *testing.T, on bool) {
				logger, logs := logtest.Observe()
				recorded := canaryCase("a", canary+"-recorded")
				recorded.Assertions = tc.assertion
				actual := canaryCase("a", canary+"-live").HTTPResp
				if pass, _ := AssertionMatch(recorded, &actual, logger); pass {
					t.Fatal("an answer that differs passed the assertion")
				}
				logtest.Check(t, logs, canary, on, tc.want)
			})
		})
	}
}

// A header_matches pattern that does not compile still says why with the
// switch off: regexp's reason, without the pattern its error quotes.
func TestAssertionMatch_APatternThatDoesNotCompileSaysWhy(t *testing.T) {
	logtest.Each(t, func(t *testing.T, on bool) {
		logger, logs := logtest.Observe()
		recorded := canaryCase("a", canary+"-recorded")
		recorded.Assertions = map[models.AssertionType]interface{}{models.HeaderMatches: map[string]interface{}{"X-Note": "(" + canary}}
		actual := canaryCase("a", canary+"-live").HTTPResp
		if pass, _ := AssertionMatch(recorded, &actual, logger); pass {
			t.Fatal("a pattern that does not compile passed")
		}
		const line = "header_matches assertion failed"
		logtest.Check(t, logs, canary, on, line)
		want := "error parsing regexp: missing closing )"
		if on {
			want += ": `(" + canary + "`"
		}
		if got := logs.FilterMessage(line).All()[0].ContextMap()["error"]; got != want {
			t.Fatalf("the line says %q, want %q", got, want)
		}
	})
}

// A pattern's error that is not regexp's own is logged as logcontent.Error
// logs it: by its kind, and its text only with the switch on. None is today,
// as regexp's compile errors are *syntax.Error; the line still may not carry
// the pattern if one ever is.
func TestPatternError_AnErrorNotRegexpsIsLoggedByItsKind(t *testing.T) {
	logtest.Each(t, func(t *testing.T, on bool) {
		logger, logs := logtest.Observe()
		logger.Error("header_matches assertion failed", patternError(fmt.Errorf("compile %q: too complex", "("+canary)))
		logtest.Check(t, logs, canary, on, "header_matches assertion failed")
	})
}

// Two curl commands whose headers differ are logged by the headers' names,
// not their values, unless KEPLOY_LOG_CONTENT is set.
func TestCompareCurl_TheHeadersAreLoggedByName(t *testing.T) {
	curl := func(v string) string {
		return `curl --request GET \
  --url http://app/notes \
  --header 'Authorization: Bearer ` + v + `' \
  --header 'Cookie: session=` + v + `'`
	}
	logtest.Each(t, func(t *testing.T, on bool) {
		logger, logs := logtest.Observe()
		if CompareCurl(curl(canary+"-a"), curl(canary+"-b"), logger) {
			t.Fatal("two curl commands whose headers differ matched")
		}
		logtest.Check(t, logs, canary, on, "test case curl headers are not equal")
	})
}
