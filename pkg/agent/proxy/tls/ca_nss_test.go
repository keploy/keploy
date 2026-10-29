package tls

import (
	"bufio"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// fakeCertutil puts a certutil on PATH (and nothing else) that logs each call's
// arguments, one line each, creates cert9.db for -N, prints list for -L and
// exits with code. It returns the log's path.
func fakeCertutil(t *testing.T, list string, code int) string {
	t.Helper()
	bin := t.TempDir()
	logPath := filepath.Join(t.TempDir(), "calls")
	listPath := filepath.Join(t.TempDir(), "list")
	if err := os.WriteFile(listPath, []byte(list), 0o600); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\nPATH=/usr/bin:/bin\n" +
		"echo \"$*\" >> '" + logPath + "'\n" +
		"for a; do last=$a; done\n" +
		"case \"$1\" in\n" +
		"  -N) : > \"${last#sql:}/cert9.db\" ;;\n" +
		"  -L) cat '" + listPath + "' ;;\n" +
		"esac\n" +
		"exit " + strconv.Itoa(code) + "\n"
	if err := os.WriteFile(filepath.Join(bin, "certutil"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	return logPath
}

func certutilCalls(t *testing.T, logPath string) []string {
	t.Helper()
	b, err := os.ReadFile(logPath)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

func nssTestDir(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the NSS database is written on Linux only")
	}
	return filepath.Join(t.TempDir(), ".pki", "nssdb")
}

func TestNSSInstallCreatesTheDatabaseAndTrustsTheCA(t *testing.T) {
	dir := nssTestDir(t)
	calls := fakeCertutil(t, "", 0)
	installNSSCA(context.Background(), zap.NewNop(), dir, "/ca.crt", "keploy-mitm-run1")
	db := "sql:" + dir
	want := []string{
		"-N --empty-password -d " + db,
		"-L -d " + db,
		"-A -d " + db + " -n keploy-mitm-run1 -t C,, -i /ca.crt",
	}
	if got := certutilCalls(t, calls); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("certutil calls:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	info, err := os.Stat(dir)
	if err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("database directory: %v %v, want a 0700 directory", info, err)
	}
}

func TestNSSInstallKeepsAnExistingDatabase(t *testing.T) {
	dir := nssTestDir(t)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cert9.db"), []byte("the user's"), 0o600); err != nil {
		t.Fatal(err)
	}
	calls := fakeCertutil(t, "", 0)
	installNSSCA(context.Background(), zap.NewNop(), dir, "/ca.crt", "keploy-mitm-run1")
	for _, c := range certutilCalls(t, calls) {
		if strings.HasPrefix(c, "-N") {
			t.Fatalf("an existing database was created anew: %q", c)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "cert9.db")); string(b) != "the user's" {
		t.Fatalf("the existing database was rewritten: %q", b)
	}
}

func TestNSSInstallRemovesOnlyEarlierRunsCAs(t *testing.T) {
	dir := nssTestDir(t)
	list := "\nCertificate Nickname                                         Trust Attributes\n" +
		"                                                             SSL,S/MIME,JAR/XPI\n\n" +
		"keploy-mitm-old                                              C,,\n" +
		"keploy-mitm-run1                                             C,,\n" +
		"Corp Root CA                                                 CT,C,C\n" +
		"keploy-mitm-older                                            C,,\n"
	calls := fakeCertutil(t, list, 0)
	installNSSCA(context.Background(), zap.NewNop(), dir, "/ca.crt", "keploy-mitm-run1")
	var deleted []string
	for _, c := range certutilCalls(t, calls) {
		if strings.HasPrefix(c, "-D") {
			deleted = append(deleted, c)
		}
	}
	db := "sql:" + dir
	want := []string{"-D -d " + db + " -n keploy-mitm-old", "-D -d " + db + " -n keploy-mitm-older"}
	if strings.Join(deleted, "\n") != strings.Join(want, "\n") {
		t.Fatalf("removed:\n%s\nwant:\n%s", strings.Join(deleted, "\n"), strings.Join(want, "\n"))
	}
}

func TestNSSTeardownRemovesTheRunsCA(t *testing.T) {
	dir := nssTestDir(t)
	calls := fakeCertutil(t, "", 0)
	recordNativeInstall(&nativeInstall{alias: "keploy-mitm-run1", nssDir: dir, remove: true})
	TeardownNativeCA(zap.NewNop())
	want := "-D -d sql:" + dir + " -n keploy-mitm-run1"
	got := certutilCalls(t, calls)
	if len(got) != 1 || got[0] != want {
		t.Fatalf("teardown's certutil calls: %q, want [%q]", got, want)
	}
}

func TestNSSWithoutCertutilWarnsOnceAndNamesTheFix(t *testing.T) {
	dir := nssTestDir(t)
	t.Setenv("PATH", t.TempDir())
	nssMissingWarned.Store(false)
	t.Cleanup(func() { nssMissingWarned.Store(false) })
	core, logs := observer.New(zapcore.WarnLevel)
	logger := zap.New(core)
	installNSSCA(context.Background(), logger, dir, "/ca.crt", "keploy-mitm-run1")
	installNSSCA(context.Background(), logger, dir, "/ca.crt", "keploy-mitm-run2")
	all := logs.All()
	if len(all) != 1 {
		t.Fatalf("%d warnings, want one per agent process: %v", len(all), all)
	}
	for _, fix := range []string{"certutil", "libnss3-tools", "nss-tools"} {
		if !strings.Contains(all[0].Message, fix) {
			t.Fatalf("the warning %q does not name %q", all[0].Message, fix)
		}
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("without certutil a database directory was made: %v", err)
	}
}

func TestNSSInstallFailureIsWarnedNotSilent(t *testing.T) {
	dir := nssTestDir(t)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cert9.db"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	fakeCertutil(t, "", 1)
	core, logs := observer.New(zapcore.WarnLevel)
	installNSSCA(context.Background(), zap.New(core), dir, "/ca.crt", "keploy-mitm-run1")
	if logs.FilterMessageSnippet("could not add its CA to the NSS database").Len() != 1 {
		t.Fatalf("a failed add was not warned: %v", logs.All())
	}
}

func TestNSSWithNoHomeIsWarned(t *testing.T) {
	core, logs := observer.New(zapcore.WarnLevel)
	installNSSCA(context.Background(), zap.New(core), "", "/ca.crt", "keploy-mitm-run1")
	if logs.FilterMessageSnippet("found no home directory").Len() != 1 {
		t.Fatalf("no warning without a home: %v", logs.All())
	}
}

func TestNSSDatabaseIsTheAppsUsersHome(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("reads /proc/<pid>/environ")
	}
	home := t.TempDir()
	// The app: a shell that says it runs, then waits on its stdin. Its
	// environ is read once it has said so: a process the exec is still
	// setting up shows an empty one.
	cmd := exec.Command("/bin/sh", "-c", "echo running; read x")
	cmd.Env = []string{"HOME=" + home}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = stdin.Close()
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_ = cmd.Wait()
	})
	if line, err := bufio.NewReader(stdout).ReadString('\n'); err != nil || line != "running\n" {
		t.Fatalf("the app did not start: %q %v", line, err)
	}
	if got, want := nssDBDir(cmd.Process.Pid), filepath.Join(home, ".pki", "nssdb"); got != want {
		t.Fatalf("app's database %q, want %q", got, want)
	}
	agentHome := t.TempDir()
	t.Setenv("HOME", agentHome)
	t.Setenv("SUDO_USER", "")
	if got, want := nssDBDir(0), filepath.Join(agentHome, ".pki", "nssdb"); got != want {
		t.Fatalf("with no app PID: %q, want %q", got, want)
	}
}

