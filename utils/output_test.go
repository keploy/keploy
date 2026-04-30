package utils

import (
	"bytes"
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
