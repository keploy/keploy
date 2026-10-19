package replay

import (
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// Under the proven switch --schema-match does not loosen the comparison:
// every response body is compared value by value (design v14 U18 deletes the
// setting at the flip), and the setting is said once to be ignored. A schema
// match passes a changed value, an added field and a reorder of same-shaped
// elements; each of those must fail its test under the switch. Without the
// switch --schema-match compares the schema, as before.
func TestSchemaMatchIsIgnoredUnderTheProvenSwitch(t *testing.T) {
	expected := func() *models.TestCase {
		return &models.TestCase{Name: "get-items", HTTPResp: models.HTTPResp{
			StatusCode: 200,
			Header:     map[string]string{"Content-Type": "application/json"},
			Body:       `{"total":2,"items":[{"id":1,"name":"a"},{"id":2,"name":"b"}]}`,
		}}
	}
	for _, tc := range []struct{ name, body string }{
		{"a value that changed", `{"total":3,"items":[{"id":1,"name":"a"},{"id":2,"name":"b"}]}`},
		{"a field that was added", `{"total":2,"items":[{"id":1,"name":"a"},{"id":2,"name":"b"}],"next":"x"}`},
		{"a reorder of same-shaped elements", `{"total":2,"items":[{"id":2,"name":"b"},{"id":1,"name":"a"}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{}
			cfg.Test.SchemaMatch = true
			actual := func() *models.HTTPResp {
				return &models.HTTPResp{StatusCode: 200, Header: map[string]string{"Content-Type": "application/json"}, Body: tc.body}
			}

			t.Setenv(provenswitch.Env, "0")
			r := &Replayer{logger: zap.NewNop(), config: cfg}
			if pass, _ := r.compareHTTPResp(expected(), actual(), "set", false); !pass {
				t.Fatal("control: without the switch --schema-match passes it, as before")
			}

			t.Setenv(provenswitch.Env, "1")
			core, logs := observer.New(zapcore.WarnLevel)
			r = &Replayer{logger: zap.New(core), config: cfg}
			for _, compare := range []func() bool{
				func() bool { pass, _ := r.compareHTTPResp(expected(), actual(), "set", false); return pass },
				func() bool { pass, _ := r.compareHTTPRespForReplay(expected(), actual(), "set", true); return pass },
			} {
				if compare() {
					t.Fatal("KEPLOY_PROVEN=1: --schema-match passed a response that differs")
				}
			}
			if n := logs.FilterMessage("schemaMatch (--schema-match) is ignored under the proven model: every response body is compared, value by value").Len(); n != 1 {
				t.Fatalf("the ignored setting was said %d times, want once", n)
			}
		})
	}
}
