package mockdb

import (
	"bytes"
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
	yamlLib "gopkg.in/yaml.v3"
)

// derivedAuthored is the record of a mock derived from TestCheckout's
// mock-7 with every part of a delta set.
func derivedAuthored() *models.Authored {
	body := `{"error":"card_declined"}`
	return &models.Authored{
		How:     models.AuthoredDerived,
		Reason:  "declined card",
		Source:  "TestCheckout/mock-7",
		Request: "sha256:0f1e",
		Delta: &models.AuthoredDelta{
			Status:     402,
			Body:       &body,
			Header:     map[string]string{"X-Decline-Code": "insufficient_funds"},
			DropHeader: []string{"Etag"},
		},
	}
}

// A mock that says it was authored (models.MockSpec.Authored: derived from
// a recorded mock, with the change it made to that mock's answer, and why)
// is a legible top-level `authored:` block of its document, and survives a
// YAML and a JSON round trip with the recorded bytes unchanged, as does a
// generated mock's, whose body is deliberately malformed. A recorded mock
// writes no such key, so a recording without the field reads and writes as
// before.
func TestAnAuthoredBlockRoundTrips(t *testing.T) {
	empty := ""
	for _, a := range []*models.Authored{
		derivedAuthored(),
		{How: models.AuthoredGenerated, Reason: "provider times out", Request: "sha256:aa", Malformed: true},
		{How: models.AuthoredDerived, Reason: "empty answer", Source: "TestA/mock-1", Delta: &models.AuthoredDelta{Body: &empty}},
	} {
		t.Run(a.Reason, func(t *testing.T) {
			m := variablesMock()
			m.Spec.Authored = a
			want := a.Clone()
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
			if !strings.Contains(text, "\nauthored:\n") || !strings.Contains(text, "reason: "+a.Reason) {
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
			if !reflect.DeepEqual(out[0].Spec.Authored, want) {
				t.Fatalf("yaml round-trip: %+v, want %+v", out[0].Spec.Authored, want)
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
			if err != nil || len(jout) != 1 || !reflect.DeepEqual(jout[0].Spec.Authored, want) {
				t.Fatalf("json round-trip: err=%v %+v, want %+v", err, jout, want)
			}
		})
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
	if strings.Contains(buf.String(), "authored") {
		t.Fatalf("a recorded mock wrote the key:\n%s", buf.String())
	}
}

// An authored mock is written by the store, in every format a recording is
// kept in (YAML, JSON, gob), and every rewrite of the recording (the noise
// write-back, the prune of unused mocks) keeps its `authored:` block as it
// is: a reader of the set still tells the authored answer from a recorded
// one, and why it is there.
func TestEveryRewriteKeepsAnAuthoredBlock(t *testing.T) {
	t.Setenv(provenswitch.Env, "")
	for _, f := range []string{string(yaml.FormatYAML), string(yaml.FormatJSON), mockFormatGob} {
		t.Run(f, func(t *testing.T) {
			dir := t.TempDir()
			ys := clockStore(dir, f)
			ctx := context.Background()
			set := "test-set-0"
			m := noiseTestMock(`{"card":"4000000000000002","other":"o"}`)
			m.Name = "mock-0"
			m.Spec.Authored = derivedAuthored()
			if err := ys.InsertMock(ctx, m, set); err != nil {
				t.Fatal(err)
			}
			unused := noiseTestMock(`{"unused":"u"}`)
			unused.Name = "mock-1"
			if err := ys.InsertMock(ctx, unused, set); err != nil {
				t.Fatal(err)
			}
			if err := ys.Close(); err != nil {
				t.Fatal(err)
			}
			if got := readBack(t, ys, set, "mock-0"); !reflect.DeepEqual(got.Spec.Authored, derivedAuthored()) {
				t.Fatalf("written and read back: %+v, want %+v", got.Spec.Authored, derivedAuthored())
			}
			if err := ys.PersistMockNoise(ctx, set, map[string]models.MockState{"mock-0": {Name: "mock-0",
				ReqBodyNoise: map[string][]string{"body.other": {}}}}); err != nil {
				t.Fatal(err)
			}
			if got := readBack(t, ys, set, "mock-0"); !reflect.DeepEqual(got.Spec.Authored, derivedAuthored()) {
				t.Fatalf("the noise write-back left %+v, want %+v", got.Spec.Authored, derivedAuthored())
			}
			future := time.Now().Add(time.Hour)
			if err := ys.UpdateMocks(ctx, set, map[string]models.MockState{"mock-0": {Name: "mock-0"}}, future, models.BaseTime); err != nil {
				t.Fatal(err)
			}
			if got := readBack(t, ys, set, "mock-0"); !reflect.DeepEqual(got.Spec.Authored, derivedAuthored()) {
				t.Fatalf("the prune left %+v, want %+v", got.Spec.Authored, derivedAuthored())
			}
			left, err := ys.GetUnFilteredMocks(ctx, set, models.BaseTime, models.BaseTime.AddDate(100, 0, 0), nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			filtered, err := ys.GetFilteredMocks(ctx, set, models.BaseTime, models.BaseTime.AddDate(100, 0, 0), nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			if n := len(left) + len(filtered); n != 1 {
				t.Fatalf("the prune kept %d mocks, want 1: it did not rewrite the file", n)
			}
		})
	}
}

// A kind an out-of-tree mapper decodes keeps its document's `authored:`
// block, though the mapper rebuilds the mock whole.
func TestAMappedKindsAuthoredBlockSurvivesItsDecode(t *testing.T) {
	const kind = models.Kind("EnterpriseKind")
	clearRegistry(t)
	RegisterMockYAMLMapper(kind, MockYAMLMapper{
		Encode: func(*models.Mock, *yaml.NetworkTrafficDoc) error { return nil },
		Decode: func(doc *yaml.NetworkTrafficDoc, m *models.Mock) error {
			*m = models.Mock{Version: doc.Version, Name: doc.Name, Kind: doc.Kind}
			return nil
		},
	})
	doc, err := EncodeMock(&models.Mock{Version: models.GetVersion(), Name: "mock-0", Kind: kind, Spec: models.MockSpec{Authored: derivedAuthored()}}, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(doc.Authored, derivedAuthored()) {
		t.Fatalf("precondition: the mapped kind's document holds %+v, want %+v", doc.Authored, derivedAuthored())
	}
	back, err := decodeMocks([]*yaml.NetworkTrafficDoc{doc}, zap.NewNop(), false)
	if err != nil || len(back) != 1 {
		t.Fatalf("read back %d mocks, %v", len(back), err)
	}
	if !reflect.DeepEqual(back[0].Spec.Authored, derivedAuthored()) {
		t.Fatalf("the mapped mock read back with %+v, want %+v", back[0].Spec.Authored, derivedAuthored())
	}
}
