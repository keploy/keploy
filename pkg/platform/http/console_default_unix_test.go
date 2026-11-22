//go:build linux || darwin

package http

import (
	"context"
	"io"
	"os"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"golang.org/x/sys/unix"
)

// stdoutOf runs fn with the process's standard output (file descriptor 1,
// whatever *os.File writes to it) a pipe, and returns what was written there.
func stdoutOf(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	saved, err := unix.Dup(1)
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.Dup2(int(w.Fd()), 1); err != nil {
		t.Fatal(err)
	}
	read := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(r)
		read <- string(b)
	}()
	defer func() {
		_ = r.Close()
	}()
	func() {
		defer func() {
			_ = unix.Dup2(saved, 1)
			_ = unix.Close(saved)
			_ = w.Close()
		}()
		fn()
	}()
	return <-read
}

// An agent client as New builds it has no console: a request the agent did
// not record, which the stream names whole, is said nowhere, so a program
// that embeds the client (a k8s proxy's sidecar recording, whose stdout is
// its pod's log) puts no path in a log. Only a console its embedder sets
// (keploy's CLI: os.Stdout) is written.
func TestAnAgentClientBuiltWithDefaultsWritesNothingToStdout(t *testing.T) {
	calls := []models.NotRecordedCall{
		{Method: "GET", Path: "/v1/reset/t0k3n", PathTag: models.PathTag(context.Background(), "GET", "/v1/reset/t0k3n"), Why: "its answer is a stream"},
		{Method: "GET", Path: "/v1/reset/t0k3n", PathTag: models.PathTag(context.Background(), "GET", "/v1/reset/t0k3n"), Why: "its answer is a stream"},
	}
	if out := stdoutOf(t, func() { streamNotRecorded(t, zap.NewNop(), nil, calls) }); out != "" {
		t.Fatalf("an agent client with no console wrote to stdout:\n%s", out)
	}
}
