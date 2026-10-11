//go:build linux

package tls

import (
	"bufio"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// These tests run the native Linux CA install and teardown — setupNativeForApp,
// TeardownNativeCA, the legacy sweep — against a trust store built in a temp
// directory, with more than one agent in play. Where a test needs a second
// agent that is really alive, or really dead, it is another process (the helper
// in truststore_linux_test.go). Nothing here touches the machine's own store or
// JDK, and nothing waits on the clock.

// ---------------------------------------------------------------------------
// a trust store in a temp directory
// ---------------------------------------------------------------------------

// fakeTrustStore is everything the native install touches, rooted in one
// directory: the anchor directory agents write into, the bundle a distro tool
// would regenerate from it, that tool, a JDK, and the directory of the
// trust-store lock and the run claims.
type fakeTrustStore struct {
	root       string
	anchors    string // where agents put keploy-mitm-<run id>.crt
	bundle     string // what applications read: every anchor, concatenated
	tool       string // stands in for update-ca-certificates
	toolLog    string // one line per run of the tool
	started    string // a FIFO the tool reports its pid to, when it exists
	gate       string // a FIFO the tool waits on before it regenerates, when it exists
	javaHome   string // a JDK whose keytool only records how it was called
	keytoolLog string
	exported   string // what that keytool prints for -exportcert, when present
	state      string // stands in for /run/keploy
}

func fakeTrustStoreAt(root string) *fakeTrustStore {
	return &fakeTrustStore{
		root:       root,
		anchors:    filepath.Join(root, "anchors"),
		bundle:     filepath.Join(root, "ca-bundle.crt"),
		tool:       filepath.Join(root, "regenerate-bundle"),
		toolLog:    filepath.Join(root, "regenerate-bundle.log"),
		started:    filepath.Join(root, "tool-started"),
		gate:       filepath.Join(root, "tool-gate"),
		javaHome:   filepath.Join(root, "jdk"),
		keytoolLog: filepath.Join(root, "keytool.log"),
		exported:   filepath.Join(root, "keytool-exportcert.pem"),
		state:      filepath.Join(root, "run", "keploy"),
	}
}

// newFakeTrustStore builds a store and makes it the one this process installs
// into, for the length of the test.
func newFakeTrustStore(t *testing.T) *fakeTrustStore {
	t.Helper()
	s := fakeTrustStoreAt(t.TempDir())

	for _, dir := range []string{s.anchors, filepath.Join(s.javaHome, "bin"), filepath.Join(s.javaHome, "lib", "security")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// The tool does what the distro tools do, in the order that makes running
	// two at once unsafe: read the anchors that exist now, replace the bundle
	// at the end. It also refuses to overlap with itself, so a regeneration
	// that runs outside the trust-store lock fails whichever test it is in.
	// When the test has made the two FIFOs, it reports its pid on one and
	// waits on the other before regenerating: that is how a test catches it
	// mid-run. It logs "done" last, so a run that was killed is one without.
	tool := fmt.Sprintf(`#!/bin/sh
set -eu
if ! mkdir %[1]q 2>/dev/null; then
	echo "overlap" >> %[2]q
	exit 1
fi
echo "run $*" >> %[2]q
if [ -p %[5]q ] && [ -p %[6]q ]; then
	echo $$ > %[5]q
	read _ < %[6]q || true
	echo "Updating certificates..."
fi
tmp=%[3]q.$$
: > "$tmp"
for f in %[4]q/*.crt; do
	if [ -f "$f" ]; then cat "$f" >> "$tmp"; fi
done
mv -f "$tmp" %[3]q
rmdir %[1]q
echo "done" >> %[2]q
`, filepath.Join(s.root, "regenerating"), s.toolLog, s.bundle, s.anchors, s.started, s.gate)
	// -list fails ("no such alias"), which is what makes the install import.
	keytool := fmt.Sprintf(`#!/bin/sh
echo "$*" >> %[1]q
case "$1" in
-list) exit 1 ;;
-exportcert) if [ -f %[2]q ]; then cat %[2]q; fi ;;
esac
exit 0
`, s.keytoolLog, s.exported)
	for path, body := range map[string]string{
		s.tool: tool,
		filepath.Join(s.javaHome, "bin", "keytool"): keytool,
	} {
		if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(s.javaHome, "lib", "security", "cacerts"), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	prevPaths, prevCmd, prevState := caStorePath, caStoreUpdateCmd, trustStateDir
	s.point()
	t.Cleanup(func() {
		// Whatever the test left installed must not hold its claim into the
		// next test.
		if n := takeNativeInstall(); n != nil {
			n.withdrawAnchors(zap.NewNop())
			n.releaseClaim()
		}
		caStorePath, caStoreUpdateCmd, trustStateDir = prevPaths, prevCmd, prevState
		ResetCAReadyForTest()
	})

	// The teardown and the legacy sweep find their JDK through JAVA_HOME, and
	// the legacy sweep looks through the temp dir.
	t.Setenv("JAVA_HOME", s.javaHome)
	t.Setenv("TMPDIR", t.TempDir())
	// The install points these at its anchor; t.Setenv puts them back.
	for _, k := range []string{"NODE_EXTRA_CA_CERTS", "REQUESTS_CA_BUNDLE", "SSL_CERT_FILE", "CARGO_HTTP_CAINFO"} {
		t.Setenv(k, "")
	}
	return s
}

// point makes this store the one the package installs into.
func (s *fakeTrustStore) point() {
	caStorePath = []string{s.anchors + "/"}
	caStoreUpdateCmd = []string{s.tool}
	trustStateDir = s.state
}

// install runs the native install in this process, as one agent would at start.
func (s *fakeTrustStore) install(logger *zap.Logger) (*caState, error) {
	ca, err := generateRunCA()
	if err != nil {
		return nil, err
	}
	return ca, setupNativeForApp(context.Background(), logger, ca, &ephemeralStore{}, 0, s.javaHome)
}

func (s *fakeTrustStore) mustInstall(t *testing.T) *caState {
	t.Helper()
	ca, err := s.install(zap.NewNop())
	if err != nil {
		t.Fatalf("native CA install failed: %v", err)
	}
	s.mustBeUnlocked(t, "the install")
	return ca
}

// teardown runs TeardownNativeCA in this process, as one agent would at stop.
func (s *fakeTrustStore) teardown(t *testing.T) {
	t.Helper()
	TeardownNativeCA(zap.NewNop())
	s.mustBeUnlocked(t, "the teardown")
}

// mustBeUnlocked fails the test if what just returned left this process
// holding the trust-store lock — asked without waiting, so that a lock that is
// never given back fails here instead of hanging whatever comes next.
func (s *fakeTrustStore) mustBeUnlocked(t *testing.T, what string) {
	t.Helper()
	if holdsTrustStoreLockFile() {
		t.Fatalf("%s returned with the trust-store lock still held", what)
	}
}

// holdsTrustStoreLockFile reports whether this process still has the
// trust-store lock file open, which is the only way it can hold the lock. It
// looks at this process's own descriptors, so another agent changing the store
// at the same moment does not count.
func holdsTrustStoreLockFile() bool {
	want := trustStoreLockPath()
	resolved, _ := filepath.EvalSymlinks(want)
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return false
	}
	for _, e := range entries {
		if target, err := os.Readlink(filepath.Join("/proc/self/fd", e.Name())); err == nil && (target == want || target == resolved) {
			return true
		}
	}
	return false
}

func (s *fakeTrustStore) anchor(runID string) string {
	return filepath.Join(s.anchors, "keploy-mitm-"+runID+".crt")
}

// anchorFiles lists every run anchor in the store, whoever owns it.
func (s *fakeTrustStore) anchorFiles(t *testing.T) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(s.anchors, "keploy-mitm-*"))
	if err != nil {
		t.Fatal(err)
	}
	return matches
}

