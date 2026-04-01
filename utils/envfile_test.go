package utils

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseEnvFile(t *testing.T) {
	tests := []struct {
		name     string
		content  string
		expected map[string]string
		wantErr  bool
	}{
		{
			name:     "basic key-value pairs",
			content:  "DB_HOST=localhost\nDB_PORT=5432\n",
			expected: map[string]string{"DB_HOST": "localhost", "DB_PORT": "5432"},
		},
		{
			name:     "comments and empty lines are skipped",
			content:  "# this is a comment\n\nAPI_KEY=secret\n",
			expected: map[string]string{"API_KEY": "secret"},
		},
		{
			name:     "double-quoted values are stripped",
			content:  `DB_URL="postgres://localhost/mydb"`,
			expected: map[string]string{"DB_URL": "postgres://localhost/mydb"},
		},
		{
			name:     "single-quoted values are stripped",
			content:  "TOKEN='abc123'",
			expected: map[string]string{"TOKEN": "abc123"},
		},
		{
			name:     "value containing equals sign",
			content:  "DSN=host=localhost port=5432",
			expected: map[string]string{"DSN": "host=localhost port=5432"},
		},
		{
			name:     "lines without equals are skipped",
			content:  "EXPORT_ME\nKEY=value\n",
			expected: map[string]string{"KEY": "value"},
		},
		{
			name:     "empty file",
			content:  "",
			expected: map[string]string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, ".env")
			if err := os.WriteFile(path, []byte(tt.content), 0600); err != nil {
				t.Fatalf("failed to write temp env file: %v", err)
			}

			got, err := ParseEnvFile(path)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ParseEnvFile() error = %v, wantErr %v", err, tt.wantErr)
			}

			if len(got) != len(tt.expected) {
				t.Fatalf("ParseEnvFile() returned %d entries, want %d; got=%v", len(got), len(tt.expected), got)
			}
			for k, want := range tt.expected {
				if got[k] != want {
					t.Errorf("ParseEnvFile()[%q] = %q, want %q", k, got[k], want)
				}
			}
		})
	}
}

func TestParseEnvFile_MissingFile(t *testing.T) {
	_, err := ParseEnvFile("/nonexistent/.env")
	if err == nil {
		t.Fatal("expected error for missing file, got nil")
	}
}

func TestResolveEnvVars(t *testing.T) {
	t.Run("inline only", func(t *testing.T) {
		result, err := ResolveEnvVars(map[string]string{"KEY": "val"}, "")
		if err != nil {
			t.Fatal(err)
		}
		if result["KEY"] != "val" {
			t.Errorf("expected KEY=val, got %q", result["KEY"])
		}
	})

	t.Run("env file only", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, ".env")
		if err := os.WriteFile(path, []byte("FROM_FILE=yes\n"), 0600); err != nil {
			t.Fatal(err)
		}
		result, err := ResolveEnvVars(nil, path)
		if err != nil {
			t.Fatal(err)
		}
		if result["FROM_FILE"] != "yes" {
			t.Errorf("expected FROM_FILE=yes, got %q", result["FROM_FILE"])
		}
	})

	t.Run("inline overrides file", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, ".env")
		if err := os.WriteFile(path, []byte("KEY=from_file\n"), 0600); err != nil {
			t.Fatal(err)
		}
		result, err := ResolveEnvVars(map[string]string{"KEY": "from_inline"}, path)
		if err != nil {
			t.Fatal(err)
		}
		if result["KEY"] != "from_inline" {
			t.Errorf("expected KEY=from_inline (inline wins), got %q", result["KEY"])
		}
	})

	t.Run("empty inputs return empty map", func(t *testing.T) {
		result, err := ResolveEnvVars(nil, "")
		if err != nil {
			t.Fatal(err)
		}
		if len(result) != 0 {
			t.Errorf("expected empty map, got %v", result)
		}
	})
}
