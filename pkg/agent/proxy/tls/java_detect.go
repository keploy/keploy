// Package tls — java_detect.go implements app-aware java.home detection.
//
// Motivation: installJavaCA installs the Keploy MITM CA into a JDK
// truststore by shelling out to `keytool`. Historically it resolved
// `keytool` from PATH, which is the shell's current Java — NOT
// necessarily the JDK the app under instrumentation is actually running
// with. Three common divergences:
//
//   - SDKMAN installs multiple JDKs side-by-side; the "current" one for
//     the shell is controlled by the sdkman shim and may differ from the
//     one the app launcher embedded via JAVA_HOME.
//   - Maven/Gradle wrappers (./mvnw, ./gradlew) set their OWN JAVA_HOME
//     at wrapper boot based on .mvn/jvm.config or toolchains.xml, so the
//     child java process is NOT the PATH java.
//   - Spring Boot fat-jars started via an absolute JDK path (e.g.
//     /opt/jdk-21/bin/java -jar app.jar) have no JAVA_HOME in env but
//     the exe symlink pins the JDK.
//
// When the app's JDK != PATH JDK, keytool -import lands in the wrong
// cacerts and the app's TLS stack rejects the Keploy MITM handshake
// with "unable to find valid certification path" — the JDBC PG driver
// drops to EOF mid-handshake, which customers work around with
// ?sslmode=disable (see sap-demo-java PR #127).
//
// The fix in this file: given the app's PID, read /proc/<pid>/environ
// for JAVA_HOME, falling back to /proc/<pid>/exe's symlink target. The
// caller (installJavaCA via installJavaCAForHome) then passes the
// detected path to keytool -keystore $javaHome/lib/security/cacerts,
// using $javaHome/bin/keytool as the executable, so the import lands
// in the app's actual truststore regardless of what's on PATH.
package tls

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"go.uber.org/zap"
)

// resolveAppJavaHome picks the java.home SetupCA should target for the
// Keploy MITM CA truststore install. Precedence, highest to lowest:
//
//  1. override — the --ca-java-home CLI flag
//     (config.Agent.CAJavaHome). Operator-set, wins over everything
//     else. Used when the auto-detector can't see through an exotic
//     launcher (custom shell wrapper, container re-exec, etc.).
//
//  2. detectJavaHomeForPID(appPID) — /proc-based auto-detect. Covers
//     the common cases (SDKMAN, Maven wrapper, fat-jar launch).
//
//  3. "" — caller falls back to PATH keytool. Correct on single-JDK
//     hosts and backward-compatible with pre-Track-C behaviour.
//
// The logger is written at Debug so operators can grep the agent log
// for "ca_java_home_source" and see exactly which path was chosen
// without turning on trace-level noise.
func resolveAppJavaHome(logger *zap.Logger, appPID int, override string) string {
	if override != "" {
		logger.Debug("using --ca-java-home override for Java truststore install",
			zap.String("ca_java_home_source", "override"),
			zap.String("java_home", override),
			zap.Int("app_pid", appPID))
		return override
	}
	if appPID > 0 {
		if detected := detectJavaHomeForPID(appPID); detected != "" {
			logger.Debug("auto-detected app's java.home from /proc",
				zap.String("ca_java_home_source", "proc"),
				zap.String("java_home", detected),
				zap.Int("app_pid", appPID))
			return detected
		}
	}
	logger.Debug("no app-aware java.home available; falling back to PATH keytool",
		zap.String("ca_java_home_source", "path"),
		zap.Int("app_pid", appPID))
	return ""
}

// detectJavaHomeForPID resolves the java.home of the process with the
// given PID by consulting /proc/<pid>/environ first, then
// /proc/<pid>/exe as a fallback.
//
// Returns "" when detection fails. This is never an error condition
// that should abort CA install — the caller falls back to the legacy
// PATH-resolved keytool behaviour, which is either also correct
// (common single-JDK hosts) or silently wrong in the same way it has
// always been.
//
// Permission model: /proc/<pid>/environ is readable only by the
// process's uid (or CAP_SYS_PTRACE). The agent attaches eBPF to the
// same PID it's trying to read here, which itself requires
// CAP_SYS_ADMIN/CAP_BPF and is per-namespace, so in practice we have
// access. We still tolerate ENOENT/EACCES silently — a non-readable
// environ is not a bug, just a degraded-detection case.
//
// pid==0 is a sentinel for "no app PID known" (e.g. the agent boots
// before any client has registered) — we short-circuit to "" rather
// than accidentally reading /proc/0/environ.
func detectJavaHomeForPID(pid int) string {
	if pid <= 0 {
		return ""
	}

	environPath := "/proc/" + strconv.Itoa(pid) + "/environ"
	exePath := "/proc/" + strconv.Itoa(pid) + "/exe"

	// #nosec G304 -- environPath is constructed from an int PID the
	// agent already has permission to observe via eBPF attach; this
	// is not user-controlled input. The read is also tolerant of
	// permission errors (see detectJavaHomeFromSources).
	environ, _ := os.ReadFile(environPath)

	exeLink, _ := os.Readlink(exePath)

	return detectJavaHomeFromSources(environ, exeLink)
}