// bundleHas reports whether the regenerated bundle — what an application reads
// — carries the CA of the given run.
func (s *fakeTrustStore) bundleHas(t *testing.T, runID string) bool {
	t.Helper()
	data, err := os.ReadFile(s.bundle)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	for {
		var block *pem.Block
		block, data = pem.Decode(data)
		if block == nil {
			return false
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			t.Fatalf("the bundle holds something that is not a certificate: %v", err)
		}
		if cert.Subject.CommonName == caCNPrefix+runID {
			return true
		}
	}
}

// regenerations returns how many times the tool has started to rebuild the
// bundle, and fails the test if two runs of it ever overlapped.
func (s *fakeTrustStore) regenerations(t *testing.T) int {
	t.Helper()
	return s.countLogLines(t, s.toolLog, "run")
}

// completedRegenerations returns how many runs of the tool got to the end.
func (s *fakeTrustStore) completedRegenerations(t *testing.T) int {
	t.Helper()
	return s.countLogLines(t, s.toolLog, "done")
}

// gateTheTool makes the tool's next run report its start on s.started and wait
// on s.gate (see openGate) before it regenerates.
func (s *fakeTrustStore) gateTheTool(t *testing.T) {
	t.Helper()
	mkfifo(t, s.root, filepath.Base(s.started))
	mkfifo(t, s.root, filepath.Base(s.gate))
}

// claimed reports whether the run with this id is live: its claim held.
func (s *fakeTrustStore) claimed(t *testing.T, runID string) bool {
	t.Helper()
	live, err := runIsLive(runID)
	if err != nil {
		t.Fatal(err)
	}
	return live
}

func (s *fakeTrustStore) countLogLines(t *testing.T, log, prefix string) int {
	t.Helper()
	data, err := os.ReadFile(log)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	n := 0
	for _, line := range strings.Split(string(data), "\n") {
		if line == "overlap" {
			t.Fatalf("two bundle regenerations ran at the same time:\n%s", data)
		}
		if strings.HasPrefix(line, prefix) {
			n++
		}
	}
	return n
}

// handshake is the application's view of an agent: a TLS client whose only
// roots are the store's bundle connects to the agent, which answers with a leaf
// signed by its run CA. An error is the client refusing that leaf.
func (s *fakeTrustStore) handshake(port int) error {
	roots := x509.NewCertPool()
	if data, err := os.ReadFile(s.bundle); err == nil {
		roots.AppendCertsFromPEM(data)
	}
	conn, err := tls.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), &tls.Config{RootCAs: roots, ServerName: "localhost"})
	if err != nil {
		return err
	}
	return conn.Close()
}

// ---------------------------------------------------------------------------
// a second agent, in a process of its own
// ---------------------------------------------------------------------------

