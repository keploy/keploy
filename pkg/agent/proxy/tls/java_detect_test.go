package tls

import (
	"testing"

	"go.uber.org/zap/zaptest"
)

// TestDetectJavaHomeFromSources_Environ covers the happy-path: when
// /proc/<pid>/environ contains JAVA_HOME, we return it verbatim. This
// is the "SDKMAN or Maven-wrapper set JAVA_HOME" case and is the path
// that fixes the bulk of sap-demo-java-style truststore failures.
func TestDetectJavaHomeFromSources_Environ(t *testing.T) {
	// NUL-separated KEY=VAL as the kernel serialises it.
	environ := []byte("PATH=/usr/bin\x00JAVA_HOME=/opt/jdk-21\x00HOME=/root\x00")
	got := detectJavaHomeFromSources(environ, "")
	if got != "/opt/jdk-21" {
		t.Fatalf("expected /opt/jdk-21, got %q", got)
	}
}

// TestDetectJavaHomeFromSources_Environ_IgnoresOtherVars asserts we
// don't mistakenly match suffix/prefix collisions. MAVEN_JAVA_HOME
// would produce /opt/maven if we naively Contains()'d KEY for
// "JAVA_HOME"; exact key equality guards against that.
func TestDetectJavaHomeFromSources_Environ_IgnoresOtherVars(t *testing.T) {
	environ := []byte("PATH=/usr/bin\x00MAVEN_JAVA_HOME=/opt/maven\x00JAVA_HOME=/opt/jdk-17\x00")
	got := detectJavaHomeFromSources(environ, "")
	if got != "/opt/jdk-17" {
		t.Fatalf("expected /opt/jdk-17, got %q", got)
	}
}

// TestDetectJavaHomeFromSources_Environ_EmptyValueFallsThrough asserts
// that JAVA_HOME= (explicit empty) does NOT return empty but falls
// through to the exe-link heuristic. Some wrapper scripts set
// JAVA_HOME="" deliberately to mean "let the JVM pick the default",
// and honoring that empty value would leave us with no useful path.
func TestDetectJavaHomeFromSources_Environ_EmptyValueFallsThrough(t *testing.T) {
	environ := []byte("PATH=/usr/bin\x00JAVA_HOME=\x00")
	exeLink := "/opt/jdk-11/bin/java"
	got := detectJavaHomeFromSources(environ, exeLink)
	if got != "/opt/jdk-11" {
		t.Fatalf("expected fallback to exe-link /opt/jdk-11, got %q", got)
	}
}

// TestDetectJavaHomeFromSources_ExeFallback covers the fat-jar case:
// no JAVA_HOME in environ, but the app was started via
// /opt/jdk-17/bin/java so the exe symlink reveals the JDK root.
func TestDetectJavaHomeFromSources_ExeFallback(t *testing.T) {
	got := detectJavaHomeFromSources(nil, "/opt/jdk-17/bin/java")
	if got != "/opt/jdk-17" {
		t.Fatalf("expected /opt/jdk-17, got %q", got)
	}
}

// TestDetectJavaHomeFromSources_ExeFallback_JRELayout covers the
// system-package JDK/JRE layout where the leaf dir isn't a simple
// "jdk-N" but a longer path; we just strip /bin/java regardless.
func TestDetectJavaHomeFromSources_ExeFallback_JRELayout(t *testing.T) {
	got := detectJavaHomeFromSources(nil, "/usr/lib/jvm/java-17-openjdk-amd64/bin/java")
	if got != "/usr/lib/jvm/java-17-openjdk-amd64" {
		t.Fatalf("expected /usr/lib/jvm/java-17-openjdk-amd64, got %q", got)
	}
}

// TestDetectJavaHomeFromSources_EnvironWinsOverExe asserts precedence:
// JAVA_HOME from environ takes priority over the exe fallback. This
// matters because some launchers re-exec via a shim: the exe symlink
// points at the shim's JDK while JAVA_HOME is the "canonical" JDK
// the app's classpath was built against.
func TestDetectJavaHomeFromSources_EnvironWinsOverExe(t *testing.T) {
	environ := []byte("JAVA_HOME=/opt/canonical-jdk\x00")
	exeLink := "/opt/shim-jdk/bin/java"
	got := detectJavaHomeFromSources(environ, exeLink)
	if got != "/opt/canonical-jdk" {
		t.Fatalf("expected environ to win (/opt/canonical-jdk), got %q", got)
	}
}

// TestDetectJavaHomeFromSources_BothEmpty verifies the "return empty,
// let caller fall back to PATH keytool" contract — neither source
// yielding a value MUST produce "" rather than a guess.
func TestDetectJavaHomeFromSources_BothEmpty(t *testing.T) {
	if got := detectJavaHomeFromSources(nil, ""); got != "" {
		t.Fatalf("expected empty, got %q", got)
	}
	if got := detectJavaHomeFromSources([]byte{}, ""); got != "" {
		t.Fatalf("expected empty on empty-slice environ, got %q", got)
	}
}

