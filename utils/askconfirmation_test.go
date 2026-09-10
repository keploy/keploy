package utils

import (
	"context"
	"os"
	"testing"
)

// TestAskForConfirmation_NonInteractiveStdinDeclines covers the documented
// contract that an unrecognised or interrupted answer degrades to "no". A
// closed / non-TTY stdin yields io.EOF, which used to be returned as an error
// and surfaced as a non-zero exit for callers like `keploy config --generate`
// running in a Dockerfile, CI step or cron job.
func TestAskForConfirmation_NonInteractiveStdinDeclines(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("failed to create pipe: %v", err)
	}
	// Closing the write end immediately makes every read return io.EOF, which
	// is what a closed or redirected-from-/dev/null stdin looks like.
	if err := w.Close(); err != nil {
		t.Fatalf("failed to close pipe writer: %v", err)
	}

	orig := os.Stdin
	os.Stdin = r
	t.Cleanup(func() {
		os.Stdin = orig
		_ = r.Close()
	})

	override, err := AskForConfirmation(context.Background(), "Config file already exists. Do you want to override it?")
	if err != nil {
		t.Fatalf("AskForConfirmation returned error %v, want nil on EOF", err)
	}
	if override {
		t.Error("AskForConfirmation returned true on EOF, want false (decline)")
	}
}
