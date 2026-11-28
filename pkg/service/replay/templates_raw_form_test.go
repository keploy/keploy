package replay

import (
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/templates"
)

func TestTestRequestOfPropagatesRawFormBody(t *testing.T) {
	run := &templates.Run{}
	for _, want := range []bool{false, true} {
		tc := &models.TestCase{
			Name: "post-form",
			Kind: models.HTTP,
			HTTPReq: models.HTTPReq{
				Method:      "POST",
				URL:         "http://localhost:8080/submit",
				Header:      map[string]string{"Content-Type": "application/x-www-form-urlencoded"},
				Body:        "a=1%262",
				RawFormBody: want,
			},
		}
		got := testRequestOf(run, tc, "")
		if got == nil {
			t.Fatalf("testRequestOf(RawFormBody=%v) = nil", want)
		}
		if got.RawFormBody != want {
			t.Fatalf("testRequestOf(RawFormBody=%v).RawFormBody = %v; want %v", want, got.RawFormBody, want)
		}
	}
}
