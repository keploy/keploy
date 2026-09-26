//go:build !windows

package provider

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap/zaptest/observer"
	"golang.org/x/sys/unix"
)

// A keploy.yml that is a FIFO blocked every keploy command in the repo for
// good, in viper's open of it. PreProcessFlags reads it through the loader:
// refused at once, in the CLI's words, the log naming the file and why.
// Mutation: read it with viper.ReadInConfig again.
func TestPreProcessFlagsRefusesAFIFOConfig(t *testing.T) {
	dir := t.TempDir()
	if err := unix.Mkfifo(filepath.Join(dir, "keploy.yml"), 0o644); err != nil {
		t.Fatal(err)
	}
	type result struct {
		logs *observer.ObservedLogs
		err  error
	}
	done := make(chan result, 1)
	go func() {
		_, logs, err := preProcessIn(t, dir, t.TempDir())
		done <- result{logs, err}
	}()
	var r result
	select {
	case r = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("PreProcessFlags did not return within 5s: it blocked on the FIFO")
	}
	if r.err == nil || r.err.Error() != "failed to read config file" {
		t.Fatalf("PreProcessFlags = %v, want \"failed to read config file\"", r.err)
	}
	if got := r.logs.FilterMessage("failed to read config file").All(); len(got) != 1 || !strings.Contains(fmt.Sprint(got[0].ContextMap()["error"]), "keploy.yml: not a regular file: it is a FIFO") {
		t.Errorf("logged %v, want the FIFO named", got)
	}
}
