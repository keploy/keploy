package testdb

import (
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// A gRPC test case written again (normalize, an edit, a re-record) keeps its
// assertions and its description, in YAML and JSON alike, as an HTTP one
// does: they used to be dropped, only the noise kept.
func TestAGrpcTestCaseWrittenAgainKeepsItsAssertionsAndDescription(t *testing.T) {
	tc := unaryGrpcTestCase()
	tc.Description = "reads one order"
	tc.Assertions = map[models.AssertionType]interface{}{
		models.StatusCode:   0,
		models.HeaderExists: []interface{}{"x-trace"},
	}
	tc.Noise = map[string][]string{"body.3": {}}

	check := func(t *testing.T, back *models.TestCase) {
		t.Helper()
		if back.Description != tc.Description {
			t.Errorf("description = %q, want %q", back.Description, tc.Description)
		}
		if _, ok := back.Assertions[models.StatusCode]; !ok {
			t.Errorf("status_code assertion dropped: %+v", back.Assertions)
		}
		if _, ok := back.Assertions[models.HeaderExists]; !ok {
			t.Errorf("header_exists assertion dropped: %+v", back.Assertions)
		}
		if _, ok := back.Noise["body.3"]; !ok {
			t.Errorf("noise dropped: %+v", back.Noise)
		}
	}
	t.Run("yaml", func(t *testing.T) {
		doc, err := EncodeTestcase(tc, zap.NewNop())
		if err != nil {
			t.Fatal(err)
		}
		back, err := Decode(doc, zap.NewNop())
		if err != nil {
			t.Fatal(err)
		}
		check(t, back)
		// Written once more from what was read: still all there.
		doc2, err := EncodeTestcase(*back, zap.NewNop())
		if err != nil {
			t.Fatal(err)
		}
		again, err := Decode(doc2, zap.NewNop())
		if err != nil {
			t.Fatal(err)
		}
		check(t, again)
	})
	t.Run("json", func(t *testing.T) {
		doc, err := EncodeTestcaseJSON(tc, zap.NewNop())
		if err != nil {
			t.Fatal(err)
		}
		back, err := DecodeJSON(doc, zap.NewNop())
		if err != nil {
			t.Fatal(err)
		}
		check(t, back)
	})
}

// Noise cleared from a gRPC test case is not brought back from the noise
// assertion it was read with.
func TestAGrpcTestCaseWhoseNoiseWasClearedStaysClear(t *testing.T) {
	tc := unaryGrpcTestCase()
	tc.Assertions = map[models.AssertionType]interface{}{models.NoiseAssertion: map[string][]string{"body.3": {}}}
	tc.Noise = map[string][]string{}
	doc, err := EncodeTestcase(tc, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	back, err := Decode(doc, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	if len(back.Noise) != 0 {
		t.Errorf("cleared noise came back: %+v", back.Noise)
	}
}

// An HTTP test case's cleared noise stays clear too: it used to come back
// from the noise assertion it was read with.
func TestAnHTTPTestCaseWhoseNoiseWasClearedStaysClear(t *testing.T) {
	tc := models.TestCase{
		Version: models.V1Beta1, Kind: models.HTTP, Name: "h",
		HTTPReq:    models.HTTPReq{Method: "GET", URL: "http://x/a"},
		HTTPResp:   models.HTTPResp{StatusCode: 200, Body: `{"a":1}`},
		Assertions: map[models.AssertionType]interface{}{models.NoiseAssertion: map[string][]string{"body.a": {}}},
		Noise:      map[string][]string{},
	}
	doc, err := EncodeTestcase(tc, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	back, err := Decode(doc, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := back.Noise["body.a"]; ok {
		t.Errorf("cleared noise came back: %+v", back.Noise)
	}
}

// UpdateAssertions writes the noise it is given, for gRPC and HTTP alike: a
// noise entry among the assertions used to be dropped (gRPC) or ignored
// (when the file had noise).
func TestUpdateAssertionsWritesTheNoiseItIsGiven(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	db := New(zap.NewNop(), dir)
	tc := unaryGrpcTestCase()
	tc.Noise = map[string][]string{"body.9": {}}
	if err := db.InsertTestCase(ctx, &tc, "set-1", false); err != nil {
		t.Fatal(err)
	}
	given := map[models.AssertionType]interface{}{
		models.StatusCode:     0,
		models.NoiseAssertion: map[string][]string{"body.3": {}},
	}
	if err := db.UpdateAssertions(ctx, tc.Name, "set-1", given); err != nil {
		t.Fatal(err)
	}
	back, err := db.GetTestCase(ctx, "set-1", tc.Name)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := back.Noise["body.3"]; !ok {
		t.Errorf("the noise given was not written: %+v", back.Noise)
	}
	if _, ok := back.Noise["body.9"]; ok {
		t.Errorf("the noise the given assertions replaced is still there: %+v", back.Noise)
	}
	if _, ok := back.Assertions[models.StatusCode]; !ok {
		t.Errorf("status_code assertion not written: %+v", back.Assertions)
	}
}

// Noise given in Go as a generic map with []string values keeps its patterns.
func TestExpandAssertionsKeepsPatternsGivenAsStrings(t *testing.T) {
	tc := &models.TestCase{Assertions: map[models.AssertionType]interface{}{}, Noise: map[string][]string{}}
	expandAssertionsJSON(tc, map[models.AssertionType]interface{}{
		models.NoiseAssertion: map[string]interface{}{"body.ts": []string{`^\\d+$`}},
	})
	if got := tc.Noise["body.ts"]; len(got) != 1 || got[0] != `^\\d+$` {
		t.Fatalf("noise = %+v", tc.Noise)
	}
}

// UpdateAssertions given no noise keeps the case's: only assertions it names
// are replaced, noise among them.
func TestUpdateAssertionsWithNoNoiseKeepsTheCasesNoise(t *testing.T) {
	ctx := t.Context()
	db := New(zap.NewNop(), t.TempDir())
	tc := unaryGrpcTestCase()
	tc.Noise = map[string][]string{"body.9": {}}
	if err := db.InsertTestCase(ctx, &tc, "set-1", false); err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateAssertions(ctx, tc.Name, "set-1", map[models.AssertionType]interface{}{models.StatusCode: 0}); err != nil {
		t.Fatal(err)
	}
	back, err := db.GetTestCase(ctx, "set-1", tc.Name)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := back.Noise["body.9"]; !ok {
		t.Fatalf("the case's noise was dropped: %+v", back.Noise)
	}
}
