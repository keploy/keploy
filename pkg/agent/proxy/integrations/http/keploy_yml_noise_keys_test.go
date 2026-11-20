package http

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/viper"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/config/loader"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// keploy.yml's url and requestbody entries clear the calls they name when
// keploy reads keploy.yml as the CLI does (loader.Files, PreProcessFlags):
// a url regex with an upper-case segment and an escaped dot, and dotted
// requestbody paths, nested and through an array. Each is only what it
// names: the same path in lower case, a path the escaped dot does not
// match, and a body field no entry names still differ. Before, keploy read
// "^/api/Users/[0-9]+$" as "^/api/users/[0-9]+$", which cleared no
// /api/Users call, and refused this keploy.yml for its dotted keys. Under
// the proven switch, where a call is served only a recording it matches
// outside noise. Mutation: read keploy.yml's noise through viper.
func TestKeployYmlNoiseKeysWithCaseAndDotsClearTheirCalls(t *testing.T) {
	dir := t.TempDir()
	yml := `test:
  globalNoise:
    global:
      url: {"^/api/Users/[0-9]+$": [], "^/v1\\.2/files/[^/]+$": []}
      requestbody: {"meta.ts": [], "items[].ts": []}
`
	if err := os.WriteFile(filepath.Join(dir, "keploy.yml"), []byte(yml), 0o600); err != nil {
		t.Fatal(err)
	}
	v := viper.New()
	v.SetConfigType("yml")
	files := loader.New(v)
	if err := files.Read(loader.Find(dir)); err != nil {
		t.Fatalf("read keploy.yml: %v", err)
	}
	cfg := config.New()
	if err := files.Unmarshal(cfg); err != nil {
		t.Fatalf("unmarshal keploy.yml: %v", err)
	}
	noise := NewRequestNoise(zap.NewNop(), models.OutgoingOptions{NoiseConfig: cfg.Test.GlobalNoise.Global})
	ct := map[string]string{"Content-Type": "application/json"}
	for _, c := range []struct {
		name, recPath, recBody, path, body string
		served                             bool
	}{
		{"an id in a path with upper case", "/api/Users/1", `{"q":"a"}`, "/api/Users/3", `{"q":"a"}`, true},
		{"the same path in lower case", "/api/users/1", `{"q":"a"}`, "/api/users/3", `{"q":"a"}`, false},
		{"a file under an escaped dot", "/v1.2/files/a.pdf", `{"q":"a"}`, "/v1.2/files/b.pdf", `{"q":"a"}`, true},
		{"a path the escaped dot does not match", "/v1x2/files/a.pdf", `{"q":"a"}`, "/v1x2/files/b.pdf", `{"q":"a"}`, false},
		{"a nested field", "/s", `{"meta":{"ts":1},"q":"a"}`, "/s", `{"meta":{"ts":2},"q":"a"}`, true},
		{"a field in an array of objects", "/s", `{"items":[{"ts":4}],"q":"a"}`, "/s", `{"items":[{"ts":5}],"q":"a"}`, true},
		{"a field no entry names", "/s", `{"meta":{"ts":1},"q":"a"}`, "/s", `{"meta":{"ts":1},"q":"b"}`, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, diag := strictMatch(t, sent(c.path, ct, c.body), nil, noise.Body, noise.URL, recorded("m", c.recPath, ct, c.recBody))
			if (got != nil) != c.served {
				t.Fatalf("served %v (differs %v); want %v", got != nil, differs(diag), c.served)
			}
		})
	}
}
