//go:build windows

package utils

// DieOnEscalation does nothing on Windows: the CLI ends a Windows agent
// itself (taskkill), with no elevation between them to relay a signal.
func DieOnEscalation() {}

// openCommandLineSink checks a command-line progress path and writes to it:
// a Windows agent is not elevated by the CLI, so it writes as the user who
// asked.
func openCommandLineSink(path string) (progressSink, error) {
	if err := checkCommandLinePath(path); err != nil {
		return nil, err
	}
	return pathSink(path), nil
}