// helperAgent is one keploy agent's CA lifecycle against the store at root: it
// installs its run CA, serves TLS with leaves that CA signs, and removes the CA
// when told to stop. A helper that is killed instead removes nothing.
func helperAgent(root string) int {
	s := fakeTrustStoreAt(root)
	s.point()
	if err := os.Setenv("JAVA_HOME", s.javaHome); err != nil {
		fmt.Printf("FAILED %v\n", err)
		return 1
	}
	logger := zap.NewNop()

	ca, err := generateRunCA()
	if err != nil {
		fmt.Printf("FAILED %v\n", err)
		return 1
	}
	setActiveCA(ca)
	if err := setupNativeForApp(context.Background(), logger, ca, &ephemeralStore{}, 0, s.javaHome); err != nil {
		fmt.Printf("FAILED %v\n", err)
		return 1
	}
	// An agent that kept the lock would stall every other agent of the test
	// for as long as it lives; say so, instead of leaving the test to wait.
	if holdsTrustStoreLockFile() {
		fmt.Println("FAILED the install returned with the trust-store lock still held")
		return 1
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Printf("FAILED %v\n", err)
		return 1
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = conn.Close() }()
				// The proxy's own handshake: a leaf for the client's SNI,
				// signed by the active run CA.
				tlsConn, _, err := HandleTLSConnection(context.Background(), logger, conn, time.Time{})
				if err != nil {
					return
				}
				_, _ = io.Copy(io.Discard, tlsConn)
				_ = tlsConn.Close()
			}()
		}
	}()
	fmt.Printf("READY %d %s\n", ln.Addr().(*net.TCPAddr).Port, ca.runID)

	in := bufio.NewScanner(os.Stdin)
	for in.Scan() {
		if in.Text() == "stop" {
			TeardownNativeCA(logger)
			if holdsTrustStoreLockFile() {
				fmt.Println("FAILED the teardown returned with the trust-store lock still held")
				return 1
			}
			fmt.Println("STOPPED")
			return 0
		}
	}
	return 0
}

type agentProc struct {
	*helperProc
	port  int
	runID string
}

// startAgent starts an agent in its own process and returns once its CA is
// installed and it is serving TLS.
func startAgent(t *testing.T, s *fakeTrustStore) *agentProc {
	t.Helper()
	h := startHelper(t, "agent", helperStoreEnv+"="+s.root)
	return awaitAgent(t, h)
}

func awaitAgent(t *testing.T, h *helperProc) *agentProc {
	t.Helper()
	fields := h.expect("READY")
	if len(fields) != 2 {
		t.Fatalf("malformed READY line: %v", fields)
	}
	port, err := strconv.Atoi(fields[0])
	if err != nil {
		t.Fatalf("malformed READY line: %v", fields)
	}
	return &agentProc{helperProc: h, port: port, runID: fields[1]}
}

// stop shuts the agent down cleanly and returns once it has removed its CA and
// exited.
func (a *agentProc) stop() {
	a.t.Helper()
	if _, err := io.WriteString(a.stdin, "stop\n"); err != nil {
		a.t.Fatalf("asking the agent to stop: %v", err)
	}
	a.expect("STOPPED")
	a.done = true
	if err := a.cmd.Wait(); err != nil {
		a.t.Fatalf("the agent did not exit cleanly: %v", err)
	}
}

// queued runs fn, which must need the trust-store lock, while the test holds
// that lock — the test standing in for another agent in the middle of changing
// the store — and returns once fn is queueing for it. If fn finishes instead,
// it changed the store without waiting its turn, and the test fails. The
// function returned lets fn have the lock and waits for it to finish.
func queued(t *testing.T, what string, fn func(logger *zap.Logger)) (finish func()) {
	t.Helper()
	unlock := mustLockTrustStore(t)
	logger, busy, _ := busyLogger()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn(logger)
	}()
	// However the test goes on from here, the lock is released and fn has
	// finished before the next test starts.
	var once sync.Once
	finish = func() {
		once.Do(func() {
			unlock()
			<-done
		})
	}
	t.Cleanup(finish)
	select {
	case <-busy:
	case <-done:
		t.Fatalf("%s did not wait for another agent that was changing the trust store", what)
	}
	return finish
}

// ---------------------------------------------------------------------------
// two runs that overlap
// ---------------------------------------------------------------------------

