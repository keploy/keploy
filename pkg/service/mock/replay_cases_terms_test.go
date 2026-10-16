package mock

import (
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

// A recorded answer is compared in the run's terms: each variable's recorded
// value is written as the value it has in this run, whole words only, on a
// copy.
func TestInRunTerms(t *testing.T) {
	tc := &models.TestCase{HTTPResp: models.HTTPResp{Body: `{"id":"rec-000001","owner":"USER:rec-000001","note":"xrec-000001","at":1792000000}`, Header: map[string]string{"Location": "/apps/rec-000001"}}}
	got := inRunTerms(tc, []models.VariableBinding{
		{Name: "id", Value: "live-7f3a9c", Recorded: "rec-000001"},
		{Name: "at", Value: "1792555000", Recorded: "1792000000"},
		{Name: "n", Value: "12", Recorded: "13"},
	})
	if want := `{"id":"live-7f3a9c","owner":"USER:live-7f3a9c","note":"xrec-000001","at":1792555000}`; got.HTTPResp.Body != want {
		t.Fatalf("body %s, want %s", got.HTTPResp.Body, want)
	}
	if got.HTTPResp.Header["Location"] != "/apps/live-7f3a9c" {
		t.Fatalf("header %s", got.HTTPResp.Header["Location"])
	}
	if tc.HTTPResp.Header["Location"] != "/apps/rec-000001" || tc.HTTPResp.Body[7:17] != "rec-000001" {
		t.Fatal("the recorded case was changed")
	}
}
