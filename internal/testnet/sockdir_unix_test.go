//go:build unix

package testnet

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// A socket with a 12-byte name binds in a test's socket directory under a
// GOTMPDIR too long for one in its t.TempDir.
func TestASocketInASocketDirBindsUnderALongGOTMPDIR(t *testing.T) {
	long := filepath.Join(t.TempDir(), strings.Repeat("d", 100))
	if err := os.Mkdir(long, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GOTMPDIR", long)
	sock := filepath.Join(SocketDir(t), "outside.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen at %s (%d bytes): %v", sock, len(sock), err)
	}
	_ = ln.Close()
}

// fatalTB records the test's failure that SocketDir says, and ends the
// goroutine it is said on, as t.Fatalf does.
type fatalTB struct {
	testing.TB
	fatal string
}

func (f *fatalTB) Fatalf(format string, args ...any) {
	f.fatal = fmt.Sprintf(format, args...)
	runtime.Goexit()
}

// Where no directory can be made under os.TempDir, SocketDir fails the test
// naming why, and returns no path to put a socket at.
func TestASocketDirThatCannotBeMadeFailsTheTest(t *testing.T) {
	notADir := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(notADir, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", notADir)
	f := &fatalTB{TB: t}
	returned := make(chan string, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		returned <- SocketDir(f)
	}()
	<-done
	select {
	case dir := <-returned:
		t.Errorf("SocketDir returned %q under a TMPDIR that is a file", dir)
	default:
	}
	if !strings.Contains(f.fatal, "unix sockets") || !strings.Contains(f.fatal, notADir) {
		t.Errorf("SocketDir failed the test with %q; want why, naming %s", f.fatal, notADir)
	}
}