// TestNativeInstall_SecondAgentLeavesTheFirstTrusted is the bug: two keploy
// runs overlap on one machine, and the second agent's start must not take the
// first agent's CA out of the system trust store. It used to — the sweep at
// start removed every keploy-mitm-*.crt but its own — and an application of the
// first run that reads only the system store then failed its next handshake
// with keploy's proxy.
func TestNativeInstall_SecondAgentLeavesTheFirstTrusted(t *testing.T) {
	s := newFakeTrustStore(t)

	first := startAgent(t, s)
	if err := s.handshake(first.port); err != nil {
		t.Fatalf("the first agent is not trusted through the system store while it runs alone: %v", err)
	}

	// This process is the second agent, starting while the first is running.
	second := s.mustInstall(t)

	if !exists(s.anchor(first.runID)) {
		t.Error("the second agent's start removed the anchor of the first agent, which is still running")
	}
	if !s.bundleHas(t, first.runID) {
		t.Error("the bundle regenerated by the second agent no longer carries the first agent's CA")
	}
	if err := s.handshake(first.port); err != nil {
		t.Errorf("after a second agent started, a client that trusts only the system store refuses the first agent: %v", err)
	}
	if !s.bundleHas(t, second.runID) {
		t.Error("the second agent's own CA is not in the bundle")
	}

	// The second agent stops while the first is still running: it removes what
	// it installed, and only that.
	s.teardown(t)
	if exists(s.anchor(second.runID)) || s.bundleHas(t, second.runID) {
		t.Errorf("the second agent left its CA behind when it stopped (anchor on disk=%v, in bundle=%v)",
			exists(s.anchor(second.runID)), s.bundleHas(t, second.runID))
	}
	if err := s.handshake(first.port); err != nil {
		t.Errorf("after the second agent stopped, a client that trusts only the system store refuses the first agent: %v", err)
	}

	first.stop()
	if left := s.anchorFiles(t); len(left) != 0 || s.bundleHas(t, first.runID) {
		t.Errorf("the first agent left its CA behind when it stopped (anchors=%v, in bundle=%v)", left, s.bundleHas(t, first.runID))
	}
	s.regenerations(t)
}

// TestNativeInstall_SweepsTheAnchorOfAKilledAgent: keeping live anchors must
// not turn into keeping all of them. An agent that was killed never removed its
// anchor; the next agent to start does, and regenerates the bundle without it.
func TestNativeInstall_SweepsTheAnchorOfAKilledAgent(t *testing.T) {
	s := newFakeTrustStore(t)

	dead := startAgent(t, s)
	dead.kill()
	if !exists(s.anchor(dead.runID)) || !s.bundleHas(t, dead.runID) {
		t.Fatalf("precondition: a killed agent should leave its CA in place (anchor on disk=%v, in bundle=%v)",
			exists(s.anchor(dead.runID)), s.bundleHas(t, dead.runID))
	}

	live := s.mustInstall(t)

	if exists(s.anchor(dead.runID)) {
		t.Error("the anchor of a killed agent was not swept by the next agent to start")
	}
	if exists(filepath.Join(s.state, "ca-runs", dead.runID)) {
		t.Error("the claim of a killed agent was left in /run/keploy")
	}
	if s.bundleHas(t, dead.runID) {
		t.Error("the bundle still carries the CA of a killed agent")
	}
	if !exists(s.anchor(live.runID)) || !s.bundleHas(t, live.runID) {
		t.Error("the agent that swept did not install its own CA")
	}
}

