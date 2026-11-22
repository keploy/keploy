package testdb

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// A test case's origin (proven switch: the clock its window start is on) is
// written as its document's top-level origin block and read back, in YAML
// and in JSON, whatever the switch.
func TestATestCasesOriginRoundTrips(t *testing.T) {
	origin := models.Origin{Clock: "proxy:run-1"}
	for _, format := range []yaml.Format{yaml.FormatYAML, yaml.FormatJSON} {
		ext := format.FileExtension()
		t.Run(ext, func(t *testing.T) {
			t.Setenv(provenswitch.Env, "1")
			dir := t.TempDir()
			ts := NewWithFormat(zap.NewNop(), dir, format)
			for _, tc := range goldenTestCases() {
				tc.Origin = &origin
				f, err := ts.TestCaseFile(context.Background(), tc, "test-set-0")
				if err != nil {
					t.Fatalf("%s: %v", tc.Name, err)
				}
				want := "\norigin:\n  clock: proxy:run-1\n"
				if format == yaml.FormatJSON {
					want = "\"origin\": {\n    \"clock\": \"proxy:run-1\"\n  }"
				}
				if !strings.Contains(string(f.Data), want) {
					t.Fatalf("%s's file holds no origin block:\n%s", tc.Name, f.Data)
				}
				if err := os.MkdirAll(filepath.Dir(f.Path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(f.Path, f.Data, 0o644); err != nil {
					t.Fatal(err)
				}
				back, err := ts.GetTestCase(context.Background(), "test-set-0", tc.Name)
				if err != nil {
					t.Fatalf("%s: read back: %v", tc.Name, err)
				}
				if back.Origin == nil || *back.Origin != origin {
					t.Fatalf("%s read back with origin %+v; want %+v", tc.Name, back.Origin, origin)
				}
				t.Setenv(provenswitch.Env, "")
				off, err := ts.GetTestCase(context.Background(), "test-set-0", tc.Name)
				if err != nil {
					t.Fatalf("%s: read back without the switch: %v", tc.Name, err)
				}
				if off.Origin == nil || *off.Origin != origin {
					t.Fatalf("%s read back without the switch with origin %+v; want %+v", tc.Name, off.Origin, origin)
				}
				t.Setenv(provenswitch.Env, "1")
			}
		})
	}
}

// A rewrite of a test case without the proven switch (normalize's and the
// replay's UpdateTestCase; templatize writes a copy of what it read the same
// way) keeps the origin a record with it wrote, and a
// case recorded without one is rewritten without one.
func TestATestCasesOriginSurvivesARewriteWithoutTheSwitch(t *testing.T) {
	origin := models.Origin{Clock: "ktime:boot-1.anchor-1"}
	for _, format := range []yaml.Format{yaml.FormatYAML, yaml.FormatJSON} {
		t.Run(format.FileExtension(), func(t *testing.T) {
			t.Setenv(provenswitch.Env, "")
			ts := NewWithFormat(zap.NewNop(), t.TempDir(), format)
			ctx := context.Background()
			cases := goldenTestCases()
			cases[0].Origin = &origin // post-url-1 has one; test-2 has none
			for _, tc := range cases {
				f, err := ts.TestCaseFile(ctx, tc, "test-set-0")
				if err != nil {
					t.Fatalf("%s: %v", tc.Name, err)
				}
				if err := os.MkdirAll(filepath.Dir(f.Path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(f.Path, f.Data, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			for _, tc := range cases {
				read, err := ts.GetTestCase(ctx, "test-set-0", tc.Name)
				if err != nil {
					t.Fatalf("%s: %v", tc.Name, err)
				}
				read.HTTPResp.Body = `{"ts":2,"url":"http://localhost:8080/Lhr4BWAi"}` // what a normalize changes
				if err := ts.UpdateTestCase(ctx, read, "test-set-0", false); err != nil {
					t.Fatalf("%s: rewrite: %v", tc.Name, err)
				}
				back, err := ts.GetTestCase(ctx, "test-set-0", tc.Name)
				if err != nil {
					t.Fatalf("%s: read the rewrite back: %v", tc.Name, err)
				}
				switch {
				case tc.Origin != nil && (back.Origin == nil || *back.Origin != *tc.Origin):
					t.Errorf("%s, rewritten without the switch, has origin %+v; want %+v", tc.Name, back.Origin, *tc.Origin)
				case tc.Origin == nil && back.Origin != nil:
					t.Errorf("%s, recorded without an origin, was rewritten with %+v", tc.Name, back.Origin)
				}
			}
		})
	}
}
