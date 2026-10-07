package mocknoise

import (
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

func mockWithBodies(reqBody, respBody string) *models.Mock {
	return &models.Mock{
		Kind: models.HTTP,
		Spec: models.MockSpec{
			HTTPReq:  &models.HTTPReq{Body: reqBody},
			HTTPResp: &models.HTTPResp{Body: respBody},
		},
	}
}

// TestCorrelationsFromMock pins the static echo detector: an app-random value
// sent in the request and reflected in the response becomes a correlation,
// while static/enum/integer/decimal values and request-only values do not.
func TestCorrelationsFromMock(t *testing.T) {
	const uuid = "550e8400-e29b-41d4-a716-446655440000"
	for _, tc := range []struct {
		name      string
		req, resp string
		wantPath  string // "" = expect no correlation
		wantClass string
	}{
		{"echoed uuid", `{"idempotencyKey":"` + uuid + `","amount":10}`, `{"status":"ok","idempotencyKey":"` + uuid + `"}`, "body.idempotencyKey", "uuid"},
		{"echoed long hex token", `{"token":"deadbeefcafe1234"}`, `{"echo":"deadbeefcafe1234"}`, "body.token", "hex"},
		{"echoed mixed nonce", `{"nonce":"aZ09kQ7xP2mN4rT8vB1c"}`, `{"seen":"aZ09kQ7xP2mN4rT8vB1c"}`, "body.nonce", "nonce"},
		{"uuid in request only, not echoed", `{"idempotencyKey":"` + uuid + `"}`, `{"status":"ok"}`, "", ""},
		{"static enum echoed is not random", `{"status":"ACTIVE"}`, `{"status":"ACTIVE"}`, "", ""},
		{"small int echoed is not random", `{"id":42}`, `{"id":42}`, "", ""},
		{"long decimal id echoed is not a hex token", `{"ts":"1700000000000000"}`, `{"ts":"1700000000000000"}`, "", ""},
		{"non-JSON bodies", `not json`, `also not json`, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := CorrelationsFromMock(mockWithBodies(tc.req, tc.resp))
			if tc.wantPath == "" {
				if len(got) != 0 {
					t.Fatalf("want no correlation, got %+v", got)
				}
				return
			}
			if len(got) != 1 {
				t.Fatalf("want exactly one correlation, got %+v", got)
			}
			c := got[0]
			if c.RequestPath != tc.wantPath {
				t.Fatalf("RequestPath = %q, want %q", c.RequestPath, tc.wantPath)
			}
			if c.ValueClass != tc.wantClass {
				t.Fatalf("ValueClass = %q, want %q", c.ValueClass, tc.wantClass)
			}
			if len(c.ResponsePaths) == 0 {
				t.Fatalf("correlation has no response paths: %+v", c)
			}
			if c.RecordedValue == "" {
				t.Fatalf("correlation has no recorded value: %+v", c)
			}
		})
	}
}
