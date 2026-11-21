package mockdb

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

func TestPersistLearnedWritesAndRegeneratesWhyHeaderIdempotently(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	dir := t.TempDir()
	set := "set-why-hdr"
	ys := NewWithFormat(zap.NewNop(), dir, "mocks", yaml.FormatYAML)
	ctx := context.Background()

	if err := ys.InsertMock(ctx, noiseTestMock(`{"id":"a"}`), set); err != nil {
		t.Fatalf("InsertMock: %v", err)
	}
	if err := ys.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	valWhy := `[value] test=".::TestA" at="body.id#0" rec="a" full="b" from="mock-0 body.id" undo="requestbody:id"`
	if _, err := ys.PersistLearned(ctx, set, map[string]models.MockState{
		"mock-0": {
			Name: "mock-0",
			Variables: models.MockVariables{
				"id0": {Recorded: "a", Use: []string{"body.id#0"}, Why: valWhy},
			},
		},
	}); err != nil {
		t.Fatalf("PersistLearned 1: %v", err)
	}

	p := filepath.Join(dir, set, "mocks."+yaml.FormatYAML.FileExtension())
	raw1, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("ReadFile 1: %v", err)
	}
	if count := strings.Count(string(raw1), learn.WhyHeaderStart); count != 1 {
		t.Fatalf("expected 1 why header after PersistLearned, got %d:\n%s", count, raw1)
	}
	hdr := learn.ParseWhyHeader(raw1)
	res, ok := learn.ResolveWhy(hdr, valWhy)
	if !ok || !strings.Contains(res.Undo, `test.globalNoise.global.requestbody: {"id": []}`) {
		t.Fatalf("header in written mocks.yaml did not resolve line: ok=%v %+v\nfile:\n%s", ok, res, raw1)
	}

	// Second PersistLearned adding a carried variable regenerates the header once (no duplicate).
	carWhy := learn.CompactCarriedWhy(".::TestA", "mock-0 body.id#0")
	if _, err := ys.PersistLearned(ctx, set, map[string]models.MockState{
		"mock-0": {
			Name: "mock-0",
			Variables: models.MockVariables{
				"c0": {Recorded: "a", Use: []string{"body.other#0"}, Why: carWhy},
			},
		},
	}); err != nil {
		t.Fatalf("PersistLearned 2: %v", err)
	}
	raw2, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("ReadFile 2: %v", err)
	}
	if count := strings.Count(string(raw2), learn.WhyHeaderStart); count != 1 {
		t.Fatalf("expected 1 why header after second PersistLearned, got %d:\n%s", count, raw2)
	}
	hdr2 := learn.ParseWhyHeader(raw2)
	if _, ok := hdr2[learn.WhyKindValue]; !ok {
		t.Fatalf("header lost value kind after second rewrite:\n%s", raw2)
	}
	if _, ok := hdr2[learn.WhyKindCarried]; !ok {
		t.Fatalf("header missing carried kind after second rewrite:\n%s", raw2)
	}
}

func TestPersistLearnedOmitsWhyHeaderWhenNoCompactKindPresent(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	dir := t.TempDir()
	set := "set-no-why-hdr"
	ys := NewWithFormat(zap.NewNop(), dir, "mocks", yaml.FormatYAML)
	ctx := context.Background()

	if err := ys.InsertMock(ctx, noiseTestMock(`{"id":"a"}`), set); err != nil {
		t.Fatalf("InsertMock: %v", err)
	}
	if err := ys.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := ys.PersistLearned(ctx, set, map[string]models.MockState{
		"mock-0": {
			Name: "mock-0",
			Variables: models.MockVariables{
				"id0": {Recorded: "a", Use: []string{"body.id#0"}, Why: "plain prose without compact tag"},
			},
		},
	}); err != nil {
		t.Fatalf("PersistLearned: %v", err)
	}
	p := filepath.Join(dir, set, "mocks."+yaml.FormatYAML.FileExtension())
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if strings.Contains(string(raw), learn.WhyHeaderStart) {
		t.Fatalf("unexpected why header when no compact kind is present:\n%s", raw)
	}
}
