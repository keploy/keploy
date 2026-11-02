package mockdb

import (
	"bytes"
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
