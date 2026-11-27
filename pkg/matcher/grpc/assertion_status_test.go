package grpc

import (
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// A test case whose assertions failed on a header alone did not judge its
// status: the status reads as unchanged, never as a change from 0 to 0. A
// failed status assertion still reads as a changed status.
func TestAssertionMatch_TheStatusIsADifferenceOnlyWhenAStatusAssertionFailed(t *testing.T) {
	for _, c := range []struct {
		name       string
		assertions map[models.AssertionType]interface{}
		wantNormal bool
	}{
		{"a header assertion failed", map[models.AssertionType]interface{}{models.HeaderExists: []interface{}{"x-missing"}}, true},
		{"a status assertion failed", map[models.AssertionType]interface{}{models.StatusCode: 5}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			tc := &models.TestCase{Name: "a", GrpcResp: *newTestGrpcResp(`{}`, "0"), Assertions: c.assertions}
			pass, res := AssertionMatch(tc, newTestGrpcResp(`{}`, "0"), zap.NewNop())
			if pass {
				t.Fatal("the assertion failed; the test passed")
			}
			if res.StatusCode.Normal != c.wantNormal {
				t.Fatalf("StatusCode.Normal = %v, want %v", res.StatusCode.Normal, c.wantNormal)
			}
		})
	}
}

// A failed status_code assertion says the status it asked for
// (ExpectedText), Expected staying the recorded one its readers take it for:
// asserting 5 of an answer of 0 recorded as 0 read as a change from 0 to 0.
func TestAssertionMatch_AFailedStatusAssertionExpectsTheStatusItAskedFor(t *testing.T) {
	tc := &models.TestCase{Name: "a", GrpcResp: *newTestGrpcResp(`{}`, "0"),
		Assertions: map[models.AssertionType]interface{}{models.StatusCode: 5}}
	_, res := AssertionMatch(tc, newTestGrpcResp(`{}`, "0"), zap.NewNop())
	if res.StatusCode.Normal || res.StatusCode.ExpectedText != "5" || res.StatusCode.Expected != 0 || res.StatusCode.Actual != 0 {
		t.Fatalf("StatusCode = %+v, want not normal, asked 5, recorded 0 kept as Expected", res.StatusCode)
	}
}

// A failed status_code_in says what it asked for (IntResult.ExpectedText).
func TestAssertionMatch_AFailedStatusInSaysWhatItAskedFor(t *testing.T) {
	tc := &models.TestCase{Name: "a", GrpcResp: *newTestGrpcResp(`{}`, "0"),
		Assertions: map[models.AssertionType]interface{}{models.StatusCodeIn: []interface{}{5, 7}}}
	_, res := AssertionMatch(tc, newTestGrpcResp(`{}`, "0"), zap.NewNop())
	if res.StatusCode.Normal || res.StatusCode.ExpectedText != "one of 5, 7" {
		t.Fatalf("StatusCode = %+v", res.StatusCode)
	}
}

// Two failed status assertions are both said, in a fixed order, whatever
// order the map ran them in.
func TestAssertionMatch_FailedStatusAssertionsAreAllSaidInAFixedOrder(t *testing.T) {
	tc := &models.TestCase{Name: "a", GrpcResp: *newTestGrpcResp(`{}`, "0"),
		Assertions: map[models.AssertionType]interface{}{models.StatusCode: 5, models.StatusCodeIn: []interface{}{7, 9}}}
	for i := 0; i < 30; i++ {
		_, res := AssertionMatch(tc, newTestGrpcResp(`{}`, "0"), zap.NewNop())
		if res.StatusCode.ExpectedText != "5; one of 7, 9" {
			t.Fatalf("run %d: ExpectedText = %q", i, res.StatusCode.ExpectedText)
		}
	}
}
