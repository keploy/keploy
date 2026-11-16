package config

import (
	"bytes"
	"testing"

	"github.com/spf13/viper"
	"go.keploy.io/server/v3/pkg/models"
)

// keploy.yml is read through viper, which lowercases map keys: a rule's
// match keys arrive lowercased (pathRegex as pathregex), and the rule must
// still validate and match as written.
func TestWorkerMockRulesReadThroughViperKeepTheirMeaning(t *testing.T) {
	src := `
record:
  workerMocks:
    - id: jwks-fetch
      kind: Http
      match:
        method: GET
        pathRegex: "^/\\.well-known/jwks\\.json$"
`
	v := viper.New()
	v.SetConfigType("yaml")
	if err := v.ReadConfig(bytes.NewBufferString(src)); err != nil {
		t.Fatal(err)
	}
	var c Config
	if err := v.Unmarshal(&c); err != nil {
		t.Fatal(err)
	}
	if len(c.Record.WorkerMocks) != 1 {
		t.Fatalf("rules %+v; want one", c.Record.WorkerMocks)
	}
	r := c.Record.WorkerMocks[0]
	if problems := models.ValidateWorkerMockRules(c.Record.WorkerMocks); len(problems) != 0 {
		t.Fatalf("a rule read through viper (match %v) does not validate: %v", r.Match, problems)
	}
	jwks := &models.Mock{Kind: models.HTTP, Spec: models.MockSpec{HTTPReq: &models.HTTPReq{Method: "GET", URL: "https://auth.example.com/.well-known/jwks.json"}}}
	if !r.Matches(jwks) {
		t.Fatalf("a rule read through viper (match %v) does not match the call it names", r.Match)
	}
}
