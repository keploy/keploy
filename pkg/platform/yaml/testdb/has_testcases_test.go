package testdb

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func sampleHTTPTestCase(name string) *models.TestCase {
	now := time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC)
	return &models.TestCase{
		Version: models.V1Beta1,
		Kind:    models.HTTP,
		Name:    name,
		Created: now.Unix(),
		HTTPReq: models.HTTPReq{
			Method:     models.Method("GET"),
			ProtoMajor: 1,
			ProtoMinor: 1,
			URL:        "http://127.0.0.1:8080/ping",
			Header:     map[string]string{"Host": "127.0.0.1:8080"},
			Timestamp:  now,
		},
		HTTPResp: models.HTTPResp{
			StatusCode: 200,
			Header:     map[string]string{"Content-Type": "text/plain"},
			Body:       "ok",
			Timestamp:  now.Add(5 * time.Millisecond),
		},
	}
}

// HasTestCases must match GetTestCases's emptiness decision on every shape
// while never decoding YAML/JSON and never logging at ERROR (so a replay
// pre-scan does not duplicate GetTestCases's ERROR log on an unreadable
// directory or file).
func TestHasTestCases_MatchesGetTestCasesWithoutLoggingErrors(t *testing.T) {
	ctx := context.Background()

	type row struct {
		name      string
		format    yaml.Format
		setup     func(t *testing.T, root string) string
		wantHas   bool
		wantErr   bool
		wantGTErr bool // whether GetTestCases is expected to return an error
		wantGTLen int  // expected GetTestCases length when !wantGTErr
	}

	rows := []row{
		{
			name:   "missing tests dir",
			format: yaml.FormatYAML,
			setup: func(t *testing.T, root string) string {
				if err := os.MkdirAll(filepath.Join(root, "test-set-0"), 0o755); err != nil {
					t.Fatal(err)
				}
				return "test-set-0"
			},
			wantHas:   false,
			wantErr:   false,
			wantGTErr: false,
			wantGTLen: 0,
		},
		{
			name:   "empty tests dir",
			format: yaml.FormatYAML,
			setup: func(t *testing.T, root string) string {
				if err := os.MkdirAll(filepath.Join(root, "test-set-0", "tests"), 0o755); err != nil {
					t.Fatal(err)
				}
				return "test-set-0"
			},
			wantHas:   false,
			wantErr:   false,
			wantGTErr: false,
			wantGTLen: 0,
		},
		{
			name:   "only mocks and non-testcase files",
			format: yaml.FormatYAML,
			setup: func(t *testing.T, root string) string {
				dir := filepath.Join(root, "test-set-0", "tests")
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatal(err)
				}
				for _, f := range []string{"mocks.yaml", "mocks.json", "notes.txt"} {
					if err := os.WriteFile(filepath.Join(dir, f), []byte("non-empty\n"), 0o644); err != nil {
						t.Fatal(err)
					}
				}
				return "test-set-0"
			},
			wantHas:   false,
			wantErr:   false,
			wantGTErr: false,
			wantGTLen: 0,
		},
		{
			name:   "only zero-byte yaml and json files",
			format: yaml.FormatYAML,
			setup: func(t *testing.T, root string) string {
				dir := filepath.Join(root, "test-set-0", "tests")
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatal(err)
				}
				for _, f := range []string{"test-1.yaml", "test-2.json"} {
					if err := os.WriteFile(filepath.Join(dir, f), nil, 0o644); err != nil {
						t.Fatal(err)
					}
				}
				return "test-set-0"
			},
			wantHas:   false,
			wantErr:   false,
			wantGTErr: false,
			wantGTLen: 0,
		},
		{
			name:   "zero-byte preferred yaml followed by valid json case",
			format: yaml.FormatYAML,
			setup: func(t *testing.T, root string) string {
				// listTestCaseFiles sorts preferred-format (.yaml) entries ahead
				// of non-preferred (.json) entries, so test-1.yaml is guaranteed
				// to be inspected before test-2.json on every filesystem.
				if err := NewWithFormat(zap.NewNop(), root, yaml.FormatJSON).InsertTestCase(ctx, sampleHTTPTestCase("test-2"), "test-set-0", false); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(root, "test-set-0", "tests", "test-1.yaml"), nil, 0o644); err != nil {
					t.Fatal(err)
				}
				return "test-set-0"
			},
			wantHas:   true,
			wantErr:   false,
			wantGTErr: false,
			wantGTLen: 1,
		},
		{
			name:   "zero-byte preferred json followed by valid yaml case",
			format: yaml.FormatJSON,
			setup: func(t *testing.T, root string) string {
				if err := NewWithFormat(zap.NewNop(), root, yaml.FormatYAML).InsertTestCase(ctx, sampleHTTPTestCase("test-2"), "test-set-0", false); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(root, "test-set-0", "tests", "test-1.json"), nil, 0o644); err != nil {
					t.Fatal(err)
				}
				return "test-set-0"
			},
			wantHas:   true,
			wantErr:   false,
			wantGTErr: false,
			wantGTLen: 1,
		},
		{
			name:   "valid yaml case",
			format: yaml.FormatYAML,
			setup: func(t *testing.T, root string) string {
				if err := New(zap.NewNop(), root).InsertTestCase(ctx, sampleHTTPTestCase("test-1"), "test-set-0", false); err != nil {
					t.Fatal(err)
				}
				return "test-set-0"
			},
			wantHas:   true,
			wantErr:   false,
			wantGTErr: false,
			wantGTLen: 1,
		},
		{
			name:   "valid json case",
			format: yaml.FormatJSON,
			setup: func(t *testing.T, root string) string {
				if err := NewWithFormat(zap.NewNop(), root, yaml.FormatJSON).InsertTestCase(ctx, sampleHTTPTestCase("test-1"), "test-set-0", false); err != nil {
					t.Fatal(err)
				}
				return "test-set-0"
			},
			wantHas:   true,
			wantErr:   false,
			wantGTErr: false,
			wantGTLen: 1,
		},
		{
			name:   "one-byte file is non-empty",
			format: yaml.FormatYAML,
			setup: func(t *testing.T, root string) string {
				dir := filepath.Join(root, "test-set-0", "tests")
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "test-1.yaml"), []byte("x"), 0o644); err != nil {
					t.Fatal(err)
				}
				return "test-set-0"
			},
			wantHas:   true,
			wantErr:   false,
			wantGTErr: true,
		},
		{
			name:   "malformed yaml is not treated as empty",
			format: yaml.FormatYAML,
			setup: func(t *testing.T, root string) string {
				dir := filepath.Join(root, "test-set-0", "tests")
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "test-1.yaml"), []byte(":::not yaml\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				return "test-set-0"
			},
			wantHas:   true,
			wantErr:   false,
			wantGTErr: true,
		},
		{
			name:   "invalid testcase kind is not treated as empty",
			format: yaml.FormatYAML,
			setup: func(t *testing.T, root string) string {
				dir := filepath.Join(root, "test-set-0", "tests")
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "test-1.yaml"), []byte("kind: Unknown\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				return "test-set-0"
			},
			wantHas:   true,
			wantErr:   false,
			wantGTErr: true,
		},
		{
			name:   "format preference dedup: preferred empty yaml shadows valid json",
			format: yaml.FormatYAML,
			setup: func(t *testing.T, root string) string {
				if err := NewWithFormat(zap.NewNop(), root, yaml.FormatJSON).InsertTestCase(ctx, sampleHTTPTestCase("test-1"), "test-set-0", false); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(root, "test-set-0", "tests", "test-1.yaml"), nil, 0o644); err != nil {
					t.Fatal(err)
				}
				return "test-set-0"
			},
			wantHas:   false,
			wantErr:   false,
			wantGTErr: false,
			wantGTLen: 0,
		},
		{
			name:   "format preference dedup: preferred valid json shadows empty yaml",
			format: yaml.FormatJSON,
			setup: func(t *testing.T, root string) string {
				if err := NewWithFormat(zap.NewNop(), root, yaml.FormatJSON).InsertTestCase(ctx, sampleHTTPTestCase("test-1"), "test-set-0", false); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(root, "test-set-0", "tests", "test-1.yaml"), nil, 0o644); err != nil {
					t.Fatal(err)
				}
				return "test-set-0"
			},
			wantHas:   true,
			wantErr:   false,
			wantGTErr: false,
			wantGTLen: 1,
		},
		{
			name:   "regular file in place of tests directory is an error",
			format: yaml.FormatYAML,
			setup: func(t *testing.T, root string) string {
				set := filepath.Join(root, "test-set-0")
				if err := os.MkdirAll(set, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(set, "tests"), []byte("not a dir"), 0o644); err != nil {
					t.Fatal(err)
				}
				return "test-set-0"
			},
			wantHas:   false,
			wantErr:   true,
			wantGTErr: true,
		},
		{
			name:   "directory named like testcase file is an error",
			format: yaml.FormatYAML,
			setup: func(t *testing.T, root string) string {
				dir := filepath.Join(root, "test-set-0", "tests", "test-1.yaml")
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatal(err)
				}
				return "test-set-0"
			},
			wantHas:   false,
			wantErr:   true,
			wantGTErr: true,
		},
	}

	for _, tc := range rows {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			setID := tc.setup(t, root)

			core, logs := observer.New(zap.ErrorLevel)
			db := NewWithFormat(zap.New(core), root, tc.format)

			has, err := db.HasTestCases(ctx, setID)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("HasTestCases = (%v, nil), want error", has)
				}
			} else if err != nil {
				t.Fatalf("HasTestCases unexpected error: %v", err)
			}
			if has != tc.wantHas {
				t.Fatalf("HasTestCases = %v, want %v", has, tc.wantHas)
			}
			if logs.Len() != 0 {
				t.Fatalf("HasTestCases emitted %d ERROR log(s), want 0: %+v", logs.Len(), logs.All())
			}

			// Verify parity with GetTestCases: empty iff HasTestCases == (false, nil).
			gtCore, _ := observer.New(zap.ErrorLevel)
			cases, gtErr := NewWithFormat(zap.New(gtCore), root, tc.format).GetTestCases(ctx, setID)
			if tc.wantGTErr {
				if gtErr == nil {
					t.Fatalf("GetTestCases = %d cases, nil; want error", len(cases))
				}
			} else {
				if gtErr != nil {
					t.Fatalf("GetTestCases unexpected error: %v", gtErr)
				}
				if len(cases) != tc.wantGTLen {
					t.Fatalf("GetTestCases = %d cases, want %d", len(cases), tc.wantGTLen)
				}
			}
			gtEmpty := gtErr == nil && len(cases) == 0
			hasEmpty := err == nil && !has
			if gtEmpty != hasEmpty {
				t.Fatalf("emptiness mismatch: GetTestCases empty=%v (len=%d err=%v), HasTestCases empty=%v (has=%v err=%v)",
					gtEmpty, len(cases), gtErr, hasEmpty, has, err)
			}
		})
	}
}

