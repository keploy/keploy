package mockdb

import (
	"bytes"
	"context"
	"path/filepath"
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
	b, err := yaml.MarshalDoc(yaml.FormatJSON, doc)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "variables") {
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

// Declared-only variables (Source == VariableSourceDeclared) travel in memory
// and on the wire to the agent, but must be stripped on every mock disk
// write-back path (YAML, JSON, in-place YAML, and gob persisted()) while
// learned variables (Source == "") are kept intact.
func TestDeclaredVariablesStrippedOnDiskWriteBack(t *testing.T) {
	m := variablesMock()
	wantLearned := m.Spec.Variables.Clone()
	m.Spec.Variables["declaredAuth"] = models.MockVariable{
		Recorded: "Bearer rec.token",
		Use:      []string{"header.Authorization"},
		Source:   models.VariableSourceDeclared,
	}

	// 1. EncodeMock (YAML)
	doc, err := EncodeMock(m, zap.NewNop())
	if err != nil {
		t.Fatalf("EncodeMock: %v", err)
	}
	if !reflect.DeepEqual(doc.Variables, wantLearned) {
		t.Fatalf("EncodeMock kept declared variable: got %+v, want %+v", doc.Variables, wantLearned)
	}

	// 2. EncodeMockJSON (JSON)
	jdoc, ok, err := EncodeMockJSON(m, zap.NewNop())
	if err != nil || !ok {
		t.Fatalf("EncodeMockJSON: ok=%v err=%v", ok, err)
	}
	if !reflect.DeepEqual(jdoc.Variables, wantLearned) {
		t.Fatalf("EncodeMockJSON kept declared variable: got %+v, want %+v", jdoc.Variables, wantLearned)
	}

	// 3. encodeMockInPlace (in-place YAML)
	for _, im := range inPlaceCorpus(t) {
		im.Name = "mock-inplace"
		im.Spec.Variables = models.MockVariables{
			"learned":  {Recorded: "1", Use: []string{"body.a"}},
			"declared": {Recorded: "2", Use: []string{"header.Authorization"}, Source: models.VariableSourceDeclared},
		}
		inPlace, ok := encodeMockInPlace(im)
		if !ok {
			continue
		}
		var buf bytes.Buffer
		enc := yamlLib.NewEncoder(&buf)
		if err := enc.Encode(inPlace); err != nil {
			t.Fatalf("%s: encodeMockInPlace: %v", im.Kind, err)
		}
		_ = enc.Close()
		if strings.Contains(buf.String(), "declared") {
			t.Fatalf("%s: encodeMockInPlace wrote declared variable:\n%s", im.Kind, buf.String())
		}
		if !strings.Contains(buf.String(), "learned:") {
			t.Fatalf("%s: encodeMockInPlace dropped learned variable:\n%s", im.Kind, buf.String())
		}
	}

	// 4. persisted (gob write-back) does not mutate m and strips declared only.
	pm := persisted(m)
	if pm == m {
		t.Fatal("persisted() returned same pointer for mock with declared variables")
	}
	if !reflect.DeepEqual(pm.Spec.Variables, wantLearned) {
		t.Fatalf("persisted() variables = %+v, want %+v", pm.Spec.Variables, wantLearned)
	}
	if m.Spec.Variables["declaredAuth"].Source != models.VariableSourceDeclared {
		t.Fatalf("disk write-back mutated in-memory mock variables: %+v", m.Spec.Variables)
	}

	// 5. Mock with ONLY declared variables emits no variables key on disk.
	onlyDeclared := variablesMock()
	onlyDeclared.Spec.Variables = models.MockVariables{
		"declaredAuth": {Recorded: "Bearer rec.token", Use: []string{"header.Authorization"}, Source: models.VariableSourceDeclared},
	}
	docOnly, err := EncodeMock(onlyDeclared, zap.NewNop())
	if err != nil {
		t.Fatalf("EncodeMock(onlyDeclared): %v", err)
	}
	if docOnly.Variables != nil {
		t.Fatalf("EncodeMock(onlyDeclared) Variables = %+v, want nil", docOnly.Variables)
	}
	jdOnly, _, err := EncodeMockJSON(onlyDeclared, zap.NewNop())
	if err != nil {
		t.Fatalf("EncodeMockJSON(onlyDeclared): %v", err)
	}
	if jdOnly.Variables != nil {
		t.Fatalf("EncodeMockJSON(onlyDeclared) Variables = %+v, want nil", jdOnly.Variables)
	}
}

// End-to-end gob carry-over test: when an existing mocks.gob holds a mock with
// a declared variable (e.g. from an earlier write) and a new mock is appended
// via MockYaml, gobCarryOverLocked strips the declared entry while keeping
// learned variables intact.
func TestGobCarryOverStripsDeclaredVariables(t *testing.T) {
	t.Setenv("KEPLOY_MOCK_FORMAT", "gob")
	root := t.TempDir()
	setID := "test-set-carry"
	gobPath := filepath.Join(root, setID, "mocks.gob")

	existing := variablesMock()
	existing.Name = "mock-existing"
	wantLearned := existing.Spec.Variables.Clone()
	existing.Spec.Variables["declaredAuth"] = models.MockVariable{
		Recorded: "Bearer carry.token",
		Use:      []string{"header.Authorization"},
		Source:   models.VariableSourceDeclared,
	}
	// Seed the file directly so declaredAuth is present in the source gob stream.
	writeGobFile(t, gobPath, existing)

	ys := New(zap.NewNop(), root, "mocks")

	added := variablesMock()
	added.Name = "mock-added"
	added.Spec.Variables["declaredAdded"] = models.MockVariable{
		Recorded: "Bearer added.token",
		Use:      []string{"header.Authorization"},
		Source:   models.VariableSourceDeclared,
	}
	if err := ys.InsertMock(context.Background(), added, setID); err != nil {
		t.Fatalf("InsertMock: %v", err)
	}
	if err := ys.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	got := decodeGobFileRaw(t, gobPath)
	if len(got) != 2 {
		t.Fatalf("gob file has %d mocks, want 2", len(got))
	}
	for _, m := range got {
		if m.Spec.Variables.HasDeclared() {
			t.Fatalf("mock %s kept declared variable after gob carry-over: %+v", m.Name, m.Spec.Variables)
		}
		if !reflect.DeepEqual(m.Spec.Variables, wantLearned) {
			t.Fatalf("mock %s variables = %+v, want %+v", m.Name, m.Spec.Variables, wantLearned)
		}
	}
}

// End-to-end gob rewriter test: gobFileRewriter.write strips declared-only
// variables while keeping learned variables intact and leaving the in-memory
// mock untouched.
func TestGobFileRewriterStripsDeclaredVariables(t *testing.T) {
	root := t.TempDir()
	gobPath := filepath.Join(root, "test-set-rw", "mocks.gob")

	m := variablesMock()
	wantLearned := m.Spec.Variables.Clone()
	m.Spec.Variables["declaredAuth"] = models.MockVariable{
		Recorded: "Bearer rw.token",
		Use:      []string{"header.Authorization"},
		Source:   models.VariableSourceDeclared,
	}
	writeGobFile(t, gobPath, m)

	rw, err := newGobFileRewriter(root, gobPath)
	if err != nil {
		t.Fatalf("newGobFileRewriter: %v", err)
	}
	if err := rw.write(m); err != nil {
		t.Fatalf("rw.write: %v", err)
	}
	if err := rw.commit(); err != nil {
		t.Fatalf("rw.commit: %v", err)
	}

	got := decodeGobFileRaw(t, gobPath)
	if len(got) != 1 {
		t.Fatalf("rewritten gob has %d mocks, want 1", len(got))
	}
	if got[0].Spec.Variables.HasDeclared() {
		t.Fatalf("gobFileRewriter kept declared variable: %+v", got[0].Spec.Variables)
	}
	if !reflect.DeepEqual(got[0].Spec.Variables, wantLearned) {
		t.Fatalf("gobFileRewriter variables = %+v, want %+v", got[0].Spec.Variables, wantLearned)
	}
	if !m.Spec.Variables.HasDeclared() {
		t.Fatalf("gobFileRewriter mutated in-memory mock: %+v", m.Spec.Variables)
	}
}
