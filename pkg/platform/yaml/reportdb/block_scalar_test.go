package reportdb

import (
	"context"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// A report whose replayed body starts with a line break (Django's 500 page),
// a space or a tab reads back. yaml.v3's emitter wrote such a value, inside the
// report's tests list, under an indentation indicator that named the wrong
// column (and dropped a leading line break), so GetReport failed ("did not find
// expected key"): a cloud replay then could not judge its run, and uploaded no
// report. Fixed in keploy/yaml, which go.mod's replace points gopkg.in/yaml.v3
// at.
func TestGetReportReadsBackBodiesStartingWithABreakSpaceOrTab(t *testing.T) {
	bodies := map[string]string{
		"django 500": "\n<!doctype html>\n<html lang=\"en\">\n<head>\n  <title>Server Error (500)</title>\n</head>\n<body>\n  <h1>Server Error (500)</h1><p></p>\n</body>\n</html>\n",
		"space":      "  indented\nnext\n",
		"tab":        "\t{\n\t\t\"a\": 1\n\t}\n",
	}
	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			rep := &models.TestReport{Version: "api.keploy.io/v1beta1", Status: "FAILED", Total: 1, Failure: 1, TestSet: "test-set-0",
				Tests: []models.TestResult{{
					Kind: models.HTTP, Name: "test-set-0", Status: models.TestStatusFailed, TestCaseID: "get-orders-1",
					Res: models.HTTPResp{StatusCode: 500, Body: body, Header: map[string]string{"Content-Type": "text/html"}},
					Result: models.Result{BodyResult: []models.BodyResult{{Normal: false, Type: models.Plain,
						Expected: `{"orders":[]}`, Actual: body}}},
				}}}
			dir := t.TempDir()
			db := New(zap.NewNop(), dir)
			if err := db.InsertReport(context.Background(), "test-run-0", "test-set-0", rep); err != nil {
				t.Fatalf("InsertReport: %v", err)
			}
			got, err := db.GetReport(context.Background(), "test-run-0", "test-set-0")
			if err != nil {
				t.Fatalf("keploy cannot read back the report it wrote: %v", err)
			}
			if len(got.Tests) != 1 || got.Tests[0].Res.Body != body || got.Tests[0].Result.BodyResult[0].Actual != body {
				t.Fatalf("the body did not round-trip:\n got %q\nwant %q", got.Tests[0].Result.BodyResult[0].Actual, body)
			}
		})
	}
}