// detectJavaHomeFromSources is the testable core of detectJavaHomeForPID —
// it takes the two raw inputs (environ bytes, exe symlink target) and
// returns the resolved java.home or "".
//
// Resolution order:
//  1. JAVA_HOME from environ (NUL-separated KEY=VAL entries) — this
//     is what SDKMAN, Maven wrapper, and explicit-env launches set.
//     JAVA_HOME wins because it's what the JVM itself uses to resolve
//     cacerts at startup; matching it guarantees we write to the
//     right truststore.
//  2. Parent of bin/java from the exe symlink — handles the
//     absolute-path launch case where no JAVA_HOME is set. If
//     exeLink ends in /bin/java (or /bin/java.exe on Windows/WSL),
//     trimming the final two path components gives the JDK root
//     whose lib/security/cacerts is the right target.
//
// Both empty → "" → caller falls back to PATH keytool.
func detectJavaHomeFromSources(environ []byte, exeLink string) string {
	if javaHome := extractJavaHomeFromEnviron(environ); javaHome != "" {
		return javaHome
	}
	if javaHome := javaHomeFromExeLink(exeLink); javaHome != "" {
		return javaHome
	}
	return ""
}

// extractJavaHomeFromEnviron walks NUL-separated KEY=VAL entries and
// returns the value of JAVA_HOME, or "" if absent.
//
// /proc/<pid>/environ uses NUL separators (not newlines) because
// execve's envp is a NUL-terminated string array and the kernel
// preserves that layout byte-for-byte. A newline-based split would
// silently return "" on every Linux host.
func extractJavaHomeFromEnviron(environ []byte) string {
	if len(environ) == 0 {
		return ""
	}
	for _, entry := range bytes.Split(environ, []byte{0}) {
		if len(entry) == 0 {
			continue
		}
		// bytes.Cut avoids allocating a slice of KEY and VAL
		// separately; we only care about the JAVA_HOME entry.
		k, v, ok := bytes.Cut(entry, []byte{'='})
		if !ok {
			continue
		}
		if string(k) == "JAVA_HOME" {
			val := strings.TrimSpace(string(v))
			if val == "" {
				// Explicit JAVA_HOME= (empty) is a deliberate
				// "unset" signal in some wrapper scripts; treat
				// it the same as absent so we fall through to
				// the exe-link heuristic.
				return ""
			}
			return val
		}
	}
	return ""
}

// javaHomeFromExeLink infers java.home from the /proc/<pid>/exe
// symlink target. Returns "" if the link is empty or doesn't look
// like a JDK java executable.
//
// Examples:
//
//	/opt/jdk-21/bin/java      → /opt/jdk-21
//	/usr/lib/jvm/java-17/bin/java → /usr/lib/jvm/java-17
//	/usr/bin/bash             → ""  (not a java exe)
//	""                        → ""
//
// We require the immediate parent dir to be named "bin" AND the leaf
// to be "java" (or "java.exe" for cross-platform robustness). Anything
// else — custom launchers, wrapper shells, rename-stripped
// deployments — returns "" rather than guessing wrong.
func javaHomeFromExeLink(exeLink string) string {
	if exeLink == "" {
		return ""
	}
	// filepath.Base is case-sensitive on Linux. On Windows /proc
	// doesn't exist, so this helper is effectively Linux-only; we
	// still accept java.exe defensively in case a future cygwin/WSL
	// path exercises it.
	base := filepath.Base(exeLink)
	if base != "java" && base != "java.exe" {
		return ""
	}
	parent := filepath.Dir(exeLink) // .../bin
	if filepath.Base(parent) != "bin" {
		return ""
	}
	javaHome := filepath.Dir(parent) // .../jdk-root
	if javaHome == "." || javaHome == "/" {
		// Degenerate — a java binary at /bin/java would imply the
		// JDK root is "/", which is almost certainly wrong and
		// definitely not where cacerts lives. Refuse rather than
		// produce a misleading result.
		return ""
	}
	return javaHome
}
