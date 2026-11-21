//go:build !windows

package main

import (
	"os"
	"path/filepath"
	"testing"

	"go.uber.org/zap"
)

// In keploy's agent container KEPLOY_DEBUG_FILE is /keploy-host/agent-debug.log,
// in the host's keploy folder, whose files come with the repository. A link
// there that leads out of the folder is refused: the agent goes on without
// its debug log and the file the link names stays as it was.
func TestTheAgentDebugFileIsNotOpenedThroughALinkOutOfItsFolder(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "keploy-host")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(base, "outside.txt")
	const kept = "a file of the user's\n"
	if err := os.WriteFile(outside, []byte(kept), 0o644); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "agent-debug.log")
	if err := os.Symlink(filepath.Join("..", "outside.txt"), path); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KEPLOY_DEBUG_FILE", path)
	f, sink := maybeAttachDebugFileSink(zap.NewNop())
	if f != nil || sink != nil {
		if sink != nil {
			_ = sink.Flush()
		}
		if f != nil {
			_ = f.Close()
		}
		t.Fatalf("the debug file was opened through a link out of its folder")
	}
	if got, _ := os.ReadFile(outside); string(got) != kept {
		t.Errorf("the file outside the folder is now %q; want it untouched %q", got, kept)
	}
}
