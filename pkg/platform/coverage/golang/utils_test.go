package golang

import (
	"fmt"
	"go/build"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// keploy test's Go coverage check reads the command's binary, and its lines
// name that binary, never the command: the command's assignments and
// arguments carry what the user passes the app. The binary here is an ELF
// with a symbol table and no coverage symbols (Go's own debug/elf test
// file), as a Go binary built without -cover has.
func TestTheCoverCheckNamesTheBinaryNotTheCommand(t *testing.T) {
	binary := filepath.Join(build.Default.GOROOT, "src", "debug", "elf", "testdata", "gcc-amd64-linux-exec")
	if _, err := os.Stat(binary); err != nil {
		t.Fatalf("the ELF file this test reads: %v", err)
	}
	core, logs := observer.New(zapcore.DebugLevel)
	if checkForCoverFlag(zap.New(core), "PORT=8080 DB_PASSWORD=S3CR3T "+binary+" --db-password S3CR3T") {
		t.Fatal("a binary with no coverage symbols read as built with -cover")
	}
	named := false
	for _, e := range logs.All() {
		line := e.Message + " " + fmt.Sprint(e.ContextMap())
		if strings.Contains(line, "S3CR3T") || strings.Contains(line, "PORT") {
			t.Errorf("a %v line names the command: %s", e.Level, line)
		}
		if e.Message == "go binary was not build with -cover flag" {
			named = e.ContextMap()["file"] == binary
		}
	}
	if !named {
		t.Errorf("no line named the binary %s: %v", binary, logs.All())
	}
}
