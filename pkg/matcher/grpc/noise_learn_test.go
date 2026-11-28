package grpc

import (
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

// A learned path whose first field the recorded message does not carry, that
// reaches an unknown field, or that steps past a map, Struct or Any (a key,
// not a field) cannot be translated by the replay. The opaque field itself
// can.
func TestTranslatablePaths(t *testing.T) {
	kept, dropped := TranslatablePaths(
		[]string{"servedAt", "order.total", "added", "order.#7", "labels", "labels.region", "order.Meta.k", "order.metadata"},
		`{"servedAt":1,"order":{"total":2,"meta":{"k":1},"metadata":"x"},"labels":{"region":"eu"}}`,
		[]string{"labels", "order.meta"}, true)
	if strings.Join(kept, ",") != "servedAt,order.total,labels,order.metadata" || strings.Join(dropped, ",") != "added,order.#7,labels.region,order.Meta.k" {
		t.Fatalf("kept %v dropped %v", kept, dropped)
	}
	// An incomplete list (a recursive schema): only top-level fields, which
	// can step past nothing.
	kept, dropped = TranslatablePaths([]string{"servedAt", "order.total"}, `{"servedAt":1,"order":{"total":2}}`, nil, false)
	if strings.Join(kept, ",") != "servedAt" || strings.Join(dropped, ",") != "order.total" {
		t.Fatalf("incomplete: kept %v dropped %v", kept, dropped)
	}
}

func TestNoiseBodies(t *testing.T) {
	decoded := &models.GrpcDecodeInfo{Expected: "release:a:1", Actual: "release:a:1", ExpectedJSON: `{"ts":1}`, ActualJSON: `{"ts":2}`}
	// A message's own length row differs with nearly every body change: it
	// is no message count.
	one := []models.BodyResult{{Type: models.GrpcLength, Expected: "3", Actual: "4"}, {Type: models.GrpcData}}
	for _, c := range []struct {
		name   string
		r      models.Result
		reason models.NoiseFailureReason
	}{
		{"decoded with one schema", models.Result{BodyResult: one, GrpcDecode: decoded}, ""},
		{"a stream", models.Result{BodyResult: []models.BodyResult{{Type: models.GrpcData}, {Type: models.GrpcData}}, GrpcDecode: decoded}, models.NoiseFailureUnresolved},
		{"not decoded", models.Result{BodyResult: one}, models.NoiseFailureNonJSONBody},
		{"two schemas", models.Result{BodyResult: one, GrpcDecode: &models.GrpcDecodeInfo{Expected: "release:a:1", Actual: "release:a:2", ExpectedJSON: "{}", ActualJSON: "{}"}}, models.NoiseFailureUnresolved},
		{"no schema named on either side", models.Result{BodyResult: one, GrpcDecode: &models.GrpcDecodeInfo{ExpectedJSON: "{}", ActualJSON: "{}"}}, models.NoiseFailureUnresolved},
		{"only the recorded side decoded", models.Result{BodyResult: one, GrpcDecode: &models.GrpcDecodeInfo{Expected: "release:a:1", Actual: "release:a:1", ExpectedJSON: "{}"}}, models.NoiseFailureNonJSONBody},
		{"only the replayed side decoded", models.Result{BodyResult: one, GrpcDecode: &models.GrpcDecodeInfo{Expected: "release:a:1", Actual: "release:a:1", ActualJSON: "{}"}}, models.NoiseFailureNonJSONBody},
		{"a decode that failed", models.Result{BodyResult: one, GrpcDecode: &models.GrpcDecodeInfo{Expected: "release:a:1", Actual: "release:a:1", Note: "could not decode"}}, models.NoiseFailureNonJSONBody},
		{"a count that differs", models.Result{BodyResult: []models.BodyResult{{Type: models.GrpcLength, Expected: "2 message(s)", Actual: "1 message(s)"}, {Type: models.GrpcData}}, GrpcDecode: decoded}, models.NoiseFailureUnresolved},
		{"a count noise excused", models.Result{BodyResult: []models.BodyResult{{Normal: true, Type: models.GrpcLength, Expected: "2 message(s)", Actual: "1 message(s)"}, {Type: models.GrpcData}}, GrpcDecode: decoded}, ""},
		{"assertions replaced the comparison", models.Result{BodyResult: []models.BodyResult{{Type: models.GrpcData, NotCompared: true}}, GrpcDecode: decoded}, models.NoiseFailureUnresolved},
		{"assertions replaced the comparison, no schema", models.Result{BodyResult: []models.BodyResult{{Type: models.GrpcData, NotCompared: true}}}, models.NoiseFailureUnresolved},
	} {
		t.Run(c.name, func(t *testing.T) {
			exp, act, why, reason := NoiseBodies(c.r)
			if reason != c.reason {
				t.Fatalf("reason = %q (%s), want %q", reason, why, c.reason)
			}
			if c.reason == "" && (exp != `{"ts":1}` || act != `{"ts":2}`) {
				t.Fatalf("bodies %q %q", exp, act)
			}
			if c.reason != "" && why == "" {
				t.Fatal("a refusal says no why")
			}
		})
	}
}
