package utils

import "testing"

// InterruptExitCode turns the captured signal number into the shell's 128+N
// "terminated by signal N" exit code, so a replay killed before it finished
// verifying exits non-zero rather than 0. No signal → 0, leaving the code alone.
func TestInterruptExitCode(t *testing.T) {
	t.Cleanup(ClearInterrupted)
	for _, tc := range []struct {
		name string
		sig  int // POSIX signal number
		want int
	}{
		{"no signal", 0, 0},
		{"SIGHUP", 1, 129},
		{"SIGINT", 2, 130},
		{"SIGTERM", 15, 143},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ClearInterrupted()
			interruptSignal.Store(int32(tc.sig))
			if got := InterruptExitCode(); got != tc.want {
				t.Fatalf("InterruptExitCode() for signal %d = %d, want %d", tc.sig, got, tc.want)
			}
			if got := InterruptSignalNumber(); got != tc.sig {
				t.Fatalf("InterruptSignalNumber() = %d, want %d", got, tc.sig)
			}
		})
	}
	ClearInterrupted()
	if InterruptExitCode() != 0 || InterruptSignalNumber() != 0 {
		t.Fatal("ClearInterrupted must reset the captured signal")
	}
}
