package mockdb

import (
	"bytes"
	"reflect"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml"
	"go.uber.org/zap"
	yamlLib "gopkg.in/yaml.v3"
)

// The places the learn step learned as a read of the application's clock
// (models.MockSpec.Clocks: a plain read, and one plus thirty minutes) are a
// top-level `clocks:` block of the document, written in durations a person
// reads ("res: 1ms", "min: 29m59.99s"), and they survive a YAML and a JSON
// round trip with the recorded bytes unchanged. A mock without them writes
// no such key, so a recording without the field reads and writes as before.
func TestAClocksBlockRoundTrips(t *testing.T) {
	m := variablesMock()
	m.Spec.Clocks = map[string]models.ClockOffset{
		"body.createdAt": {Res: time.Millisecond},
		"body.expiresAt": {Min: 30*time.Minute - 10*time.Millisecond, Max: 30*time.Minute + 5*time.Millisecond, Res: time.Millisecond},
	}
	want := map[string]models.ClockOffset{}
	for k, v := range m.Spec.Clocks {
		want[k] = v
	}
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
	if !strings.Contains(text, "\nclocks:\n") || !strings.Contains(text, "res: 1ms") || !strings.Contains(text, "min: 29m59.99s") {
		t.Fatalf("the block is not a legible top-level key of the document:\n%s", text)
	}
	var back yaml.NetworkTrafficDoc
	if err := yamlLib.Unmarshal(buf.Bytes(), &back); err != nil {
		t.Fatal(err)
	}
	out, err := DecodeMocks([]*yaml.NetworkTrafficDoc{&back}, zap.NewNop())
	if err != nil || len(out) != 1 {
		t.Fatalf("decode: err=%v n=%d", err, len(out))
	}
	if !reflect.DeepEqual(out[0].Spec.Clocks, want) {
		t.Fatalf("yaml round-trip: %+v, want %+v", out[0].Spec.Clocks, want)
	}
	if out[0].Spec.HTTPReq.Body != m.Spec.HTTPReq.Body || out[0].Spec.HTTPResp.Body != m.Spec.HTTPResp.Body {
		t.Fatal("the recorded bytes changed")
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
	if err != nil || len(jout) != 1 || !reflect.DeepEqual(jout[0].Spec.Clocks, want) {
		t.Fatalf("json round-trip: err=%v %+v, want %+v", err, jout, want)
	}

	m.Spec.Clocks = nil
	doc, err = EncodeMock(m, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	buf.Reset()
	enc = yamlLib.NewEncoder(&buf)
	if err := enc.Encode(doc); err != nil {
		t.Fatal(err)
	}
	_ = enc.Close()
	if strings.Contains(buf.String(), "clocks") {
		t.Fatalf("a mock without clocks wrote the key:\n%s", buf.String())
	}
}