func TestOnlyWhatKeployOwnsGoesToTheHomesOwner(t *testing.T) {
	for _, c := range []struct {
		file, euid, home int
		want             bool
	}{
		{file: 0, euid: 0, home: 1000, want: true},     // keploy under sudo made it
		{file: 1000, euid: 0, home: 1000, want: false}, // the user's own database
		{file: 0, euid: 0, home: 0, want: false},       // root's own home
		{file: 1000, euid: 1000, home: 1000, want: false},
		{file: 1001, euid: 0, home: 1000, want: false}, // another user's file: not keploy's
	} {
		if got := needsChown(c.file, c.euid, c.home); got != c.want {
			t.Errorf("needsChown(file %d, euid %d, home %d) = %v, want %v", c.file, c.euid, c.home, got, c.want)
		}
	}
}

// TestNSSRealCertutil adds a CA with NSS's own certutil, where it is installed,
// and checks that the database trusts it and that teardown removes it.
func TestNSSRealCertutil(t *testing.T) {
	if _, err := exec.LookPath("certutil"); err != nil || runtime.GOOS != "linux" {
		t.Skip("NSS certutil is not installed")
	}
	dir := nssTestDir(t)
	certPath := filepath.Join(t.TempDir(), "ca.crt")
	if err := os.WriteFile(certPath, testCACertPEM(), 0o600); err != nil {
		t.Fatal(err)
	}
	installNSSCA(context.Background(), zap.NewNop(), dir, certPath, "keploy-mitm-real")
	out, err := exec.Command("certutil", "-L", "-d", "sql:"+dir, "-n", "keploy-mitm-real").CombinedOutput()
	if err != nil || !strings.Contains(string(out), "Valid CA") {
		t.Fatalf("the database does not trust the CA: %v\n%s", err, out)
	}
	removeNSSCA(context.Background(), zap.NewNop(), dir, "keploy-mitm-real")
	if err := exec.Command("certutil", "-L", "-d", "sql:"+dir, "-n", "keploy-mitm-real").Run(); err == nil {
		t.Fatal("teardown left the CA in the database")
	}
}

