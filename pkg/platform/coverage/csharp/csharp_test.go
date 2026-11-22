package csharp

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestBuildCsharpCoverageCmd(t *testing.T) {
	tests := []struct {
		name       string
		cmd        string
		executable string
		goos       string
		want       string
	}{
		{
			name:       "posix quotes output path with TESTSETID",
			cmd:        "dotnet-coverage dotnet MyApp.dll",
			executable: "dotnet-coverage",
			goos:       "linux",
			want:       `dotnet-coverage collect --output "target/${TESTSETID}.cobertura" --output-format cobertura dotnet MyApp.dll`,
		},
		{
			name:       "windows avoids embedded double quotes for cmd /C",
			cmd:        "dotnet-coverage dotnet MyApp.dll",
			executable: "dotnet-coverage",
			goos:       "windows",
			want:       `dotnet-coverage collect --output target/${TESTSETID}.cobertura --output-format cobertura dotnet MyApp.dll`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := buildCsharpCoverageCmd(tc.cmd, tc.executable, tc.goos)
			if got != tc.want {
				t.Fatalf("buildCsharpCoverageCmd() =\n  %q\nwant:\n  %q", got, tc.want)
			}
		})
	}
}

func TestBuildCsharpCoverageCmdPOSIXShellSplitting(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX sh test")
	}

	tmpDir := t.TempDir()
	targetDir := filepath.Join(tmpDir, "target")
	if err := os.MkdirAll(targetDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(targetDir, "test-set-0.cobertura"), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name      string
		testSetID string
	}{
		{name: "spaces", testSetID: "my test set"},
		{name: "glob", testSetID: "test-set-*"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmdStr := buildCsharpCoverageCmd("printargs dotnet MyApp.dll", "printargs", "linux")
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
				"collect",
				"--output",
				"target/" + tc.testSetID + ".cobertura",
				"--output-format",
				"cobertura",
				"dotnet",
				"MyApp.dll",
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
