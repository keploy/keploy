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

func variablesMock() *models.Mock {
	return &models.Mock{
		Version: models.GetVersion(),
		Kind:    models.HTTP,
		Name:    "mock-1",
		Spec: models.MockSpec{
			Metadata: map[string]string{"name": "Http", "operation": "POST"},
			HTTPReq: &models.HTTPReq{Method: "POST", URL: "http://store/orders", ProtoMajor: 1, ProtoMinor: 1,
				Header: map[string]string{"Content-Type": "application/json"}, Body: `{"id":"o1","item":"pen"}`},
			HTTPResp: &models.HTTPResp{StatusCode: 201, Header: map[string]string{"Content-Type": "application/json"}, Body: `{"id":"o1"}`},
			Variables: models.MockVariables{
				"orderId":   {Recorded: "o1", Bind: "body.id", Answer: []string{"body.id"}},
				"requestId": {Recorded: "r1", Bind: "header.X-Request-Id", Use: []string{"url.query.rid"}, Echo: true},
			},
			ReqTimestampMock: time.Unix(1_700_000_000, 0).UTC(),
			ResTimestampMock: time.Unix(1_700_000_001, 0).UTC(),
		},
	}
}

// The variables block is written as a top-level block of the mock document and
// read back as written, through the yaml and json documents and gob; the
// recorded request and response are unchanged by it.
func TestVariablesBlockRoundTrips(t *testing.T) {
	m := variablesMock()
	want := m.Spec.Variables.Clone()

	doc, err := EncodeMock(m, zap.NewNop())
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if !reflect.DeepEqual(doc.Variables, want) {
		t.Fatalf("doc.Variables = %+v, want %+v", doc.Variables, want)
	}
	var buf bytes.Buffer
	enc := yamlLib.NewEncoder(&buf)
	if err := enc.Encode(doc); err != nil {
		t.Fatal(err)
	}
	_ = enc.Close()
	text := buf.String()
	if !strings.Contains(text, "\nvariables:\n") || strings.Contains(text, "spec:\n    variables") {
		t.Fatalf("the block is not a top-level key of the document:\n%s", text)
	}
	var back yaml.NetworkTrafficDoc
	if err := yamlLib.Unmarshal(buf.Bytes(), &back); err != nil {
		t.Fatal(err)
	}
	out, err := DecodeMocks([]*yaml.NetworkTrafficDoc{&back}, zap.NewNop())
	if err != nil || len(out) != 1 {
		t.Fatalf("decode: err=%v n=%d", err, len(out))
	}
	if !reflect.DeepEqual(out[0].Spec.Variables, want) {
		t.Fatalf("yaml round-trip: %+v, want %+v", out[0].Spec.Variables, want)
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
	if err != nil || len(jout) != 1 || !reflect.DeepEqual(jout[0].Spec.Variables, want) {
		t.Fatalf("json document round-trip: err=%v %+v", err, jout)
	}

	jd, ok, err := EncodeMockJSON(m, zap.NewNop())
	if err != nil || !ok {
		t.Fatalf("EncodeMockJSON: ok=%v err=%v", ok, err)
	}
	jm, err := DecodeMocksJSON([]*yaml.NetworkTrafficDocJSON{jd}, zap.NewNop())
	if err != nil || len(jm) != 1 || !reflect.DeepEqual(jm[0].Spec.Variables, want) {
		t.Fatalf("json format round-trip: err=%v %+v", err, jm)
	}

	roundTrip(t, "HTTP with variables", variablesMock())

	if c := m.DeepCopy(); !reflect.DeepEqual(c.Spec.Variables, want) {
		t.Fatalf("DeepCopy: %+v", c.Spec.Variables)
	} else {
		c.Spec.Variables["orderId"].Answer[0] = "body.other"
		if m.Spec.Variables["orderId"].Answer[0] != "body.id" {
			t.Fatal("DeepCopy shares the variables' slices")
		}
	}
}

// A mock without variables is written as before: no variables key.
func TestNoVariablesNoKey(t *testing.T) {
	m := variablesMock()
	m.Spec.Variables = nil
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
	if strings.Contains(buf.String(), "variables") {
		t.Fatalf("a mock without variables wrote the key:\n%s", buf.String())
	}
	jd, _, err := EncodeMockJSON(m, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := yaml.MarshalDoc(yaml.FormatJSON, &yaml.NetworkTrafficDoc{Version: doc.Version, Kind: doc.Kind, Name: doc.Name, Spec: doc.Spec}); strings.Contains(string(b), "variables") {
		t.Fatalf("json doc wrote the key: %s", b)
	}
	if jd.Variables != nil {
		t.Fatalf("EncodeMockJSON set variables: %+v", jd.Variables)
	}
}

// The in-place writer writes a mock with variables as EncodeMock does.
func TestEncodeMockInPlaceWritesTheVariables(t *testing.T) {
	for _, m := range inPlaceCorpus(t) {
		m.Name = "mock-x"
		m.Spec.Variables = models.MockVariables{"v": {Recorded: "1", Use: []string{"body.a"}}}
		inPlace, ok := encodeMockInPlace(m)
		if !ok {
			continue
		}
		var got, want bytes.Buffer
		enc := yamlLib.NewEncoder(&got)
		if err := enc.Encode(inPlace); err != nil {
			t.Fatalf("%s: in place: %v", m.Kind, err)
		}
		_ = enc.Close()
		doc, err := EncodeMock(m, zap.NewNop())
		if err != nil {
			t.Fatalf("%s: EncodeMock: %v", m.Kind, err)
		}
		enc = yamlLib.NewEncoder(&want)
		if err := enc.Encode(&doc); err != nil {
			t.Fatalf("%s: node: %v", m.Kind, err)
		}
		_ = enc.Close()
		if !bytes.Equal(got.Bytes(), want.Bytes()) || !strings.Contains(got.String(), "variables:") {
			t.Fatalf("%s: written in place:\n%s\nthrough EncodeMock:\n%s", m.Kind, got.String(), want.String())
		}
	}
}