// TestNativeInstall_ClearsTheClaimOfARunThatIsGone: a claim whose agent is
// gone and that has no anchor left to protect — the agent died between its
// claim and its first anchor, or after its teardown removed the anchors — is
// cleared by the next agent to start, so /run/keploy does not fill up.
func TestNativeInstall_ClearsTheClaimOfARunThatIsGone(t *testing.T) {
	s := newFakeTrustStore(t)
	h := startHelper(t, "claim", helperStateEnv+"="+s.state, helperRunEnv+"="+testRunID)
	h.expect("CLAIMED")
	h.kill()

	live := s.mustInstall(t)
	entries, err := os.ReadDir(filepath.Join(s.state, "ca-runs"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != live.runID {
		t.Errorf("the claims left are %v, want only the running agent's", entries)
	}
}

// TestNativeInstall_ManyAgentsAtOnce starts and stops a handful of agents at
// the same instant. Every one must end up trusted — a regeneration that ran
// beside another would have dropped an anchor, or been refused by the tool —
// and nothing may be left once they have all stopped.
func TestNativeInstall_ManyAgentsAtOnce(t *testing.T) {
	s := newFakeTrustStore(t)

	const n = 6
	helpers := make([]*helperProc, n)
	for i := range helpers {
		helpers[i] = startHelper(t, "agent", helperStoreEnv+"="+s.root)
	}
	agents := make([]*agentProc, n)
	for i, h := range helpers {
		agents[i] = awaitAgent(t, h)
	}
	for i, a := range agents {
		if !s.bundleHas(t, a.runID) {
			t.Errorf("agent %d of %d started together is missing from the bundle", i+1, n)
		}
		if err := s.handshake(a.port); err != nil {
			t.Errorf("agent %d of %d started together is not trusted through the system store: %v", i+1, n, err)
		}
	}
	if got := s.regenerations(t); got != n {
		t.Errorf("the bundle was regenerated %d times for %d installs", got, n)
	}

	for _, a := range agents {
		if _, err := io.WriteString(a.stdin, "stop\n"); err != nil {
			t.Fatal(err)
		}
	}
	for _, a := range agents {
		a.expect("STOPPED")
		a.done = true
		if err := a.cmd.Wait(); err != nil {
			t.Errorf("an agent did not exit cleanly: %v", err)
		}
	}
	if left := s.anchorFiles(t); len(left) != 0 {
		t.Errorf("anchors left after every agent stopped: %v", left)
	}
	for i, a := range agents {
		if s.bundleHas(t, a.runID) {
			t.Errorf("agent %d of %d is still in the bundle after it stopped", i+1, n)
		}
	}
	if got := s.regenerations(t); got != 2*n {
		t.Errorf("the bundle was regenerated %d times for %d installs and %d removals", got, n, n)
	}
}

// ---------------------------------------------------------------------------
// one agent at a time changes the store
// ---------------------------------------------------------------------------

// TestNativeInstall_WaitsItsTurnToChangeTheStore: an agent that starts while
// another is changing the store writes nothing and regenerates nothing until
// the other is done.
func TestNativeInstall_WaitsItsTurnToChangeTheStore(t *testing.T) {
	s := newFakeTrustStore(t)

	var ca *caState
	var err error
	finish := queued(t, "the install", func(logger *zap.Logger) {
		ca, err = s.install(logger)
	})

	if left := s.anchorFiles(t); len(left) != 0 {
		t.Errorf("an anchor was written before the install had the trust-store lock: %v", left)
	}
	if got := s.regenerations(t); got != 0 {
		t.Errorf("the bundle was regenerated %d times before the install had the trust-store lock", got)
	}

	finish()
	if err != nil {
		t.Fatalf("the install failed once the store was free: %v", err)
	}
	if !exists(s.anchor(ca.runID)) || !s.bundleHas(t, ca.runID) || s.regenerations(t) != 1 {
		t.Errorf("the install did not complete once the store was free (anchor on disk=%v, in bundle=%v, regenerations=%d)",
			exists(s.anchor(ca.runID)), s.bundleHas(t, ca.runID), s.regenerations(t))
	}
}

// TestTeardownNativeCA_WaitsItsTurnToChangeTheStore is the same rule for an
// agent that stops.
func TestTeardownNativeCA_WaitsItsTurnToChangeTheStore(t *testing.T) {
	s := newFakeTrustStore(t)
	ca := s.mustInstall(t)

	finish := queued(t, "the teardown", func(logger *zap.Logger) {
		TeardownNativeCA(logger)
	})

	if got := s.regenerations(t); got != 1 {
		t.Errorf("the bundle was regenerated before the teardown had the trust-store lock (%d runs, want the install's 1)", got)
	}

	finish()
	if exists(s.anchor(ca.runID)) || s.bundleHas(t, ca.runID) || s.regenerations(t) != 2 {
		t.Errorf("the teardown did not complete once the store was free (anchor on disk=%v, in bundle=%v, regenerations=%d)",
			exists(s.anchor(ca.runID)), s.bundleHas(t, ca.runID), s.regenerations(t))
	}
}

// TestTeardownNativeCA_AfterASecondInterruptTheCAIsLeftForTheNextSweep: a
// stopping agent queues behind other agents for as long as it takes, and a
// second interrupt ends that. It then changes nothing in the store — removing
// its anchor without the lock could pull it from under another agent's
// regeneration — and gives up its claim, so the next agent to start sweeps
// the anchor, under the lock.
//
// The interrupt comes first here, so the teardown must not queue at all; a
// teardown that ignored it would log that it is queueing, and the test would
// say so rather than wait for a lock it holds.
func TestTeardownNativeCA_AfterASecondInterruptTheCAIsLeftForTheNextSweep(t *testing.T) {
	s := newFakeTrustStore(t)
	ca := s.mustInstall(t)
	utils.ResetInterruptedAgain()
	t.Cleanup(utils.ResetInterruptedAgain)

	unlock := mustLockTrustStore(t) // another agent is changing the store
	var once sync.Once
	release := func() { once.Do(unlock) }
	t.Cleanup(release)
	utils.MarkInterruptedAgain() // and the user has said "stop now"

	logger, busy, logs := busyLogger()
	done := make(chan struct{})
	go func() {
		defer close(done)
		TeardownNativeCA(logger)
	}()
	select {
	case <-done:
	case <-busy:
		t.Fatal("the teardown queued for the trust store although keploy had been interrupted again")
	}

	if !exists(s.anchor(ca.runID)) {
		t.Error("the agent removed its anchor without the trust-store lock")
	}
	if got := s.regenerations(t); got != 1 {
		t.Errorf("the bundle was regenerated without the trust-store lock (%d runs, want the install's 1)", got)
	}
	if s.claimed(t, ca.runID) {
		t.Error("the interrupted agent kept its claim, so no other agent may sweep its anchor while it lingers")
	}
	warned := false
	for _, e := range logs.FilterLevelExact(zapcore.WarnLevel).All() {
		if err, ok := e.ContextMap()["error"].(string); ok && strings.Contains(err, errInterruptedAgain.Error()) {
			warned = true
		}
	}
	if !warned {
		t.Error("the agent did not warn that it left its CA in the store because the stop was interrupted")
	}

	// The next agent to start sweeps it.
	release()
	utils.ResetInterruptedAgain()
	next := s.mustInstall(t)
	if exists(s.anchor(ca.runID)) || s.bundleHas(t, ca.runID) {
		t.Errorf("the next agent did not sweep the interrupted agent's CA (anchor on disk=%v, in bundle=%v)",
			exists(s.anchor(ca.runID)), s.bundleHas(t, ca.runID))
	}
	if !s.bundleHas(t, next.runID) {
		t.Error("the next agent's own CA is not installed")
	}
}

// TestTeardownNativeCA_TheFirstSignalOfAnAPIStopEndsTheWait: the CLI stops an
// agent over its API (POST /agent/stop), not with a signal, so no interrupt
// started that stop and there is no "second" one to send. The first SIGINT or
// SIGTERM that reaches the agent while its stop waits for the trust store is
// the one that ends the wait. Here it has already arrived when the teardown
// starts: the teardown must not queue at all (one that did would say so, and
// the test would fail rather than wait for a lock it holds).
func TestTeardownNativeCA_TheFirstSignalOfAnAPIStopEndsTheWait(t *testing.T) {
	s := newFakeTrustStore(t)
	ca := s.mustInstall(t)
	utils.ResetInterruptedAgain()
	t.Cleanup(utils.ResetInterruptedAgain)
	signals := make(chan os.Signal, 1)
	signals <- syscall.SIGTERM
	prev := stopSignals
	stopSignals = func() (<-chan os.Signal, func()) { return signals, func() {} }
	t.Cleanup(func() { stopSignals = prev })

	unlock := mustLockTrustStore(t) // another agent is changing the store
	defer unlock()
	logger, busy, logs := busyLogger()
	done := make(chan struct{})
	go func() {
		defer close(done)
		TeardownNativeCA(logger)
	}()
	select {
	case <-done:
	case <-busy:
		t.Fatal("the teardown queued for the trust store although keploy had been signalled to stop now")
	}
	if !exists(s.anchor(ca.runID)) || s.claimed(t, ca.runID) {
		t.Errorf("the CA was not left, unclaimed, for the next sweep (anchor on disk=%v, claimed=%v)",
			exists(s.anchor(ca.runID)), s.claimed(t, ca.runID))
	}
	warned := false
	for _, e := range logs.FilterLevelExact(zapcore.WarnLevel).All() {
		if err, ok := e.ContextMap()["error"].(string); ok && strings.Contains(err, errSignalledWhileStopping.Error()) {
			warned = true
		}
	}
	if !warned {
		t.Error("the agent did not warn that a signal made it leave its CA in the store")
	}
}

// TestTeardownNativeCA_ASIGTERMDuringTheWaitEndsIt: the same with a real
// SIGTERM sent to this process while the teardown is queued: the teardown is
// listening for it then (were it not, the signal would kill the test binary),
// and it ends the wait.
func TestTeardownNativeCA_ASIGTERMDuringTheWaitEndsIt(t *testing.T) {
	s := newFakeTrustStore(t)
	ca := s.mustInstall(t)
	utils.ResetInterruptedAgain()
	t.Cleanup(utils.ResetInterruptedAgain)

	unlock := mustLockTrustStore(t)
	defer unlock()
	logger, busy, _ := busyLogger()
	done := make(chan struct{})
	go func() {
		defer close(done)
		TeardownNativeCA(logger)
	}()
	select {
	case <-busy:
	case <-done:
		t.Fatal("the teardown did not wait for another agent that was changing the trust store")
	}
	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	<-done
	if !exists(s.anchor(ca.runID)) || s.regenerations(t) != 1 {
		t.Errorf("the teardown went on with the store after the signal (anchor on disk=%v, regenerations=%d)",
			exists(s.anchor(ca.runID)), s.regenerations(t))
	}
}

// TestTeardownNativeCA_ASecondInterruptDoesNotSkipAFreeStore: the second
// interrupt gives up waiting for other agents. With nobody to wait for, there
// is nothing to give up, and the CA is removed from the bundle as usual — by a
// regeneration that the interrupt, already in, must not stop either.
func TestTeardownNativeCA_ASecondInterruptDoesNotSkipAFreeStore(t *testing.T) {
	s := newFakeTrustStore(t)
	ca := s.mustInstall(t)
	utils.ResetInterruptedAgain()
	t.Cleanup(utils.ResetInterruptedAgain)
	utils.MarkInterruptedAgain()

	s.teardown(t)

	if exists(s.anchor(ca.runID)) || s.bundleHas(t, ca.runID) {
		t.Errorf("the CA was not fully removed (anchor on disk=%v, in bundle=%v)", exists(s.anchor(ca.runID)), s.bundleHas(t, ca.runID))
	}
}

// TestSweepCAByFingerprint_RegeneratesInItsTurn: the legacy sweep regenerates
// the bundle too, on the first start after an upgrade, and that regeneration
// queues like every other.
func TestSweepCAByFingerprint_RegeneratesInItsTurn(t *testing.T) {
	s := newFakeTrustStore(t)
	retired, err := generateRunCA()
	if err != nil {
		t.Fatal(err)
	}
	legacy := filepath.Join(s.anchors, "ca.crt")
	if err := os.WriteFile(legacy, retired.certPEM, 0o644); err != nil {
		t.Fatal(err)
	}

	finish := queued(t, "the legacy sweep", func(logger *zap.Logger) {
		sweepCAByFingerprint(context.Background(), logger, sha256.Sum256(retired.der), false, false)
	})

	if got := s.regenerations(t); got != 0 {
		t.Errorf("the bundle was regenerated %d times before the legacy sweep had the trust-store lock", got)
	}
	if !exists(legacy) {
		t.Error("the legacy anchor was removed before the sweep had the trust-store lock, under another agent's regeneration")
	}

	finish()
	if exists(legacy) {
		t.Error("the legacy anchor was not removed")
	}
	if got := s.regenerations(t); got != 1 {
		t.Errorf("the bundle was regenerated %d times after the legacy anchor was removed, want 1", got)
	}
}

// TestRemoveLegacyJavaAlias_RewritesTheKeystoreInItsTurn: keytool rewrites the
// whole cacerts file, which every agent's import rewrites as well.
func TestRemoveLegacyJavaAlias_RewritesTheKeystoreInItsTurn(t *testing.T) {
	s := newFakeTrustStore(t)
	retired, err := generateRunCA()
	if err != nil {
		t.Fatal(err)
	}
	// The JDK "has" the retired CA under the legacy alias.
	if err := os.WriteFile(s.exported, retired.certPEM, 0o644); err != nil {
		t.Fatal(err)
	}

	finish := queued(t, "the legacy alias removal", func(logger *zap.Logger) {
		removeLegacyJavaAlias(context.Background(), logger, sha256.Sum256(retired.der))
	})

	if got := s.countLogLines(t, s.keytoolLog, "-delete"); got != 0 {
		t.Errorf("keytool -delete ran %d times before the removal had the trust-store lock", got)
	}

	finish()
	if got := s.countLogLines(t, s.keytoolLog, "-delete -alias "+legacyCAAlias); got != 1 {
		t.Errorf("keytool -delete ran %d times for the legacy alias, want 1", got)
	}
}

// ---------------------------------------------------------------------------
// what one process holds
// ---------------------------------------------------------------------------

// TestNativeInstall_FailedInstallLeavesNoAnchor: an install that fails holds no
// claim and leaves no file — otherwise a CA nothing will ever sign with would
// sit in the store, marked live, until the process exits.
func TestNativeInstall_FailedInstallLeavesNoAnchor(t *testing.T) {
	s := newFakeTrustStore(t)
	failing := filepath.Join(s.root, "failing-tool")
	if err := os.WriteFile(failing, []byte("#!/bin/sh\necho 'cannot rebuild the bundle' >&2\nexit 3\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	caStoreUpdateCmd = []string{failing}

	_, err := s.install(zap.NewNop())
	if err == nil {
		t.Fatal("the install succeeded although the bundle could not be regenerated")
	}
	if !strings.Contains(err.Error(), "cannot rebuild the bundle") {
		t.Errorf("the error does not say what the tool reported: %v", err)
	}
	if left := s.anchorFiles(t); len(left) != 0 {
		t.Errorf("a failed install left anchors in the store: %v", left)
	}
	if n := takeNativeInstall(); n != nil {
		t.Error("a failed install was recorded as installed")
	}
	if entries, _ := os.ReadDir(filepath.Join(s.state, "ca-runs")); len(entries) != 0 {
		t.Errorf("a failed install kept its claim: %v", entries)
	}
}

// TestNativeInstall_ANewInstallRetiresThePreviousOne: a process that installs a
// second CA stops using the first, so the first's anchor goes. Its claim is
// held by this very process, so no sweep would ever remove it.
func TestNativeInstall_ANewInstallRetiresThePreviousOne(t *testing.T) {
	s := newFakeTrustStore(t)

	first := s.mustInstall(t)
	second := s.mustInstall(t)

	if exists(s.anchor(first.runID)) || s.bundleHas(t, first.runID) {
		t.Errorf("the superseded CA was left in the store (anchor on disk=%v, in bundle=%v)",
			exists(s.anchor(first.runID)), s.bundleHas(t, first.runID))
	}
	if !exists(s.anchor(second.runID)) || !s.bundleHas(t, second.runID) {
		t.Error("the new CA is not installed")
	}
	if s.claimed(t, first.runID) {
		t.Error("the superseded CA's run is still claimed")
	}

	s.teardown(t)
	if left := s.anchorFiles(t); len(left) != 0 {
		t.Errorf("anchors left after teardown: %v", left)
	}
}

// TestNativeInstall_AnAgentKilledMidRegenerationKeepsTheStoreLockedUntilItsToolEnds:
// the kernel releases a killed agent's lock at once, but the tool that agent
// started runs on. The next agent must not regenerate beside it: it waits for
// the tool to finish, then sweeps the dead agent's anchor.
func TestNativeInstall_AnAgentKilledMidRegenerationKeepsTheStoreLockedUntilItsToolEnds(t *testing.T) {
	s := newFakeTrustStore(t)
	s.gateTheTool(t)

	doomed := startHelper(t, "agent", helperStoreEnv+"="+s.root)
	if pid := readFIFOLine(t, s.started); pid == "" {
		t.Fatal("the agent's regeneration did not start")
	}
	doomed.kill()
	if trustStoreLockFree(t) {
		t.Fatal("the trust-store lock was free while the killed agent's regeneration was still running")
	}

	openGate(t, s.gate)
	next := s.mustInstall(t) // queues until the orphaned tool has exited

	if got := s.completedRegenerations(t); got != 2 {
		t.Errorf("%d regenerations ran to the end, want 2 (the killed agent's, then the next agent's)", got)
	}
	if left := s.anchorFiles(t); len(left) != 1 || left[0] != s.anchor(next.runID) {
		t.Errorf("the anchors in the store are %v, want only the next agent's (the killed agent's swept)", left)
	}
	s.regenerations(t) // and no two of them overlapped
}

// TestNativeInstall_ACancelledRunStillFinishesItsChangeToTheStore: once an
// agent holds the trust-store lock, the end of its run does not stop the change
// it is making: the regeneration and the keytool import run to their end. (On
// Fedora, update-ca-trust killed 0.3 s in left OpenSSL's default directory
// with no certificates.) Only the wait for the lock is cancellable, and a free
// lock is taken even by a run that has just been cancelled.
func TestNativeInstall_ACancelledRunStillFinishesItsChangeToTheStore(t *testing.T) {
	s := newFakeTrustStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	ca, err := generateRunCA()
	if err != nil {
		t.Fatal(err)
	}
	if err := setupNativeForApp(ctx, zap.NewNop(), ca, &ephemeralStore{}, 0, s.javaHome); err != nil {
		t.Fatalf("the install was abandoned half-way because the run was cancelled: %v", err)
	}
	if !s.bundleHas(t, ca.runID) || s.completedRegenerations(t) != 1 {
		t.Errorf("the regeneration did not run to its end (in bundle=%v, completed=%d)", s.bundleHas(t, ca.runID), s.completedRegenerations(t))
	}
	if got := s.countLogLines(t, s.keytoolLog, "-import"); got != 1 {
		t.Errorf("keytool -import ran %d times, want 1", got)
	}
}

// TestNativeInstall_TwoStorePathsThatAreOneDirectory: an image whose store
// directories are aliases of each other (a compatibility symlink) gets the same
// anchor written twice. That is one file, and one run's — nothing to refuse.
func TestNativeInstall_TwoStorePathsThatAreOneDirectory(t *testing.T) {
	s := newFakeTrustStore(t)
	alias := filepath.Join(s.root, "alias-anchors")
	if err := os.Symlink(s.anchors, alias); err != nil {
		t.Fatal(err)
	}
	caStorePath = []string{s.anchors + "/", alias + "/"}

	ca := s.mustInstall(t)
	if !s.bundleHas(t, ca.runID) {
		t.Error("the CA is not in the bundle")
	}
	s.teardown(t)
	if left := s.anchorFiles(t); len(left) != 0 || s.bundleHas(t, ca.runID) {
		t.Errorf("the CA was left behind (anchors=%v, in bundle=%v)", left, s.bundleHas(t, ca.runID))
	}
}

// TestNativeInstall_ImportsIntoTheJDKsOwnKeystore: a JDK with a cacerts of its
// own (not a p11-kit output; for that, see
// TestNativeInstall_LeavesAGeneratedJavaKeystoreToTheRegeneration) gets the
// run's CA from keytool, and loses it again at teardown.
func TestNativeInstall_ImportsIntoTheJDKsOwnKeystore(t *testing.T) {
	s := newFakeTrustStore(t)
	s.mustInstall(t)
	s.teardown(t)
	imports, deletes := s.countLogLines(t, s.keytoolLog, "-import"), s.countLogLines(t, s.keytoolLog, "-delete")
	if imports != 1 || deletes != 1 {
		t.Errorf("keytool did not import into and delete from the JDK's own keystore (%d imports, %d deletes)", imports, deletes)
	}
}

// TestNativeInstall_KeepsAnAnchorWhoseRunItCannotTell: an anchor is swept only
// on proof that its run is gone. A claim that cannot be read — here a symlink
// where the claim should be, which no agent makes — is not that proof.
func TestNativeInstall_KeepsAnAnchorWhoseRunItCannotTell(t *testing.T) {
	s := newFakeTrustStore(t)
	anchor := s.anchor(testRunID)
	if err := os.WriteFile(anchor, testCertPEM(t), 0o644); err != nil {
		t.Fatal(err)
	}
	claims := filepath.Join(s.state, "ca-runs")
	if err := os.MkdirAll(claims, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/nonexistent", filepath.Join(claims, testRunID)); err != nil {
		t.Fatal(err)
	}
	if _, err := runIsLive(testRunID); err == nil {
		t.Fatal("precondition: a claim that is a symlink should not be readable")
	}

	s.mustInstall(t)
	if !exists(anchor) {
		t.Error("an anchor was swept although whether its run is alive could not be told")
	}
}

// TestRefreshCaStore_PassesFreshOnlyToUpdateCACertificates: --fresh is Debian's
// flag. The other distros' tools reject an argument they do not know.
func TestRefreshCaStore_PassesFreshOnlyToUpdateCACertificates(t *testing.T) {
	for _, tc := range []struct {
		tool  string
		fresh bool
		want  string
	}{
		{tool: "update-ca-certificates", fresh: true, want: "args: --fresh"},
		{tool: "update-ca-certificates", fresh: false, want: "args: "},
		{tool: "update-ca-trust", fresh: true, want: "args: "},
	} {
		t.Run(fmt.Sprintf("%s fresh=%v", tc.tool, tc.fresh), func(t *testing.T) {
			dir := t.TempDir()
			log := filepath.Join(dir, "log")
			tool := filepath.Join(dir, tc.tool)
			if err := os.WriteFile(tool, []byte(fmt.Sprintf("#!/bin/sh\necho \"args: $*\" > %q\n", log)), 0o755); err != nil {
				t.Fatal(err)
			}
			prev := caStoreUpdateCmd
			caStoreUpdateCmd = []string{filepath.Join(dir, "not-installed"), tool}
			defer func() { caStoreUpdateCmd = prev }()

			if err := refreshCaStore(context.Background(), zap.NewNop(), tc.fresh); err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(log)
			if err != nil {
				t.Fatalf("the tool was not run: %v", err)
			}
			if strings.TrimRight(string(got), "\n") != tc.want {
				t.Errorf("the tool was run with %q, want %q", strings.TrimRight(string(got), "\n"), tc.want)
			}
		})
	}
}
