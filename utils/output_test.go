package utils

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"
)

func captureStdout(t *testing.T, fn func() error) (string, error) {
	t.Helper()

	originalStdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("failed to create pipe: %v", err)
	}
	os.Stdout = w

	runErr := fn()

	if err := w.Close(); err != nil {
		t.Fatalf("failed to close writer: %v", err)
	}
	os.Stdout = originalStdout

	var buf bytes.Buffer
	if _, err := io.Copy(&buf, r); err != nil {
		t.Fatalf("failed to read captured output: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("failed to close reader: %v", err)
	}

	return buf.String(), runErr
}

func TestJSONWriterWriteEnabled(t *testing.T) {
	writer := NewJSONWriter(true)

	out, err := captureStdout(t, func() error {
		return writer.Write(map[string]string{"status": "ok"})
	})
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	if strings.TrimSpace(out) != `{"status":"ok"}` {
		t.Fatalf("unexpected output: %q", out)
	}
}

func TestJSONWriterWriteDisabled(t *testing.T) {
	writer := NewJSONWriter(false)

	out, err := captureStdout(t, func() error {
		return writer.Write(map[string]string{"status": "ok"})
	})
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if out != "" {
		t.Fatalf("expected no output, got %q", out)
	}
}

func TestJSONWriterWriteMarshalError(t *testing.T) {
	writer := NewJSONWriter(true)
	ch := make(chan int)

	out, err := captureStdout(t, func() error {
		return writer.Write(ch)
	})
	if err == nil {
		t.Fatal("expected marshal error, got nil")
	}
	if out != "" {
		t.Fatalf("expected no output on marshal error, got %q", out)
	}
}

func TestJSONWriterIsEnabled(t *testing.T) {
	if !NewJSONWriter(true).IsEnabled() {
		t.Fatal("expected writer to be enabled")
	}
	if NewJSONWriter(false).IsEnabled() {
		t.Fatal("expected writer to be disabled")
	}
}

// --- NDJSON support (keploy-consumer-design-v2.md §7 slice 4) ---

func TestJSONWriterOut(t *testing.T) {
	type payload struct {
		A string `json:"a"`
	}

	tests := []struct {
		name    string
		enabled bool
		values  []any
		want    []string
	}{
		{name: "disabled writes nothing", enabled: false, values: []any{payload{A: "x"}}, want: nil},
		{name: "one value, one line", enabled: true, values: []any{payload{A: "x"}}, want: []string{`{"a":"x"}`}},
		{
			name:    "a loop produces NDJSON",
			enabled: true,
			values:  []any{payload{A: "x"}, payload{A: "y"}},
			want:    []string{`{"a":"x"}`, `{"a":"y"}`},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			w := NewJSONWriterOut(&buf, tt.enabled)
			for _, v := range tt.values {
				if err := w.Write(v); err != nil {
					t.Fatalf("Write: %v", err)
				}
			}
			if len(tt.want) == 0 {
				if buf.Len() != 0 {
					t.Fatalf("expected no output, got %q", buf.String())
				}
				return
			}
			lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
			if len(lines) != len(tt.want) {
				t.Fatalf("got %d lines, want %d: %q", len(lines), len(tt.want), buf.String())
			}
			for i, want := range tt.want {
				if lines[i] != want {
					t.Errorf("line %d = %q, want %q", i, lines[i], want)
				}
				var decoded map[string]any
				if err := json.Unmarshal([]byte(lines[i]), &decoded); err != nil {
					t.Errorf("line %d is not standalone JSON: %v", i, err)
				}
			}
		})
	}
}

// A nil sink must not panic; it falls back to stdout like the original writer.
func TestJSONWriterOutNilSinkFallsBack(t *testing.T) {
	w := NewJSONWriterOut(nil, false)
	if w == nil || w.IsEnabled() {
		t.Fatal("expected a disabled writer")
	}
	if err := w.Write(struct{}{}); err != nil {
		t.Fatalf("Write on a disabled writer: %v", err)
	}
}