// TestNativeInstallAddsTheCAToTheAppUsersNSSDatabase runs the native Linux
// install end to end (a temporary trust store, a fake certutil) and checks that
// it adds the run's anchor to the NSS database in the app user's home before
// the CA is ready, and that teardown removes it from there.
func TestNativeInstallAddsTheCAToTheAppUsersNSSDatabase(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the trust-store anchors are Linux's")
	}
	calls := fakeCertutil(t, "", 0)
	store, bin, home := t.TempDir(), t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "keploy-test-update-ca"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", os.Getenv("PATH")+":"+bin)
	t.Setenv("JAVA_HOME", "")
	t.Setenv("HOME", home)
	t.Setenv("SUDO_USER", "")
	for _, k := range []string{"NODE_EXTRA_CA_CERTS", "REQUESTS_CA_BUNDLE", "SSL_CERT_FILE", "CARGO_HTTP_CAINFO"} {
		t.Setenv(k, "")
	}
	oldPaths, oldCmds := caStorePath, caStoreUpdateCmd
	caStorePath, caStoreUpdateCmd = []string{store + "/"}, []string{"keploy-test-update-ca"}
	t.Cleanup(func() {
		caStorePath, caStoreUpdateCmd = oldPaths, oldCmds
		recordNativeInstall(nil)
		ResetCAReadyForTest()
	})

	ca, err := generateRunCA()
	if err != nil {
		t.Fatal(err)
	}
	if err := setupNativeForApp(context.Background(), zap.NewNop(), ca, nil, 0, ""); err != nil {
		t.Fatal(err)
	}
	db := "sql:" + filepath.Join(home, ".pki", "nssdb")
	add := "-A -d " + db + " -n " + runAlias(ca) + " -t C,, -i " + filepath.Join(store, runCertFileName(ca))
	if got := certutilCalls(t, calls); len(got) == 0 || got[len(got)-1] != add {
		t.Fatalf("install's certutil calls: %q, want the last to be %q", got, add)
	}
	TeardownNativeCA(zap.NewNop())
	del := "-D -d " + db + " -n " + runAlias(ca)
	if got := certutilCalls(t, calls); got[len(got)-1] != del {
		t.Fatalf("teardown's certutil calls: %q, want the last to be %q", got, del)
	}
}
