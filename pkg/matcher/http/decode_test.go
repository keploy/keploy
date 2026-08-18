package http

import (
	"os"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/platform/yaml"
	"go.keploy.io/server/v3/pkg/platform/yaml/testdb"
	"go.uber.org/zap"
	yamlv3 "gopkg.in/yaml.v3"
)

// TestMatch_DecodedFromYAML_SectionedNoise runs a recorded test-case file
// through the production YAML decoder and then through Match, so the whole
// path — decode, SplitNoise, compare — is covered rather than a hand-built
// TestCase. The fixture is a verbatim trim of a real recorded HTTP test case,
// including its indexed noise path.
func TestMatch_DecodedFromYAML_SectionedNoise(t *testing.T) {
	const fixture = `version: api.keploy.io/v1beta1
kind: Http
name: get-order-details
spec:
  metadata: {}
  req:
    method: GET
    proto_major: 1
    proto_minor: 1
    url: http://localhost:8080/api/v1/orders/5978a5ae/details
    header:
      Accept: '*/*'
    body: ""
    timestamp: 2026-08-05T08:22:51.125846719Z
  resp:
    status_code: 200
    header:
      Content-Type: application/json; charset=utf-8
    body: '{"created_at":"2026-08-05T08:22:51Z","items":[{"product":{"name":"Laptop","price":1200,"stock":99}}]}'
    status_message: OK
    proto_major: 0
    proto_minor: 0
    timestamp: 2026-08-05T08:22:51.131208762Z
  objects: []
  assertions:
    noise:
      body:
        - items.0.product.stock
      body.created_at: []
      header:
        - Content-Length
      header.Date: []
  created: 1785918171
  app_port: 8080
`
	path := t.TempDir() + "/tc.yaml"
	if err := os.WriteFile(path, []byte(fixture), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	var doc yaml.NetworkTrafficDoc
	if err := yamlv3.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	tc, err := testdb.Decode(&doc, zap.NewNop())
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}

	// Sanity-check that the decoder really produced the sectioned shape; if it
	// stops doing so, this test would silently stop covering the bug.
	if got := tc.Noise["body"]; len(got) != 1 || got[0] != "items.0.product.stock" {
		t.Fatalf("decoded noise[body] = %v, want the sectioned path list", got)
	}

	// An indexed path is what recordings made before the producer fix carry, and
	// it matches nothing: the JSON walker keys array elements without a position,
	// so the effective path is "items.product.stock".
	//
	// The matcher deliberately still does not rewrite it. Dropping numeric
	// segments from the STRING is unsafe — a string cannot tell an array position
	// from a genuinely numeric object key, so "data.2026.count" would be
	// corrupted into "data.count", which names a different field. See
	// TestJSONDiffWithNoiseControl_IndexedPathIsNotSupported.
	//
	// The fix therefore lives on the producing side: matcher.BodyNoiseFromJSONDiff
	// derives paths by walking the two documents in step with this walker, so it
	// distinguishes the two cases because it is looking at the document rather
	// than at a string. Such a field keeps failing loudly here instead of passing
	// silently, and matcher.WarnUnmatchableBodyNoise names the dead entry in the
	// log — which is the upgrade path for suites recorded before that fix.
	t.Run("indexed noise path does not silence its own field", func(t *testing.T) {
		actual := tc.HTTPResp
		actual.Body = strings.Replace(tc.HTTPResp.Body, `"stock":99`, `"stock":100`, 1)
		pass, _ := Match(tc, &actual, map[string]map[string][]string{}, false, false, zap.NewNop(), false)
		if pass {
			t.Errorf("if this now passes, indexed noise paths became supported; " +
				"update SplitNoise's docs and this test")
		}
	})

	t.Run("index-free path is honoured end to end", func(t *testing.T) {
		tc2, err := testdb.Decode(&doc, zap.NewNop())
		if err != nil {
			t.Fatalf("Decode: %v", err)
		}
		tc2.Noise["body"] = []string{"items.product.stock"}
		actual := tc2.HTTPResp
		actual.Body = strings.Replace(tc2.HTTPResp.Body, `"stock":99`, `"stock":100`, 1)
		pass, _ := Match(tc2, &actual, map[string]map[string][]string{}, false, false, zap.NewNop(), false)
		if !pass {
			t.Errorf("expected pass: only the listed noisy path differs")
		}
	})

	t.Run("real regression is caught", func(t *testing.T) {
		tc2, err := testdb.Decode(&doc, zap.NewNop())
		if err != nil {
			t.Fatalf("Decode: %v", err)
		}
		actual := tc2.HTTPResp
		actual.Body = strings.NewReplacer(
			`"stock":99`, `"stock":100`,
			`"name":"Laptop"`, `"name":"Tablet"`,
			`"price":1200`, `"price":1999`,
		).Replace(tc2.HTTPResp.Body)
		pass, _ := Match(tc2, &actual, map[string]map[string][]string{}, false, false, zap.NewNop(), false)
		if pass {
			t.Errorf("expected failure: name and price differ and are not noise")
		}
	})
}