// TestDetectJavaHomeFromSources_ExeNotJava asserts the exe-fallback
// heuristic refuses to infer a JDK from a non-java binary. A Go app
// whose /proc/<pid>/exe resolves to /usr/local/bin/myapp must not
// surface "/usr/local" as a java.home.
func TestDetectJavaHomeFromSources_ExeNotJava(t *testing.T) {
	cases := []string{
		"/usr/bin/bash",
		"/opt/myapp/bin/myapp", // /bin parent but not named java
		"/usr/lib/java/java",   // leaf is "java" but parent not "bin"
	}
	for _, link := range cases {
		if got := detectJavaHomeFromSources(nil, link); got != "" {
			t.Fatalf("exeLink %q should return empty, got %q", link, got)
		}
	}
}

// TestDetectJavaHomeFromSources_ExeJavaExe asserts Windows/WSL
// java.exe is recognised the same way as Linux java. The /proc path
// is Linux-only, but WSL+cross-mount can produce a /proc-like exe
// link that ends in java.exe, and accepting it costs nothing.
func TestDetectJavaHomeFromSources_ExeJavaExe(t *testing.T) {
	got := detectJavaHomeFromSources(nil, "/opt/jdk-21/bin/java.exe")
	if got != "/opt/jdk-21" {
		t.Fatalf("expected /opt/jdk-21, got %q", got)
	}
}

// TestDetectJavaHomeFromSources_DegenerateRoot guards against the
// pathological "/bin/java" case where stripping /bin/java yields
// "/" — that's almost certainly a misread or a broken container,
// and returning "/" would produce /lib/security/cacerts which is
// not a valid JDK cacerts path.
func TestDetectJavaHomeFromSources_DegenerateRoot(t *testing.T) {
	if got := detectJavaHomeFromSources(nil, "/bin/java"); got != "" {
		t.Fatalf("expected empty for degenerate /bin/java, got %q", got)
	}
}

// TestDetectJavaHomeForPID_InvalidPID asserts the public wrapper
// refuses pid<=0 without touching /proc. pid==0 is the sentinel for
// "no app PID known" (e.g. e2e setup harness calling SetupCA
// directly) and must short-circuit to "".
func TestDetectJavaHomeForPID_InvalidPID(t *testing.T) {
	for _, pid := range []int{0, -1, -99999} {
		if got := detectJavaHomeForPID(pid); got != "" {
			t.Fatalf("pid=%d: expected empty, got %q", pid, got)
		}
	}
}

// TestDetectJavaHomeForPID_UnlikelyPID runs the public wrapper
// against an astronomical PID that almost certainly doesn't exist,
// verifying we tolerate the ENOENT/EACCES silently and return "".
// The purpose here is only to exercise the error-tolerant paths —
// the core detection logic is covered by the Sources tests above.
func TestDetectJavaHomeForPID_UnlikelyPID(t *testing.T) {
	// PIDs are capped at pid_max (usually 4194304). 2^30 is above
	// that on every kernel we ship on, so /proc/<pid>/environ will
	// ENOENT and the function should fall through to "".
	got := detectJavaHomeForPID(1 << 30)
	if got != "" {
		t.Fatalf("expected empty for nonexistent PID, got %q", got)
	}
}

// TestResolveAppJavaHome_OverrideWins asserts the --ca-java-home CLI
// flag short-circuits auto-detection. The caller's stored override
// must be honoured even when a sibling /proc path might yield a
// different value.
func TestResolveAppJavaHome_OverrideWins(t *testing.T) {
	logger := zaptest.NewLogger(t)
	got := resolveAppJavaHome(logger, 1, "/opt/override-jdk")
	if got != "/opt/override-jdk" {
		t.Fatalf("expected override to win, got %q", got)
	}
}

// TestResolveAppJavaHome_ZeroPIDNoOverride is the degenerate state at
// agent boot — no app registered, no override. Must return "" so
// SetupCA falls through to legacy PATH-keytool (preserving backward
// compat for pre-ClientNSPID callers like the e2e harness).
func TestResolveAppJavaHome_ZeroPIDNoOverride(t *testing.T) {
	logger := zaptest.NewLogger(t)
	if got := resolveAppJavaHome(logger, 0, ""); got != "" {
		t.Fatalf("expected empty, got %q", got)
	}
}

// TestExtractJavaHomeFromEnviron_MalformedEntries asserts we don't
// panic on garbage environ bytes — a non-KEY=VAL fragment simply
// doesn't match and we continue to the next entry.
func TestExtractJavaHomeFromEnviron_MalformedEntries(t *testing.T) {
	environ := []byte("NOT-AN-ENV-ENTRY\x00\x00JAVA_HOME=/opt/jdk-21\x00")
	if got := extractJavaHomeFromEnviron(environ); got != "/opt/jdk-21" {
		t.Fatalf("expected /opt/jdk-21 despite malformed entries, got %q", got)
	}
}

// TestExtractJavaHomeFromEnviron_TrimsWhitespace catches shell
// scripts that produce `JAVA_HOME= /opt/jdk` with a leading space;
// strings.TrimSpace normalises that so the keytool invocation works.
func TestExtractJavaHomeFromEnviron_TrimsWhitespace(t *testing.T) {
	environ := []byte("JAVA_HOME=  /opt/jdk-21  \x00")
	if got := extractJavaHomeFromEnviron(environ); got != "/opt/jdk-21" {
		t.Fatalf("expected trimmed /opt/jdk-21, got %q", got)
	}
}