func TestHasTestCases_SymlinksAndCanceledContext(t *testing.T) {
	t.Run("canceled context returns context error without logging", func(t *testing.T) {
		for _, populated := range []bool{false, true} {
			root := t.TempDir()
			if populated {
				if err := New(zap.NewNop(), root).InsertTestCase(context.Background(), sampleHTTPTestCase("test-1"), "test-set-0", false); err != nil {
					t.Fatal(err)
				}
			}
			core, logs := observer.New(zap.ErrorLevel)
			db := New(zap.New(core), root)
			ctx, cancel := context.WithCancel(context.Background())
			cancel()

			has, err := db.HasTestCases(ctx, "test-set-0")
			if err != context.Canceled || has {
				t.Fatalf("populated=%v: HasTestCases(canceled) = (%v, %v), want (false, context.Canceled)", populated, has, err)
			}
			if logs.Len() != 0 {
				t.Fatalf("populated=%v: HasTestCases emitted %d ERROR log(s), want 0: %+v", populated, logs.Len(), logs.All())
			}
		}
	})

	t.Run("symlink escaping recording root to empty file is an error, not empty", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("symlinks require elevated privilege on Windows")
		}
		outside := filepath.Join(t.TempDir(), "empty.yaml")
		if err := os.WriteFile(outside, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		root := t.TempDir()
		dir := filepath.Join(root, "test-set-0", "tests")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(dir, "test-1.yaml")); err != nil {
			t.Fatal(err)
		}
		core, logs := observer.New(zap.ErrorLevel)
		has, err := New(zap.New(core), root).HasTestCases(context.Background(), "test-set-0")
		if err == nil || has {
			t.Fatalf("HasTestCases(symlink out) = (%v, %v), want (false, error)", has, err)
		}
		if logs.Len() != 0 {
			t.Fatalf("HasTestCases emitted %d ERROR log(s), want 0: %+v", logs.Len(), logs.All())
		}
	})

	t.Run("symlink inside recording root follows target", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("symlinks require elevated privilege on Windows")
		}
		root := t.TempDir()
		if err := New(zap.NewNop(), root).InsertTestCase(context.Background(), sampleHTTPTestCase("test-1"), "test-set-0", false); err != nil {
			t.Fatal(err)
		}
		targetDir := filepath.Join(root, "test-set-1", "tests")
		if err := os.MkdirAll(targetDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join("..", "..", "test-set-0", "tests", "test-1.yaml"), filepath.Join(targetDir, "test-1.yaml")); err != nil {
			t.Fatal(err)
		}
		has, err := New(zap.NewNop(), root).HasTestCases(context.Background(), "test-set-1")
		if err != nil || !has {
			t.Fatalf("HasTestCases(in-root symlink to case) = (%v, %v), want (true, nil)", has, err)
		}
	})

	t.Run("path traversal testSetID returns error without logging", func(t *testing.T) {
		core, logs := observer.New(zap.ErrorLevel)
		has, err := New(zap.New(core), "").HasTestCases(context.Background(), "../escape")
		if err == nil || has {
			t.Fatalf("HasTestCases(../escape) = (%v, %v), want (false, error)", has, err)
		}
		if logs.Len() != 0 {
			t.Fatalf("HasTestCases emitted %d ERROR log(s), want 0: %+v", logs.Len(), logs.All())
		}
	})

	t.Run("parent-escaping testSetID with existing file outside root is refused without logging", func(t *testing.T) {
		base := t.TempDir()
		root := filepath.Join(base, "keploy")
		if err := os.MkdirAll(root, 0o755); err != nil {
			t.Fatal(err)
		}
		escapeDir := filepath.Join(base, "escape", "tests")
		if err := os.MkdirAll(escapeDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(escapeDir, "test-1.yaml"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
		core, logs := observer.New(zap.ErrorLevel)
		has, err := New(zap.New(core), root).HasTestCases(context.Background(), "../escape")
		if err == nil || has {
			t.Fatalf("HasTestCases(../escape with file outside root) = (%v, %v), want (false, error)", has, err)
		}
		if logs.Len() != 0 {
			t.Fatalf("HasTestCases emitted %d ERROR log(s), want 0: %+v", logs.Len(), logs.All())
		}
	})
}

// PresenceAppliesTo must return true only for the *TestYaml receiver itself,
// never for a distinct instance, nil, or any 1-level / 2-level wrapper that
// embeds *TestYaml (including non-comparable value structs with map fields).
func TestPresenceAppliesTo_RejectsEmbeddedWrappers(t *testing.T) {
	type innerWrap struct {
		*TestYaml
	}
	type outerValueWrap struct {
		*innerWrap
		filter map[string]bool // non-comparable; db == ts must still return false without panicking
	}

	root := t.TempDir()
	db := New(zap.NewNop(), root)
	other := New(zap.NewNop(), root)
	inner := &innerWrap{TestYaml: db}
	outerVal := outerValueWrap{innerWrap: inner, filter: map[string]bool{"test-set-1": true}}
	outerPtr := &outerVal

	if !db.PresenceAppliesTo(db) {
		t.Fatal("db.PresenceAppliesTo(db) = false, want true")
	}
	if db.PresenceAppliesTo(other) {
		t.Fatal("db.PresenceAppliesTo(other) = true, want false")
	}
	if db.PresenceAppliesTo(nil) {
		t.Fatal("db.PresenceAppliesTo(nil) = true, want false")
	}
	if inner.PresenceAppliesTo(inner) {
		t.Fatal("1-level pointer wrapper inherited PresenceAppliesTo = true, want false")
	}
	if outerVal.PresenceAppliesTo(outerVal) {
		t.Fatal("2-level value wrapper inherited PresenceAppliesTo = true, want false")
	}
	if outerPtr.PresenceAppliesTo(outerPtr) {
		t.Fatal("2-level pointer wrapper inherited PresenceAppliesTo = true, want false")
	}
}
