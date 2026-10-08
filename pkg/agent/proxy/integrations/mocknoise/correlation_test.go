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

// TestMaterializeCorrelations pins the replay-ingest helper: it populates an
// echo mock's Correlations, leaves a no-echo mock empty, and never clobbers a
// mock that already carries correlations.
func TestMaterializeCorrelations(t *testing.T) {
	const id = "550e8400-e29b-41d4-a716-446655440000"

	echo := mockWithBodies(`{"idempotencyKey":"`+id+`"}`, `{"ok":true,"idempotencyKey":"`+id+`"}`)
	MaterializeCorrelations(echo)
	if len(echo.Spec.Correlations) != 1 || echo.Spec.Correlations[0].RequestPath != "body.idempotencyKey" {
		t.Fatalf("materialize should populate the echo correlation, got %+v", echo.Spec.Correlations)
	}

	none := mockWithBodies(`{"amount":10}`, `{"ok":true}`)
	MaterializeCorrelations(none)
	if len(none.Spec.Correlations) != 0 {
		t.Fatalf("no echo must stay empty, got %+v", none.Spec.Correlations)
	}

	pre := mockWithBodies(`{"idempotencyKey":"`+id+`"}`, `{"idempotencyKey":"`+id+`"}`)
	pre.Spec.Correlations = []models.FieldCorrelation{{RequestPath: "pre", RecordedValue: "x"}}
	MaterializeCorrelations(pre)
	if len(pre.Spec.Correlations) != 1 || pre.Spec.Correlations[0].RequestPath != "pre" {
		t.Fatalf("already-populated must be left unchanged, got %+v", pre.Spec.Correlations)
	}
}

// ReplaceWords replaces whole words only, and returns its input when it
// replaces nothing.
func TestReplaceWords(t *testing.T) {
	const rec, live = "0f8fad5b-d9cb-469f-a165-70867728950e", "7c9e6679-7425-40de-944b-e07fc1f90ae7"
	lookup := func(w string) (string, bool) {
		if w == rec {
			return live, true
		}
		return "", false
	}
	for in, want := range map[string]string{
		`{"id":"` + rec + `"}`:               `{"id":"` + live + `"}`,
		"/orders/" + rec + "/items?x=" + rec: "/orders/" + live + "/items?x=" + live,
		rec:                                  live,
		"order-" + rec:                       "order-" + rec, // a longer word
		rec + "_thumb":                       rec + "_thumb",
		rec + "." + rec:                      live + "." + live,
		"no ids here":                        "no ids here",
		"":                                   "",
	} {
		if got := ReplaceWords(in, lookup); got != want {
			t.Errorf("ReplaceWords(%q) = %q, want %q", in, got, want)
		}
	}
	var words []string
	s := "a " + rec + " short1234 0123456789abcdef"
	ScanWords(s, func(start, end int) { words = append(words, s[start:end]) })
	if len(words) != 2 || words[0] != rec || words[1] != "0123456789abcdef" {
		t.Fatalf("ScanWords found %q", words)
	}
}
