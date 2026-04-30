package utils

import (
	"encoding/json"
	"fmt"
	"os"
)

type JSONWriter struct {
	enabled bool
}

func NewJSONWriter(enabled bool) *JSONWriter {
	return &JSONWriter{enabled: enabled}
}

func (w *JSONWriter) Write(v interface{}) error {
	if !w.enabled {
		return nil
	}

	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("failed to marshal json output: %w", err)
	}

	if _, err := fmt.Fprintln(os.Stdout, string(data)); err != nil {
		return fmt.Errorf("failed to write json output: %w", err)
	}
	return nil
}

func (w *JSONWriter) IsEnabled() bool {
	return w.enabled
}
