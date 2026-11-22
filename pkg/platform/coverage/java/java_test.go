package java

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestBuildJavaCoverageCmd(t *testing.T) {
	tests := []struct {
		name            string
		cmd             string
		executable      string
		jacocoAgentPath string
		goos            string
		want            string
	}{
		{
			name:            "posix quotes javaagent with TESTSETID",
			cmd:             "java -jar app.jar",
			executable:      "java",
			jacocoAgentPath: "/opt/jacoco/agent.jar",
			goos:            "linux",
			want:            `java "-javaagent:/opt/jacoco/agent.jar=destfile=target/${TESTSETID}.exec" -jar app.jar`,
		},
		{
			name:            "posix escapes double-quote special characters in jacocoAgentPath",
			cmd:             "java -jar app.jar",
			executable:      "java",
			jacocoAgentPath: `/tmp/my dir/$HOME/"quoted"` + "`cmd`" + `\end/agent.jar`,
			goos:            "linux",
			want:            `java "-javaagent:/tmp/my dir/\$HOME/\"quoted\"` + "\\`cmd\\`" + `\\end/agent.jar=destfile=target/${TESTSETID}.exec" -jar app.jar`,
		},
		{
			name:            "windows avoids embedded double quotes for cmd /C",
			cmd:             "java -jar app.jar",
			executable:      "java",
			jacocoAgentPath: `C:\jacoco\agent.jar`,
			goos:            "windows",
			want:            `java -javaagent:C:\jacoco\agent.jar=destfile=target/${TESTSETID}.exec -jar app.jar`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := buildJavaCoverageCmd(tc.cmd, tc.executable, tc.jacocoAgentPath, tc.goos)
			if got != tc.want {
				t.Fatalf("buildJavaCoverageCmd() =\n  %q\nwant:\n  %q", got, tc.want)
			}
		})
	}
}

func TestBuildJavaCoverageCmdPOSIXShellSplitting(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX sh test")
	}

	tmpDir := t.TempDir()
	// Create a matching file under target/ so an unquoted glob in TESTSETID would expand.
	targetDir := filepath.Join(tmpDir, "target")
	if err := os.MkdirAll(targetDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(targetDir, "test-set-0.exec"), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	agentPath := `/opt/my agents/a$b"c` + "`d`" + `\agent.jar`
	cases := []struct {
		name      string
		testSetID string
	}{
		{name: "spaces", testSetID: "my test set"},
		{name: "glob", testSetID: "test-set-*"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmdStr := buildJavaCoverageCmd("printargs -jar app.jar", "printargs", agentPath, "linux")
			script := "printargs() { printf '%s\\n' \"$@\"; }; " + cmdStr
			cmd := exec.Command("sh", "-c", script)
			cmd.Dir = tmpDir
			cmd.Env = append(os.Environ(), "TESTSETID="+tc.testSetID)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("sh -c failed: %v\noutput: %s", err, out)
			}

			lines := strings.Split(strings.TrimSuffix(string(out), "\n"), "\n")
			want := []string{
				"-javaagent:" + agentPath + "=destfile=target/" + tc.testSetID + ".exec",
				"-jar",
				"app.jar",
			}
			if len(lines) != len(want) {
				t.Fatalf("argv len = %d (%q), want %d (%q)", len(lines), lines, len(want), want)
			}
			for i := range want {
				if lines[i] != want[i] {
					t.Fatalf("argv[%d] = %q, want %q", i, lines[i], want[i])
				}
			}
		})
	}
}
