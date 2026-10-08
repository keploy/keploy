//go:build composelib && linux

package docker

import (
	"context"
	"fmt"
	"io"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/creack/pty"
	"github.com/docker/compose/v5/pkg/api"
)

// TestLiveTeardownProgressStartsBelowWhatKeployPrinted pins the terminal
// progress display to one operation. On a terminal it redraws the block it
// last drew, moving the cursor up over it. keploy prints its test summary
// between Up and the teardown's Down; a Down that reported through Up's
// display moved the cursor up over that summary, erased it, and showed Up's
// rows ("Network ... Created") again as if they were Down's.
//
// Up runs with --abort-on-container-failure (CascadeFail), one of the flags
// keploy adds when the user's command has none (ensureComposeExitOnAppFailure),
// and an app that exits 0, so nothing cascades. With a cascading stop, Up's
// own stop is reported outside any operation, as plain lines that make the
// display forget its block, and a shared display would then show itself only
// when a redraw happened to land before Down's events.
func TestLiveTeardownProgressStartsBelowWhatKeployPrinted(t *testing.T) {
	apiClient := liveClient(t)
	project := fmt.Sprintf("keploylive%d", time.Now().UnixNano()%100000)

	ptmx, tty, err := pty.Open()
	if err != nil {
		t.Skipf("no pty: %v", err)
	}
	defer func() { _ = ptmx.Close() }()
	var screen syncBuffer
	read := make(chan struct{})
	go func() {
		defer close(read)
		_, _ = io.Copy(&screen, ptmx)
	}()

	yaml := fmt.Sprintf(`
services:
  app:
    image: %s
    command: ["sh", "-c", "exit 0"]
`, busybox)
	r := runnerWith(t, apiClient, ComposeRunnerOptions{
		Content:     []byte(yaml),
		ProjectName: project,
		WorkingDir:  t.TempDir(),
		Progress:    tty,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	var out, errW syncBuffer
	if err := r.Up(ctx, ComposeUpOptions{OnExit: api.CascadeFail}, &out, &errW); err != nil {
		t.Fatalf("Up: %v\n%s", err, out.String())
	}
	const summary = "keploy test summary: 7 passed"
	if _, err := fmt.Fprintln(tty, summary); err != nil {
		t.Fatal(err)
	}
	if err := r.Down(ctx, time.Second); err != nil {
		t.Fatalf("Down: %v", err)
	}
	_ = tty.Close()
	select {
	case <-read:
	case <-time.After(10 * time.Second):
		t.Fatal("the terminal never closed")
	}

	all := screen.String()
	i := strings.Index(all, summary)
	if i < 0 {
		t.Fatalf("the summary never reached the terminal:\n%q", all)
	}
	after := all[i+len(summary):]
	first := strings.Index(after, "[+] down")
	if first < 0 {
		t.Fatalf("Down drew no progress on the terminal:\n%q", after)
	}
	if up := regexp.MustCompile(`\x1b\[\d+A`).FindString(after[:first]); up != "" {
		t.Fatalf("Down's display moved the cursor up (%q) over what was printed after Up:\n%q", up, after)
	}
	if strings.Contains(after, "Created") || strings.Contains(after, "Started") {
		t.Fatalf("Down's display showed Up's rows again:\n%q", after)
	}
}
