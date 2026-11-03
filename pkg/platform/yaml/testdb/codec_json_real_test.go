package testdb

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// A JSON test case written through the codec, as a writer outside this store
// writes one (EncodeTestcase, then yaml.MarshalDocIndent with FormatJSON),
// and read back by the store's own JSON read keeps real: under the proven
// switch.
func TestJSONTestCaseWrittenThroughTheCodecKeepsReal(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	logger := zap.NewNop()
	tc := models.TestCase{
		Version: models.GetVersion(), Kind: models.HTTP, Name: "test-1",
		HTTPReq:  models.HTTPReq{Method: "GET", URL: "http://localhost:8080/orders", Header: map[string]string{"Accept": "*/*"}},
		HTTPResp: models.HTTPResp{StatusCode: 200, Header: map[string]string{"Content-Type": "application/json"}, Body: "{}"},
		Real:     []string{"localhost:5432", "auth.example.com:443"},
	}
	doc, err := EncodeTestcase(tc, logger)
	if err != nil {
		t.Fatal(err)
	}
	data, err := yaml.MarshalDocIndent(yaml.FormatJSON, doc)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	tests := filepath.Join(dir, "test-set-0", "tests")
	if err := os.MkdirAll(tests, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tests, "test-1.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := NewWithFormat(logger, dir, yaml.FormatJSON).GetTestCase(context.Background(), "test-set-0", "test-1")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Real, tc.Real) {
		t.Errorf("Real = %#v, want %#v", got.Real, tc.Real)
	}
}
