package utils

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
)

type JSONWriter struct {
	enabled bool
	out     io.Writer
}

// NewJSONWriter writes to os.Stdout. Kept as-is for every existing caller.
//
// The sink is left nil rather than captured here on purpose: Write resolves
// os.Stdout at call time, so a caller (or a test) that swaps os.Stdout AFTER
// constructing the writer still gets its output redirected — the behaviour
// this type had before it grew an explicit sink.
func NewJSONWriter(enabled bool) *JSONWriter {
	return &JSONWriter{enabled: enabled}
}

// NewJSONWriterOut writes to an explicit sink. Needed so callers that already
// own a buffered writer can emit through it instead of racing os.Stdout, and
// so the output is assertable in a test. Mirrors matcher.NewDiffsPrinterOut.
// A nil sink means "os.Stdout, resolved at Write time".
func NewJSONWriterOut(out io.Writer, enabled bool) *JSONWriter {
	return &JSONWriter{enabled: enabled, out: out}
}

// Write marshals v and appends a trailing newline. Called in a loop this
// produces NDJSON — one self-contained JSON object per line.
func (w *JSONWriter) Write(v interface{}) error {
	if !w.enabled {
		return nil
	}

	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("failed to marshal json output: %w", err)
	}

	out := w.out
	if out == nil {
		out = os.Stdout
	}
	if _, err := fmt.Fprintln(out, string(data)); err != nil {
		return fmt.Errorf("failed to write json output: %w", err)
	}
	return nil
}

func (w *JSONWriter) IsEnabled() bool {
	return w.enabled
}
