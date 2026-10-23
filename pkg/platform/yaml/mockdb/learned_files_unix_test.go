//go:build !windows

package mockdb

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"go.uber.org/zap"
)

// A set whose folder or mocks file keploy may not read is an error, never a
// set with no mocks (Legible would then say it learned nothing).
func TestASetKeployMayNotReadIsAnError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root is refused no permission")
	}
	dir := t.TempDir()
	set := filepath.Join(dir, "test-set-0")
	if err := os.MkdirAll(set, 0o755); err != nil {
		t.Fatal(err)
	}
	mocks := filepath.Join(set, "mocks.yaml")
	if err := os.WriteFile(mocks, []byte("version: api.keploy.io/v1beta1\nkind: Http\nname: mock-0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(mocks, 0o000); err != nil {
		t.Fatal(err)
	}
	if got, err := ReadSetMocks(context.Background(), zap.NewNop(), set); err == nil {
		t.Errorf("a mocks file it may not open: %v", got)
	}
	if err := os.Chmod(mocks, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(set, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(set, 0o755) })
	// The read's error is its own: it logs to the logger it is given, and
	// prints nothing of its own.
	out, err := os.CreateTemp(t.TempDir(), "stdout")
	if err != nil {
		t.Fatal(err)
	}
	stdout := os.Stdout
	os.Stdout = out
	got, rerr := ReadSetMocks(context.Background(), zap.NewNop(), set)
	os.Stdout = stdout
	if rerr == nil {
		t.Errorf("a set folder it may not search: %v", got)
	}
	if b, _ := os.ReadFile(out.Name()); len(b) != 0 {
		t.Errorf("the read printed %q", b)
	}
}
