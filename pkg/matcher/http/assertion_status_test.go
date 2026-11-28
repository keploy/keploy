package http

import (
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// As for gRPC: the status is a difference only when a status assertion
// (status_code, status_code_class, status_code_in) failed.
func TestAssertionMatch_TheStatusIsADifferenceOnlyWhenAStatusAssertionFailed(t *testing.T) {
	for _, c := range []struct {
		name       string
		assertions map[models.AssertionType]interface{}
		wantNormal bool
	}{
		{"a header assertion failed", map[models.AssertionType]interface{}{models.HeaderExists: []interface{}{"X-Missing"}}, true},
		{"status_code failed", map[models.AssertionType]interface{}{models.StatusCode: 201}, false},
		{"status_code_class failed", map[models.AssertionType]interface{}{models.StatusCodeClass: "4xx"}, false},
		{"status_code_in failed", map[models.AssertionType]interface{}{models.StatusCodeIn: []interface{}{"500"}}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			tc := &models.TestCase{Name: "a", HTTPResp: models.HTTPResp{StatusCode: 200}, Assertions: c.assertions}
			pass, res := AssertionMatch(tc, &models.HTTPResp{StatusCode: 200, Header: map[string]string{}}, zap.NewNop())
			if pass {
				t.Fatal("the assertion failed; the test passed")
			}
			if res.StatusCode.Normal != c.wantNormal {
				t.Fatalf("StatusCode.Normal = %v, want %v", res.StatusCode.Normal, c.wantNormal)
			}
		})
	}
}

// Whatever order the assertions run in (a map's), a failed status assertion
// leaves the status a difference: a passing status_code beside a failing
// status_code_in used to mark it matched when it ran last.
func TestAssertionMatch_AFailedStatusAssertionWinsWhateverTheOrder(t *testing.T) {
	tc := &models.TestCase{Name: "a", HTTPResp: models.HTTPResp{StatusCode: 200},
		Assertions: map[models.AssertionType]interface{}{models.StatusCode: 200, models.StatusCodeIn: []interface{}{"201"}}}
	for i := 0; i < 50; i++ {
		_, res := AssertionMatch(tc, &models.HTTPResp{StatusCode: 200, Header: map[string]string{}}, zap.NewNop())
		if res.StatusCode.Normal {
			t.Fatalf("run %d: StatusCode.Normal = true, want false (status_code_in failed)", i)
		}
	}
	// And a failed status_code says what it asked for, the recorded status
	// kept as Expected.
	tc.Assertions = map[models.AssertionType]interface{}{models.StatusCode: 201}
	if _, res := AssertionMatch(tc, &models.HTTPResp{StatusCode: 200, Header: map[string]string{}}, zap.NewNop()); res.StatusCode.ExpectedText != "201" || res.StatusCode.Expected != 200 {
		t.Fatalf("StatusCode = %+v, want asked 201, recorded 200", res.StatusCode)
	}
	// An empty status_code_class is said, not left to read as the recorded status.
	tc.Assertions = map[models.AssertionType]interface{}{models.StatusCodeClass: ""}
	if _, res := AssertionMatch(tc, &models.HTTPResp{StatusCode: 200, Header: map[string]string{}}, zap.NewNop()); res.StatusCode.ExpectedText == "" {
		t.Fatalf("an empty class left no ExpectedText: %+v", res.StatusCode)
	}
}

// A failed status_code_in or status_code_class says what it asked for.
func TestAssertionMatch_AFailedStatusInOrClassSaysWhatItAskedFor(t *testing.T) {
	for _, c := range []struct {
		assertions map[models.AssertionType]interface{}
		want       string
	}{
		{map[models.AssertionType]interface{}{models.StatusCodeIn: []interface{}{"200", "201"}}, "one of 200, 201"},
		{map[models.AssertionType]interface{}{models.StatusCodeClass: "4xx"}, "4xx"},
	} {
		tc := &models.TestCase{Name: "a", HTTPResp: models.HTTPResp{StatusCode: 500}, Assertions: c.assertions}
		_, res := AssertionMatch(tc, &models.HTTPResp{StatusCode: 500, Header: map[string]string{}}, zap.NewNop())
		if res.StatusCode.Normal || res.StatusCode.ExpectedText != c.want {
			t.Fatalf("%v: StatusCode = %+v, want ExpectedText %q", c.assertions, res.StatusCode, c.want)
		}
	}
}
