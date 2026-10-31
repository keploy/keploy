//go:build unix

package replayer

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	vtlog "vitess.io/vitess/go/vt/log"
)

// vitessLogMarker stands for customer content: a card number in a recorded DDL.
const vitessLogMarker = "4111-1111-1111-1111"

// A DDL vitess only partly parses. Matching it on replay parses it twice
// (getQueryStructureCached, computeQueryShape), and vitess's Parse2 logs
// "ignoring error parsing DDL '<the whole statement>'" each time.
const vitessPartialDDL = "create table customers (card varchar(19) default '" + vitessLogMarker + "', c garbage)"

const vitessLogChildEnv = "KEPLOY_TEST_REPLAYER_VITESSLOG_CHILD"

// Replaying a recorded DDL vitess only partly parses, in a process whose
// TMPDIR is empty, leaves that directory empty and prints no line that holds
// the statement. Before the vitesslog import, glog wrote
// replayer.test.<host>.<user>.log.WARNING... (and .INFO) there with the
// whole statement in it.
func TestReplayOfPartlyParsedDDLWritesNoFileAndPrintsNoStatement(t *testing.T) {
	if os.Getenv(vitessLogChildEnv) == "1" {
		t.Skip("the child runs TestReplayOfPartlyParsedDDLChild only")
	}
	tmp := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestReplayOfPartlyParsedDDLChild$", "-test.count=1", "-test.v")
	cmd.Env = append(os.Environ(), vitessLogChildEnv+"=1", "TMPDIR="+tmp)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var out strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) })
	if err := cmd.Wait(); err != nil {
		t.Fatalf("child: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "replayer-vitesslog-child-done") {
		t.Fatalf("child did not finish:\n%s", out.String())
	}
	if strings.Contains(out.String(), vitessLogMarker) {
		t.Errorf("child printed the statement:\n%s", out.String())
	}
	_ = filepath.WalkDir(tmp, func(path string, d fs.DirEntry, err error) error {
		if err != nil || path == tmp {
			return err
		}
		body, _ := os.ReadFile(path)
		t.Errorf("replay wrote %s (holds the statement: %v)", path, strings.Contains(string(body), vitessLogMarker))
		return nil
	})
}

func TestReplayOfPartlyParsedDDLChild(t *testing.T) {
	if os.Getenv(vitessLogChildEnv) != "1" {
		t.Skip("run by TestReplayOfPartlyParsedDDLWritesNoFileAndPrintsNoStatement")
	}
	if _, err := getQueryStructureCached(vitessPartialDDL); err != nil {
		t.Fatalf("getQueryStructureCached(%q) = %v, want the partly parsed DDL's structure", vitessPartialDDL, err)
	}
	if shape := computeQueryShape(vitessPartialDDL); shape.template == "" {
		t.Fatalf("computeQueryShape(%q) found no template: the partly parsed path was not taken", vitessPartialDDL)
	}
	vtlog.Flush() // so a leaked line is in its file, not only in glog's buffer
	fmt.Println("replayer-vitesslog-child-done")
}
