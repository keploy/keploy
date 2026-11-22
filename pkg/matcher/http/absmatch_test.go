package http

import (
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

func absTestCase(body string, noise map[string][]string) *models.TestCase {
	return &models.TestCase{
		Name:    "abs",
		HTTPReq: models.HTTPReq{Method: "GET", URL: "http://localhost:8080/x", Body: ""},
		HTTPResp: models.HTTPResp{
			StatusCode: 200,
			Header:     map[string]string{"Content-Type": "application/json"},
			Body:       body,
		},
		Noise: noise,
	}
}

// TestCompareHTTPResp_SectionedBodyNoise covers CompareHTTPResp, which had no
// test file at all. It shares the sectioned-noise contract with Match.
func TestCompareHTTPResp_SectionedBodyNoise(t *testing.T) {
	const recorded = `{"stock":99,"name":"Laptop"}`
	noise := func() map[string][]string { return map[string][]string{"body": {"stock"}} }

	t.Run("listed path is ignored", func(t *testing.T) {
		pass, _ := CompareHTTPResp(
			absTestCase(recorded, noise()),
			absTestCase(`{"stock":100,"name":"Laptop"}`, noise()),
			models.GlobalNoise{}, false, zap.NewNop())
		if !pass {
			t.Errorf("expected pass: only the listed noisy path differs")
		}
	})

	t.Run("unlisted field is still compared", func(t *testing.T) {
		pass, _ := CompareHTTPResp(
			absTestCase(recorded, noise()),
			absTestCase(`{"stock":100,"name":"Tablet"}`, noise()),
			models.GlobalNoise{}, false, zap.NewNop())
		if pass {
			t.Errorf("expected failure: name differs and is not noise")
		}
	})

	t.Run("empty section list still ignores the whole body", func(t *testing.T) {
		skip := func() map[string][]string { return map[string][]string{"body": {}} }
		pass, _ := CompareHTTPResp(
			absTestCase(recorded, skip()),
			absTestCase(`{"stock":1,"name":"totally-different"}`, skip()),
			models.GlobalNoise{}, false, zap.NewNop())
		if !pass {
			t.Errorf("an empty list on the bare key means ignore the whole body")
		}
	})
}

// TestCompareHTTPResp_SectionedNoise_NonJSONBody covers the non-JSON branch,
// which was the half of this function left on the old whole-body-skip semantics.
func TestCompareHTTPResp_SectionedNoise_NonJSONBody(t *testing.T) {
	noise := func() map[string][]string { return map[string][]string{"body": {"stock"}} }

	pass, _ := CompareHTTPResp(
		absTestCase("plain recorded text", noise()),
		absTestCase("plain REPLAYED text", noise()),
		models.GlobalNoise{}, false, zap.NewNop())
	if pass {
		t.Errorf("expected failure: a sectioned path list must not skip a non-JSON body")
	}
}

// TestCompareHTTPResp_DoesNotMutateSharedNoiseConfig pins that the caller's
// long-lived global noise config is not widened by a test case's own noise.
func TestCompareHTTPResp_DoesNotMutateSharedNoiseConfig(t *testing.T) {
	shared := models.GlobalNoise{"body": {}, "header": {}}

	noise := func() map[string][]string {
		return map[string][]string{"body": {"total"}, "body.stock": {}}
	}
	CompareHTTPResp(
		absTestCase(`{"total":1}`, noise()),
		absTestCase(`{"total":2}`, noise()),
		shared, false, zap.NewNop())

	if len(shared["body"]) != 0 {
		t.Fatalf("shared global noise config was mutated: %v", shared["body"])
	}

	// A later comparison declaring no noise must still catch the difference.
	pass, _ := CompareHTTPResp(
		absTestCase(`{"total":1}`, map[string][]string{}),
		absTestCase(`{"total":2}`, map[string][]string{}),
		shared, false, zap.NewNop())
	if pass {
		t.Errorf("noise from an earlier comparison leaked into a later one")
	}
}
