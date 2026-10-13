package mockdb

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml"
	"go.uber.org/zap"
)

// What the learn step learned (noise in every part of a request, and
// correlations) is written onto the recording, said per mock, read back, and
// never added twice; in every storage format.
func TestPersistLearnedWritesNoiseAndCorrelations(t *testing.T) {
	for _, f := range []yaml.Format{yaml.FormatYAML, yaml.FormatJSON} {
		t.Run(string(f), func(t *testing.T) {
			dir := t.TempDir()
			ys := NewWithFormat(zap.NewNop(), dir, "mocks", f)
			ctx := context.Background()
			set := "test-set-0"
			if err := ys.InsertMock(ctx, noiseTestMock(`{"key":"k-1"}`), set); err != nil {
				t.Fatal(err)
			}
			if err := ys.InsertMock(ctx, noiseTestMock(`{"other":"o"}`), set); err != nil {
				t.Fatal(err)
			}
			if err := ys.Close(); err != nil {
				t.Fatal(err)
			}
			corr := models.FieldCorrelation{RequestPath: "body.key", ResponsePaths: []string{"body.idempotency_key"}, RecordedValue: "k-1"}
			states := map[string]models.MockState{
				"mock-0": {Name: "mock-0", ReqBodyNoise: map[string][]string{"header.x-trace": {}, "url.path.1": {}}, Correlations: []models.FieldCorrelation{corr}},
				"mock-1": {Name: "mock-1"},
			}
			added, err := ys.PersistLearned(ctx, set, states)
			if err != nil {
				t.Fatal(err)
			}
			want := map[string][]string{"mock-0": {"body.key (correlation)", "header.x-trace (noise)", "url.path.1 (noise)"}}
			if !reflect.DeepEqual(added, want) {
				t.Fatalf("added %v, want %v", added, want)
			}
			mocks, err := ys.GetUnFilteredMocks(ctx, set, models.BaseTime, models.BaseTime.AddDate(100, 0, 0), nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			filtered, err := ys.GetFilteredMocks(ctx, set, models.BaseTime, models.BaseTime.AddDate(100, 0, 0), nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			var m0 *models.Mock
			for _, m := range append(mocks, filtered...) {
				if m.Name == "mock-0" {
					m0 = m
				}
			}
			if m0 == nil {
				t.Fatal("mock-0 is gone")
			}
			if !reflect.DeepEqual(m0.Spec.Correlations, []models.FieldCorrelation{corr}) {
				t.Fatalf("correlations read back %+v", m0.Spec.Correlations)
			}
			if !reflect.DeepEqual(m0.Spec.ReqBodyNoise, map[string][]string{"header.x-trace": {}, "url.path.1": {}}) {
				t.Fatalf("noise read back %v", m0.Spec.ReqBodyNoise)
			}
			again, err := ys.PersistLearned(ctx, set, states)
			if err != nil || len(again) != 0 {
				t.Fatalf("what a mock already carries is not added again: %v %v", again, err)
			}
		})
	}
}

// A mock that carries no correlation is written as before: no key.
func TestEncodeMockWithoutCorrelationsWritesNoKey(t *testing.T) {
	doc, err := EncodeMock(noiseTestMock(`{"a":1}`), zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	b, err := yaml.MarshalDoc(yaml.FormatYAML, doc)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "correlations") {
		t.Fatalf("a mock without correlations writes the key:\n%s", b)
	}
}
