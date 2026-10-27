//go:build windows

package utils

import (
	"context"
	"path/filepath"
)

// DieOnEscalation does nothing on Windows: the CLI ends a Windows agent
// itself (taskkill), with no elevation between them to relay a signal.
func DieOnEscalation(context.Context) {}

// openCommandLineSink checks a command-line progress path and writes to it:
// a Windows agent is not elevated by the CLI, so it writes as the user who
// asked.
func openCommandLineSink(path string) (progressSink, error) {
	return openCommandLineSinkNamed(path, filepath.Base(path))
}

// openCommandLineSinkNamed is openCommandLineSink writing the file name, in the
// command-line path's directory: an agent reports its start there too
// (BootProgressFileName), next to its teardown.
func openCommandLineSinkNamed(path, name string) (progressSink, error) {
	if err := checkCommandLinePath(path); err != nil {
		return nil, err
	}
	return pathSink(filepath.Join(filepath.Dir(path), name)), nil
}
