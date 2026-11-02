package mockdb

import (
	"bytes"
	"context"
	"encoding/gob"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml"
	"go.uber.org/zap"
	yamlLib "gopkg.in/yaml.v3"
)

// A fault on an HTTP mock is a `fault:` block of its spec, written legibly
// ("delay: 1.5s"), and read back as written through the YAML document, the
// JSON document and the JSON mock form. A mock without one writes no such
// key, so a recording without faults reads and writes as before.
func TestAFaultRoundTrips(t *testing.T) {
	for _, want := range []models.Fault{
		{Kind: models.FaultTruncate, BodyBytes: 10, Observed: "unexpected EOF"},
		{Kind: models.FaultReset, AfterHead: true, BodyBytes: 3},
		{Kind: models.FaultDelay, Delay: 1500 * time.Millisecond},
	} {
		m := variablesMock()
		f := want
		m.Spec.Fault = &f
		doc, err := EncodeMock(m, zap.NewNop())
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		var buf bytes.Buffer
		enc := yamlLib.NewEncoder(&buf)
		if err := enc.Encode(doc); err != nil {
			t.Fatal(err)
		}
		_ = enc.Close()
		text := buf.String()
		if !strings.Contains(text, "    fault:\n        kind: "+string(want.Kind)+"\n") {
			t.Fatalf("the fault is not a legible block of the spec:\n%s", text)
		}
		if want.Delay != 0 && !strings.Contains(text, "delay: 1.5s") {
			t.Fatalf("the delay is not written as a duration a person reads:\n%s", text)
		}
		var back yaml.NetworkTrafficDoc
		if err := yamlLib.Unmarshal(buf.Bytes(), &back); err != nil {
			t.Fatal(err)
		}
		out, err := DecodeMocks([]*yaml.NetworkTrafficDoc{&back}, zap.NewNop())
		if err != nil || len(out) != 1 || out[0].Spec.Fault == nil || *out[0].Spec.Fault != want {
			t.Fatalf("yaml round-trip: err=%v %+v, want %+v", err, out, want)
		}

		jsonBytes, err := yaml.MarshalDoc(yaml.FormatJSON, doc)
		if err != nil {
			t.Fatalf("MarshalDoc(json): %v", err)
		}
		jdoc, err := yaml.UnmarshalDoc(yaml.FormatJSON, jsonBytes)
		if err != nil {
			t.Fatalf("UnmarshalDoc(json): %v", err)
		}
		jout, err := DecodeMocks([]*yaml.NetworkTrafficDoc{jdoc}, zap.NewNop())
		if err != nil || len(jout) != 1 || jout[0].Spec.Fault == nil || *jout[0].Spec.Fault != want {
			t.Fatalf("json document round-trip: err=%v %+v, want %+v", err, jout, want)
		}

		jm, ok, err := EncodeMockJSON(m, zap.NewNop())
		if err != nil || !ok {
			t.Fatalf("EncodeMockJSON: ok=%v err=%v", ok, err)
		}
		jmout, err := DecodeMocksJSON([]*yaml.NetworkTrafficDocJSON{jm}, zap.NewNop())
		if err != nil || len(jmout) != 1 || jmout[0].Spec.Fault == nil || *jmout[0].Spec.Fault != want {
			t.Fatalf("json mock round-trip: err=%v %+v, want %+v", err, jmout, want)
		}
	}

	m := variablesMock()
	doc, err := EncodeMock(m, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	enc := yamlLib.NewEncoder(&buf)
	if err := enc.Encode(doc); err != nil {
		t.Fatal(err)
	}
	_ = enc.Close()
	if strings.Contains(buf.String(), "fault") {
		t.Fatalf("a mock without a fault wrote the key:\n%s", buf.String())
	}
	jm, _, err := EncodeMockJSON(m, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(jm.Spec, []byte("fault")) {
		t.Fatalf("a mock without a fault wrote the key in its JSON form: %s", jm.Spec)
	}
}

// A mock whose fault this keploy cannot enact (a kind from a newer keploy, a
// spelling the format does not have, a fault on a call of another protocol)
// is skipped by the set's read, in YAML, NDJSON and gob alike, and named with
// why (MockSkipFault): never served as its head or as a whole answer. A fault
// this keploy enacts is served.
func TestAFaultThisKeployCannotEnactIsSkippedAndNamed(t *testing.T) {
	cases := map[yaml.Format]string{
		yaml.FormatYAML: httpDocYAML("fault-newer", "Http", "a") + "    fault:\n        kind: explode\n" +
			"---\n" + httpDocYAML("fault-misspelt", "Http", "b") + "    fault:\n        kind: eof\n        after_head: true\n" +
			"---\n" + httpDocYAML("fault-ok", "Http", "c") + "    fault:\n        kind: eof\n",
		yaml.FormatJSON: strings.TrimSuffix(httpDocJSON("fault-newer", "Http", "a"), "}}") + `,"fault":{"kind":"explode"}}}` + "\n" +
			strings.TrimSuffix(httpDocJSON("fault-misspelt", "Http", "b"), "}}") + `,"fault":{"kind":"eof","after_head":true}}}` + "\n" +
			strings.TrimSuffix(httpDocJSON("fault-ok", "Http", "c"), "}}") + `,"fault":{"kind":"eof"}}}` + "\n",
	}
	for format, body := range cases {
		t.Run(string(format), func(t *testing.T) {
			t.Setenv("KEPLOY_MOCK_FORMAT", "")
			dir := t.TempDir()
			writeSetFile(t, dir, format, body)
			served, listed := readSetOracle(t, dir, format)
			if !served["fault-ok"] || served["fault-newer"] || served["fault-misspelt"] {
				t.Fatalf("the set's read served %v; want only the fault this keploy enacts", served)
			}
			if listed["fault-newer"] != models.HTTP || listed["fault-misspelt"] != models.HTTP {
				t.Fatalf("the set's read lists as skipped %v", listed)
			}
			for _, d := range readDocs(t, dir, format) {
				want := models.MockSkip("")
				if d.Name != "fault-ok" {
					want = models.MockSkipFault
				}
				if d.Skip != want {
					t.Errorf("the reader says %s is %q, want %q", d.Name, d.Skip, want)
				}
			}
			_, skipped := NewWithFormat(zap.NewNop(), dir, "mocks", format).SkippedMocks(context.Background(), pruneSet)
			if skipped["fault-newer"].Why != models.MockSkipFault || skipped["fault-misspelt"].Why != models.MockSkipFault || len(skipped) != 2 {
				t.Errorf("SkippedMocks: %v", skipped)
			}
		})
	}

	t.Run("gob", func(t *testing.T) {
		newer, other, ok, unnamed := pruneHTTPMock(0), pruneHTTPMock(1), pruneHTTPMock(2), pruneHTTPMock(3)
		newer.Name, other.Name, ok.Name, unnamed.Name = "fault-newer", "fault-other-protocol", "fault-ok", ""
		newer.Spec.Fault = &models.Fault{Kind: "explode"}
		unnamed.Spec.Fault = &models.Fault{Kind: "explode"} // skipped, and named nowhere: it has no name
		other.Kind = models.DNS
		other.Spec.Fault = &models.Fault{Kind: models.FaultEOF}
		ok.Spec.Fault = &models.Fault{Kind: models.FaultEOF}
		dir := t.TempDir()
		if err := os.MkdirAll(filepath.Join(dir, pruneSet), 0o755); err != nil {
			t.Fatal(err)
		}
		var buf bytes.Buffer
		buf.WriteString(gobMockMagic)
		enc := gob.NewEncoder(&buf)
		for _, m := range []*models.Mock{newer, other, ok, unnamed} {
			if err := enc.Encode(m); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(filepath.Join(dir, pruneSet, "mocks.gob"), buf.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
		served, listed := readSetOracle(t, dir, yaml.FormatYAML)
		if !served["fault-ok"] || served["fault-newer"] || served["fault-other-protocol"] {
			t.Fatalf("the set's read served %v; want only the fault this keploy enacts", served)
		}
		if listed["fault-newer"] != models.HTTP || listed["fault-other-protocol"] != models.DNS {
			t.Fatalf("the set's read lists as skipped %v", listed)
		}
		_, skipped := NewWithFormat(zap.NewNop(), dir, "mocks", yaml.FormatYAML).SkippedMocks(context.Background(), pruneSet)
		if skipped["fault-newer"].Why != models.MockSkipFault || skipped["fault-other-protocol"].Why != models.MockSkipFault || len(skipped) != 2 {
			t.Errorf("SkippedMocks: %v", skipped)
		}
	})

	t.Run("json mock form", func(t *testing.T) {
		m := variablesMock()
		m.Spec.Fault = &models.Fault{Kind: "explode"}
		jm, ok, err := EncodeMockJSON(m, zap.NewNop())
		if err != nil || !ok {
			t.Fatalf("EncodeMockJSON: ok=%v err=%v", ok, err)
		}
		out, err := DecodeMocksJSON([]*yaml.NetworkTrafficDocJSON{jm}, zap.NewNop())
		if err != nil || len(out) != 0 {
			t.Fatalf("DecodeMocksJSON = %d mocks, %v; want the mock skipped", len(out), err)
		}
	})
}
