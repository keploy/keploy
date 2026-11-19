//go:build !windows

package util

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// writeJavaTool writes an executable script at dir/name that exits with code.
func writeJavaTool(t *testing.T, dir, name string, code int) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("#!/bin/sh\nexit "+strconv.Itoa(code)+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestAPATHJavaRunsAJVMOnlyAsTheJavaOnPATHDoes pins pathJavaRuns's answer to
// the java on PATH, the one IsJavaInstalled's callers run next. macOS's
// /usr/bin/java and java_home are stood in for by scripts, so the macOS rows
// run on Linux and macOS (the file is not built on Windows).
func TestAPATHJavaRunsAJVMOnlyAsTheJavaOnPATHDoes(t *testing.T) {
	dir := t.TempDir()
	stub := writeJavaTool(t, filepath.Join(dir, "usr-bin"), "java", 1)
	jdkJava := writeJavaTool(t, filepath.Join(dir, "jdk", "bin"), "java", 0)
	homeYes := writeJavaTool(t, filepath.Join(dir, "libexec-jdk"), "java_home", 0)
	homeNo := writeJavaTool(t, filepath.Join(dir, "libexec-none"), "java_home", 1)
	linkDir := filepath.Join(dir, "link-bin")
	if err := os.MkdirAll(linkDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(stub, filepath.Join(linkDir, "java")); err != nil {
		t.Fatal(err)
	}
	noJava := filepath.Join(dir, "empty-bin")
	if err := os.MkdirAll(noJava, 0o755); err != nil {
		t.Fatal(err)
	}
	noStub := filepath.Join(dir, "no-such-stub")

	for _, r := range []struct {
		name, path, goos, stub, javaHome string
		want                             bool
	}{
		{"macOS: no java on PATH, though java_home names a JDK", noJava, "darwin", stub, homeYes, false},
		{"macOS: PATH's java is the stub and java_home names no JDK", filepath.Dir(stub), "darwin", stub, homeNo, false},
		{"macOS: PATH's java links to the stub and java_home names no JDK", linkDir, "darwin", stub, homeNo, false},
		{"macOS: PATH's java is the stub and java_home names a JDK", filepath.Dir(stub), "darwin", stub, homeYes, true},
		{"macOS: PATH's java is a JDK's that java_home does not know", filepath.Dir(jdkJava), "darwin", stub, homeNo, true},
		{"macOS: PATH's java is a JDK's, with no stub on the machine", filepath.Dir(jdkJava), "darwin", noStub, homeNo, true},
		{"Linux: PATH's java runs, java_home not asked", filepath.Dir(stub), "linux", stub, homeNo, true},
		{"Linux: no java on PATH", noJava, "linux", stub, homeYes, false},
	} {
		t.Run(r.name, func(t *testing.T) {
			t.Setenv("PATH", r.path)
			if got := pathJavaRuns(r.goos, r.stub, r.javaHome); got != r.want {
				t.Fatalf("pathJavaRuns(%q) with PATH=%s, stub %s, java_home %s = %v, want %v",
					r.goos, r.path, r.stub, r.javaHome, got, r.want)
			}
		})
	}
}

// TestIsJavaInstalledIsFalseWithNoJavaOnPATH: on a Mac whose java_home names
// a JDK (GitHub's macOS runners have several) as anywhere else.
func TestIsJavaInstalledIsFalseWithNoJavaOnPATH(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	if IsJavaInstalled() {
		t.Fatal("IsJavaInstalled() = true with no java on PATH, want false: its callers run PATH's java next")
	}
}
