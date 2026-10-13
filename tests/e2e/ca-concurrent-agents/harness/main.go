//go:build linux

// Package main is the harness for the concurrent-agents CA e2e. It drives the
// real native CA install (SetupCA with isDocker=false, TeardownNativeCA) from
// several processes at once, against the distro's real trust store
// (update-ca-certificates on Debian and Ubuntu, the p11-kit layout of Fedora
// and RHEL), and checks trust the way applications do: with TLS clients that
// know nothing but the system store — curl, which reads the bundle file its
// build names, and openssl s_client with OpenSSL's compiled-in default paths,
// which is what Python's ssl module, Node built against the system OpenSSL,
// Ruby and PHP use. On Fedora 44 those default paths are the hashed directory
// /etc/pki/tls/certs alone.
//
// It has two modes:
//
//	harness agent <name>   one keploy agent's CA lifecycle: install the run CA,
//	                       serve TLS with leaves it signs, remove it on SIGTERM.
//	harness scenario       starts and stops agents and asserts what must hold
//	                       while they overlap.
//
// Nothing here waits on the clock. An agent announces each state on stdout and
// the scenario blocks on that line; an agent that dies instead closes the pipe,
// which is what the scenario sees.
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	tlsmod "go.keploy.io/server/v3/pkg/agent/proxy/tls"
	"go.uber.org/zap"
	"golang.org/x/sys/unix"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: harness agent <name> | harness scenario")
		os.Exit(2)
	}
	switch os.Args[1] {
	case "agent":
		if len(os.Args) != 3 {
			fmt.Fprintln(os.Stderr, "usage: harness agent <name>")
			os.Exit(2)
		}
		os.Exit(runAgent(os.Args[2]))
	case "scenario":
		os.Exit(runScenario())
	default:
		fmt.Fprintf(os.Stderr, "unknown mode %q\n", os.Args[1])
		os.Exit(2)
	}
}

// ---------------------------------------------------------------------------
// agent mode
// ---------------------------------------------------------------------------

// runAgent is one keploy agent as far as the trust store is concerned. It
// reports on stdout, one line per state:
//
//	READY <port> <setup-ms> <base64 CA DER>   the CA is installed and TLS is served
//	FAILED <error>                            SetupCA returned an error
//	STOPPED                                   SIGTERM handled, the CA was removed
func runAgent(name string) int {
	logger, err := newLogger(name)
	if err != nil {
		fmt.Printf("FAILED logger: %v\n", err)
		return 1
	}
	defer func() { _ = logger.Sync() }()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	start := time.Now()
	if err := tlsmod.SetupCA(ctx, logger, false); err != nil {
		fmt.Printf("FAILED %s\n", strings.ReplaceAll(err.Error(), "\n", " "))
		return 1
	}
	setup := time.Since(start)

	block, _ := pem.Decode(tlsmod.ActiveCACertPEM())
	if block == nil {
		fmt.Println("FAILED SetupCA succeeded but published no CA certificate")
		return 1
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Printf("FAILED listen: %v\n", err)
		return 1
	}
	go serve(logger, ln, name)

	fmt.Printf("READY %d %d %s\n", ln.Addr().(*net.TCPAddr).Port, setup.Milliseconds(),
		base64.StdEncoding.EncodeToString(block.Bytes))

	<-ctx.Done()
	_ = ln.Close()
	tlsmod.TeardownNativeCA(logger)
	fmt.Println("STOPPED")
	return 0
}

// newLogger returns the agent's logger. It is quiet by default: each refused
// handshake the scenario provokes on purpose would otherwise print a stack
// trace. HARNESS_VERBOSE=1 turns on the package's debug logging, which names
// every anchor written, kept and swept.
func newLogger(name string) (*zap.Logger, error) {
	cfg := zap.NewDevelopmentConfig()
	cfg.DisableStacktrace = true
	cfg.DisableCaller = true
	if os.Getenv("HARNESS_VERBOSE") != "1" {
		cfg.Level = zap.NewAtomicLevelAt(zap.WarnLevel)
	}
	logger, err := cfg.Build()
	if err != nil {
		return nil, err
	}
	return logger.Named(name), nil
}

// serve terminates TLS exactly as the proxy does — HandleTLSConnection mints a
// leaf for the client's SNI and signs it with this run's CA — and answers any
// request with the agent's name, so a probe can tell which agent it reached.
func serve(logger *zap.Logger, ln net.Listener, name string) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go func() {
			defer func() { _ = conn.Close() }()
			tlsConn, _, err := tlsmod.HandleTLSConnection(context.Background(), logger, conn, time.Time{})
			if err != nil {
				return // the client rejected the leaf; the probe reports that side
			}
			defer func() { _ = tlsConn.Close() }()
			if _, err := http.ReadRequest(bufio.NewReader(tlsConn)); err != nil {
				return
			}
			_, _ = fmt.Fprintf(tlsConn, "HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s", len(name), name)
		}()
	}
}

// ---------------------------------------------------------------------------
// scenario mode
// ---------------------------------------------------------------------------

// The directories the native install writes anchors into, and the bundle files
// the distro's tool regenerates from them. Kept as literals rather than
// imported: the scenario checks the host the way an outside observer would.
var (
	anchorDirs = []string{
		"/usr/local/share/ca-certificates/",
		"/etc/pki/ca-trust/source/anchors/",
		"/etc/ca-certificates/trust-source/anchors/",
		"/etc/pki/trust/anchors/",
		"/usr/local/share/certs/",
		"/etc/ssl/certs/",
	}
	bundleFiles = []string{
		"/etc/ssl/certs/ca-certificates.crt",
		"/etc/pki/tls/certs/ca-bundle.crt",
		"/etc/ssl/ca-bundle.pem",
		"/etc/pki/tls/cacert.pem",
	}
)

// keployCNPrefix starts the Common Name of every run CA; the run id follows it
// and names the anchor file, keploy-mitm-<run id>.crt.
const keployCNPrefix = "Keploy MITM CA "

// simultaneousAgents is how many agents the scenario starts (and later stops)
// at the same instant to put the trust-store tooling under contention.
const simultaneousAgents = 6

type agentProc struct {
	name    string
	cmd     *exec.Cmd
	out     *bufio.Reader
	port    int
	setupMS int64
	caDER   []byte
	runID   string
}

type scenario struct {
	bundle  string
	roots   int               // certificates the bundle held before any agent ran
	layout0 map[string]string // the trust outputs before any agent ran (trustOutputs)
	failed  bool
}

func (s *scenario) pass(format string, args ...any) {
	fmt.Printf("[ASSERT][PASS] "+format+"\n", args...)
}

func (s *scenario) fail(format string, args ...any) {
	s.failed = true
	fmt.Printf("[ASSERT][FAIL] "+format+"\n", args...)
}

func (s *scenario) check(ok bool, format string, args ...any) {
	if ok {
		s.pass(format, args...)
		return
	}
	s.fail(format, args...)
}

func runScenario() int {
	s := &scenario{}
	for _, p := range bundleFiles {
		if fi, err := os.Stat(p); err == nil && fi.Size() > 0 {
			s.bundle = p
			break
		}
	}
	if s.bundle == "" {
		fmt.Println("[ASSERT][FAIL] this image has no system CA bundle; the scenario needs a distro trust store")
		return 1
	}
	fmt.Printf("system bundle: %s\n", s.bundle)

	if left := anchorsOnDisk(); len(left) != 0 {
		fmt.Printf("[ASSERT][FAIL] keploy anchors exist before the scenario starts: %v\n", left)
		return 1
	}
	roots, keploy, err := bundleCounts(s.bundle)
	if err != nil || roots == 0 || keploy != 0 {
		fmt.Printf("[ASSERT][FAIL] the system bundle is not a clean distro bundle before the scenario starts (%d roots, %d keploy CAs, err=%v)\n", roots, keploy, err)
		return 1
	}
	s.roots = roots
	fmt.Printf("system bundle holds %d certificates\n", s.roots)
	if s.layout0, err = trustOutputs(); err != nil {
		fmt.Printf("[ASSERT][FAIL] reading the trust outputs before the scenario starts: %v\n", err)
		return 1
	}
	fmt.Printf("trust outputs: %d entries under %s\n", len(s.layout0), strings.Join(trustOutputDirs, ", "))

	if err := s.run(); err != nil {
		fmt.Printf("[ASSERT][FAIL] scenario aborted: %v\n", err)
		s.failed = true
	}
	if s.failed {
		fmt.Println("[RESULT] FAIL")
		return 1
	}
	fmt.Println("[RESULT] PASS")
	return 0
}

func (s *scenario) run() error {
	// --- two overlapping runs -------------------------------------------------
	fmt.Println("\n=== [1] one agent runs ===")
	a, err := startReady("A")
	if err != nil {
		return err
	}
	defer a.kill()
	s.trusted(a, "A is trusted through the system store while it runs alone")

	// From here until A stops, A is asked continuously whether it is still
	// trusted, while every other agent in the scenario starts and stops
	// around it.
	watch := startTrustWatch(a)
	defer watch.stop()

	fmt.Println("\n=== [2] a second agent starts while the first is still running ===")
	b, err := startReady("B")
	if err != nil {
		return err
	}
	defer b.kill()
	s.trusted(b, "B is trusted through the system store")
	s.trusted(a, "A is STILL trusted through the system store after B started")
	s.rootsIntact("two agents running")

	fmt.Println("\n=== [3] the second agent stops; the first keeps running ===")
	if err := b.stop(); err != nil {
		return err
	}
	s.removed(b, "B removed its own anchor when it stopped")
	s.trusted(a, "A is still trusted after B stopped and refreshed the store")
	s.rootsIntact("one agent stopped")

	// --- a crashed run's anchor is still swept --------------------------------
	fmt.Println("\n=== [4] an agent is killed; the next start sweeps what it left ===")
	c, err := startReady("C")
	if err != nil {
		return err
	}
	c.kill()
	s.check(len(c.anchorFiles()) > 0, "killed agent C left its anchor on disk (nothing ran to remove it)")
	d, err := startReady("D")
	if err != nil {
		return err
	}
	defer d.kill()
	s.removed(c, "the next agent to start swept dead agent C's anchor")
	s.trusted(d, "D is trusted through the system store")
	s.trusted(a, "A is still trusted after D swept C")
	s.rootsIntact("a dead agent's anchor swept")

	// --- the store tooling under contention -----------------------------------
	fmt.Printf("\n=== [5] %d agents start at the same instant ===\n", simultaneousAgents)
	var burst []*agentProc
	for i := 0; i < simultaneousAgents; i++ {
		p, err := startAgent(fmt.Sprintf("E%d", i+1))
		if err != nil {
			return err
		}
		defer p.kill()
		burst = append(burst, p)
	}
	allReady := true
	for _, p := range burst {
		if err := p.awaitReady(); err != nil {
			s.fail("%s did not become ready: %v", p.name, err)
			allReady = false
		}
	}
	if !allReady {
		// Say what state the failed starts left the machine's own bundle in
		// before giving up on the rest of the scenario.
		s.rootsIntact(fmt.Sprintf("%d agents started together", simultaneousAgents))
		return errors.New("an agent failed CA setup while others were starting")
	}
	s.pass("all %d agents completed CA setup", simultaneousAgents)
	for _, p := range burst {
		s.trusted(p, "%s is trusted through the system store", p.name)
	}
	s.trusted(a, "A is still trusted after the burst")
	s.trusted(d, "D is still trusted after the burst")
	s.rootsIntact(fmt.Sprintf("%d agents started together", simultaneousAgents))

	fmt.Printf("\n=== [6] those %d agents stop at the same instant ===\n", simultaneousAgents)
	var wg sync.WaitGroup
	stopErrs := make([]error, len(burst))
	for i, p := range burst {
		wg.Add(1)
		go func() {
			defer wg.Done()
			stopErrs[i] = p.stop()
		}()
	}
	wg.Wait()
	for i, p := range burst {
		if stopErrs[i] != nil {
			return stopErrs[i]
		}
		s.removed(p, "%s removed its own anchor", p.name)
	}
	s.trusted(a, "A is still trusted after the burst stopped")
	s.trusted(d, "D is still trusted after the burst stopped")
	s.rootsIntact(fmt.Sprintf("%d agents stopped together", simultaneousAgents))

	// --- everything stops: nothing is left behind -----------------------------
	fmt.Println("\n=== [7] the last agents stop ===")
	if err := d.stop(); err != nil {
		return err
	}
	s.removed(d, "D removed its own anchor")
	watch.stop()
	s.watched(watch)
	if err := a.stop(); err != nil {
		return err
	}
	s.removed(a, "A removed its own anchor")
	left := anchorsOnDisk()
	s.check(len(left) == 0, "no keploy anchor is left in the trust store (found %v)", left)
	s.rootsIntact("every agent stopped")

	// --- the distro's own tool still owns the store ---------------------------
	fmt.Println("\n=== [8] the store is in the shape the distro's own tool leaves it ===")
	after, err := trustOutputs()
	if err != nil {
		return err
	}
	s.sameLayout(s.layout0, withoutDebianOrphans(s.layout0, after), "keploy left the trust outputs exactly as it found them")
	tool := distroTool()
	if tool == "" {
		return errors.New("this image has neither update-ca-certificates nor update-ca-trust")
	}
	out, err := exec.Command(tool).CombinedOutput()
	s.check(err == nil, "the distro's %s runs cleanly on the store keploy left (err=%v %s)", tool, err, bytes.TrimSpace(out))
	rerun, err := trustOutputs()
	if err != nil {
		return err
	}
	s.sameLayout(after, rerun, "a run of the distro's "+tool+" afterwards changes nothing keploy left")
	return nil
}

// trusted asserts that an application relying on the system store alone would
// accept the leaves this agent signs. Four independent views must agree: the
// agent's CA is in the bundle file the distro's tool generates; curl with its
// built-in defaults completes a handshake; curl restricted to that bundle file
// does too (an OpenSSL client would otherwise be satisfied by a hash link in
// the certs directory even when the bundle had lost the CA); and openssl
// s_client with OpenSSL's default paths does (which on Fedora is the hashed
// certificate directory and nothing else).
func (s *scenario) trusted(a *agentProc, format string, args ...any) {
	what := fmt.Sprintf(format, args...)
	inBundle, err := bundleHas(s.bundle, a.caDER)
	if err != nil {
		s.fail("%s: reading %s: %v", what, s.bundle, err)
		return
	}
	def := probe(a, "")
	only := probe(a, s.bundle)
	ossl := opensslProbe(a)
	if inBundle && def.ok && only.ok && ossl.ok {
		s.pass("%s", what)
		return
	}
	s.fail("%s — in bundle file: %v; curl, built-in defaults: %s; curl, bundle file only: %s; openssl, default paths: %s",
		what, inBundle, def, only, ossl)
}

// removed asserts that nothing of this agent's CA is left: no anchor file and
// no entry in the regenerated bundle.
func (s *scenario) removed(a *agentProc, format string, args ...any) {
	what := fmt.Sprintf(format, args...)
	files := a.anchorFiles()
	inBundle, err := bundleHas(s.bundle, a.caDER)
	if err != nil {
		s.fail("%s: reading %s: %v", what, s.bundle, err)
		return
	}
	if len(files) == 0 && !inBundle {
		s.pass("%s", what)
		return
	}
	s.fail("%s — anchor files left: %v; still in bundle file: %v", what, files, inBundle)
}

// watched reports what a trust watch saw. Every probe must have succeeded: an
// agent that is running stays trusted while the others change the store. And
// some probes must have run while the store was being changed, or the watch
// proved nothing.
func (s *scenario) watched(w *trustWatch) {
	for _, c := range watchClients {
		st := w.stats[c.name]
		fmt.Printf("watch of %s, %s: %d probes, %d failed, %d while a store change was under way\n",
			w.target.name, c.name, st.probes, st.failed, st.duringChange)
		s.check(st.failed == 0, "%s never refused %s while other agents started and stopped (%d of %d probes failed; first failure: %s)",
			c.name, w.target.name, st.failed, st.probes, st.firstFailure)
		s.check(st.duringChange > 0, "%s probed %s while other agents were changing the store (%d probes did)",
			c.name, w.target.name, st.duringChange)
	}
}

// withoutDebianOrphans returns got without the links update-ca-certificates
// leaves behind for a removed keploy anchor, and says how many there were.
//
// Debian's and Ubuntu's tool links each local CA into /etc/ssl/certs (a
// <name>.pem link and a hash link to it) and, when the CA's file is removed,
// removes those links only on a later run that adds or removes a certificate
// it counts — and it does not count a removed local one. So after the last
// agent's teardown its links dangle until something next adds a CA. That is
// the distro tool's behaviour, unchanged by this PR (main leaves the same);
// what the scenario asserts about it is that it is the only difference: a
// dangling link, new since the start, whose target names a keploy anchor.
func withoutDebianOrphans(before, got map[string]string) map[string]string {
	out := make(map[string]string, len(got))
	orphans := 0
	for p, desc := range got {
		if _, existed := before[p]; !existed && strings.HasPrefix(desc, "link -> ") {
			target := strings.TrimPrefix(desc, "link -> ")
			if !filepath.IsAbs(target) {
				target = filepath.Join(filepath.Dir(p), target)
			}
			if _, err := os.Stat(p); err != nil && strings.HasPrefix(filepath.Base(target), "keploy-mitm-") {
				orphans++
				continue
			}
		}
		out[p] = desc
	}
	if orphans > 0 {
		fmt.Printf("[NOTE] %d dangling links to removed keploy anchors left by the distro's tool (see withoutDebianOrphans)\n", orphans)
	}
	return out
}

// sameLayout asserts that two snapshots of the trust outputs (trustOutputs)
// are identical.
func (s *scenario) sameLayout(want, got map[string]string, what string) {
	var diffs []string
	for p, w := range want {
		if g, ok := got[p]; !ok {
			diffs = append(diffs, fmt.Sprintf("missing %s (%s)", p, w))
		} else if g != w {
			diffs = append(diffs, fmt.Sprintf("changed %s: %s, was %s", p, g, w))
		}
	}
	for p, g := range got {
		if _, ok := want[p]; !ok {
			diffs = append(diffs, fmt.Sprintf("new %s (%s)", p, g))
		}
	}
	sort.Strings(diffs)
	if len(diffs) > 20 {
		diffs = append(diffs[:20], fmt.Sprintf("... and %d more", len(diffs)-20))
	}
	s.check(len(diffs) == 0, "%s (%d entries; differences: %v)", what, len(got), diffs)
}

// rootsIntact asserts that the bundle still holds every certificate the distro
// shipped. keploy only ever adds and removes its own CA, but it does so by
// running the distro's tool, and a tool that is run twice at once can write a
// bundle that is missing the machine's own roots — which breaks TLS for every
// process on it, not only for keploy's.
func (s *scenario) rootsIntact(when string) {
	roots, _, err := bundleCounts(s.bundle)
	if err != nil {
		s.fail("the system bundle could not be read (%s): %v", when, err)
		return
	}
	s.check(roots == s.roots, "the system bundle still holds the distro's %d certificates (%s; found %d)", s.roots, when, roots)
}

// startAgent spawns one agent process and returns without waiting for it.
func startAgent(name string) (*agentProc, error) {
	self, err := os.Executable()
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(self, "agent", name)
	cmd.Stderr = os.Stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting agent %s: %w", name, err)
	}
	return &agentProc{name: name, cmd: cmd, out: bufio.NewReader(stdout)}, nil
}

func startReady(name string) (*agentProc, error) {
	a, err := startAgent(name)
	if err != nil {
		return nil, err
	}
	if err := a.awaitReady(); err != nil {
		a.kill()
		return nil, fmt.Errorf("agent %s did not become ready: %w", name, err)
	}
	return a, nil
}

// awaitReady blocks until the agent reports READY or FAILED, or exits.
func (a *agentProc) awaitReady() error {
	line, err := a.out.ReadString('\n')
	if err != nil {
		return fmt.Errorf("exited before reporting a state (%v)", err)
	}
	fields := strings.Fields(line)
	if len(fields) == 0 || fields[0] != "READY" {
		return errors.New(strings.TrimSpace(line))
	}
	if len(fields) != 4 {
		return fmt.Errorf("malformed READY line %q", line)
	}
	if a.port, err = strconv.Atoi(fields[1]); err != nil {
		return fmt.Errorf("malformed READY line %q", line)
	}
	if a.setupMS, err = strconv.ParseInt(fields[2], 10, 64); err != nil {
		return fmt.Errorf("malformed READY line %q", line)
	}
	if a.caDER, err = base64.StdEncoding.DecodeString(fields[3]); err != nil {
		return fmt.Errorf("malformed READY line %q", line)
	}
	cert, err := x509.ParseCertificate(a.caDER)
	if err != nil {
		return fmt.Errorf("agent reported an unparseable CA: %w", err)
	}
	a.runID = strings.TrimPrefix(cert.Subject.CommonName, keployCNPrefix)
	fmt.Printf("agent %s ready: run %s, CA setup took %dms\n", a.name, a.runID, a.setupMS)
	return nil
}

// stop asks the agent to shut down cleanly and waits for it to finish removing
// its CA.
func (a *agentProc) stop() error {
	if err := a.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		return fmt.Errorf("signalling agent %s: %w", a.name, err)
	}
	rest, _ := io.ReadAll(a.out)
	err := a.cmd.Wait()
	a.cmd = nil
	if err != nil {
		return fmt.Errorf("agent %s did not stop cleanly: %w", a.name, err)
	}
	if !strings.Contains(string(rest), "STOPPED") {
		return fmt.Errorf("agent %s exited without reporting STOPPED", a.name)
	}
	return nil
}

// kill ends the agent with SIGKILL — the crash path, where nothing in the agent
// runs to remove its CA. It is a no-op for an agent that already stopped.
func (a *agentProc) kill() {
	if a.cmd == nil {
		return
	}
	_ = a.cmd.Process.Kill()
	_, _ = io.Copy(io.Discard, a.out)
	_ = a.cmd.Wait()
	a.cmd = nil
}

// anchorFiles lists this agent's anchor files still present in the store.
func (a *agentProc) anchorFiles() []string {
	var found []string
	for _, dir := range anchorDirs {
		p := filepath.Join(dir, "keploy-mitm-"+a.runID+".crt")
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() {
			found = append(found, p)
		}
	}
	return found
}

// anchorsOnDisk lists every keploy run anchor present in the store, whoever
// owns it.
func anchorsOnDisk() []string {
	var found []string
	for _, dir := range anchorDirs {
		matches, _ := filepath.Glob(filepath.Join(dir, "keploy-mitm-*.crt"))
		for _, m := range matches {
			if fi, err := os.Stat(m); err == nil && fi.Mode().IsRegular() {
				found = append(found, m)
			}
		}
	}
	return found
}

// bundleHas reports whether the bundle file contains the certificate der.
func bundleHas(bundle string, der []byte) (bool, error) {
	data, err := os.ReadFile(bundle)
	if err != nil {
		return false, err
	}
	for {
		var block *pem.Block
		block, data = pem.Decode(data)
		if block == nil {
			return false, nil
		}
		if bytes.Equal(block.Bytes, der) {
			return true, nil
		}
	}
}

// bundleCounts returns how many certificates in the bundle are the distro's
// roots and how many are keploy run CAs.
func bundleCounts(bundle string) (roots, keploy int, err error) {
	data, err := os.ReadFile(bundle)
	if err != nil {
		return 0, 0, err
	}
	for {
		var block *pem.Block
		block, data = pem.Decode(data)
		if block == nil {
			return roots, keploy, nil
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err == nil && strings.HasPrefix(cert.Subject.CommonName, keployCNPrefix) {
			keploy++
			continue
		}
		roots++
	}
}

type probeResult struct {
	ok     bool
	detail string
}

func (r probeResult) String() string {
	if r.ok {
		return "handshake ok"
	}
	return r.detail
}

// opensslProbe makes one TLS handshake with the agent using openssl s_client
// and reports whether it verified. s_client is given no CA option at all, so it
// uses OpenSSL's compiled-in defaults: the CA file and the hashed certificate
// directory under OPENSSLDIR. The environment is emptied so SSL_CERT_FILE and
// SSL_CERT_DIR cannot stand in for them.
func opensslProbe(a *agentProc) probeResult {
	cmd := exec.Command("openssl", "s_client", "-connect", fmt.Sprintf("127.0.0.1:%d", a.port),
		"-servername", "localhost", "-verify_hostname", "localhost", "-verify_return_error", "-brief")
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=/nonexistent"}
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	if err == nil && strings.Contains(out.String(), "Verification: OK") {
		return probeResult{ok: true}
	}
	reason := strings.TrimSpace(out.String())
	for _, line := range strings.Split(reason, "\n") {
		if strings.Contains(line, "verify error") || strings.Contains(line, "rror") {
			reason = strings.TrimSpace(line)
			break
		}
	}
	return probeResult{detail: fmt.Sprintf("%v: %s", err, reason)}
}

// watchClients are the clients a trust watch probes with.
var watchClients = []struct {
	name  string
	probe func(*agentProc) probeResult
}{
	{"openssl s_client with OpenSSL's default paths", opensslProbe},
	{"curl with its built-in defaults", func(a *agentProc) probeResult { return probe(a, "") }},
}

type watchStats struct {
	probes, failed, duringChange int
	firstFailure                 string
}

// trustWatch asks, with every client, whether one running agent is trusted, one
// probe after another until it is stopped. Each probe that found the
// trust-store lock held both when it started and when it ended is counted as
// run while a store change was under way.
type trustWatch struct {
	target *agentProc
	done   chan struct{}
	once   sync.Once
	wg     sync.WaitGroup
	mu     sync.Mutex
	stats  map[string]*watchStats
}

func startTrustWatch(target *agentProc) *trustWatch {
	w := &trustWatch{target: target, done: make(chan struct{}), stats: map[string]*watchStats{}}
	for _, c := range watchClients {
		st := &watchStats{firstFailure: "none"}
		w.stats[c.name] = st
		w.wg.Add(1)
		go func() {
			defer w.wg.Done()
			for {
				select {
				case <-w.done:
					return
				default:
				}
				before := storeLockHeld()
				r := c.probe(target)
				during := before && storeLockHeld()
				w.mu.Lock()
				st.probes++
				if during {
					st.duringChange++
				}
				if !r.ok {
					if st.failed == 0 {
						st.firstFailure = r.String()
					}
					st.failed++
				}
				w.mu.Unlock()
			}
		}()
	}
	return w
}

// stop ends the watch and returns once no probe is running.
func (w *trustWatch) stop() {
	w.once.Do(func() { close(w.done) })
	w.wg.Wait()
}

// storeLockFile is the lock every keploy agent holds while it changes the
// system trust store.
const storeLockFile = "/run/keploy/ca-trust.lock"

// storeLockHeld reports whether some process holds the trust-store lock right
// now. It reads /proc/locks rather than trying the lock, so the agents never
// see the watch.
func storeLockHeld() bool {
	var st unix.Stat_t
	if err := unix.Stat(storeLockFile, &st); err != nil {
		return false
	}
	id := fmt.Sprintf("%02x:%02x:%d", unix.Major(uint64(st.Dev)), unix.Minor(uint64(st.Dev)), st.Ino)
	data, err := os.ReadFile("/proc/locks")
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(data), "\n") {
		// "1: FLOCK  ADVISORY  WRITE 1907 00:19:13026 0 EOF"; a waiter's
		// line has "->" after the number.
		f := strings.Fields(line)
		if len(f) >= 6 && f[1] == "FLOCK" && f[5] == id {
			return true
		}
	}
	return false
}

// trustOutputDirs are where the distros' tools write what applications read:
// Debian's and Ubuntu's /etc/ssl/certs, and the p11-kit outputs of Fedora and
// RHEL (where /etc/ssl/certs holds compatibility links or is one).
var trustOutputDirs = []string{"/etc/ssl/certs", "/etc/pki/tls/certs", "/etc/pki/ca-trust/extracted"}

// trustOutputs describes every entry under trustOutputDirs: a directory by its
// mode, a symlink by its target, a file by its mode and digest. A Java keystore
// is described by its entry count, because p11-kit stamps each entry with the
// time it was written.
func trustOutputs() (map[string]string, error) {
	out := map[string]string{}
	for _, root := range trustOutputDirs {
		err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				if errors.Is(err, fs.ErrNotExist) && p == root {
					return fs.SkipDir
				}
				return err
			}
			info, err := d.Info()
			if err != nil {
				return err
			}
			switch {
			case d.Type()&fs.ModeSymlink != 0:
				target, err := os.Readlink(p)
				if err != nil {
					return err
				}
				out[p] = "link -> " + target
			case d.IsDir():
				out[p] = fmt.Sprintf("dir %o", info.Mode().Perm())
			default:
				data, err := os.ReadFile(p)
				if err != nil {
					return err
				}
				if len(data) >= 12 && binary.BigEndian.Uint32(data) == 0xFEEDFEED {
					out[p] = fmt.Sprintf("file %o java keystore, %d entries", info.Mode().Perm(), binary.BigEndian.Uint32(data[8:]))
					return nil
				}
				sum := sha256.Sum256(data)
				out[p] = fmt.Sprintf("file %o sha256:%s", info.Mode().Perm(), hex.EncodeToString(sum[:8]))
			}
			return nil
		})
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
	}
	return out, nil
}

// distroTool is the tool this image's distro rebuilds its trust store with.
func distroTool() string {
	for _, t := range []string{"update-ca-certificates", "update-ca-trust"} {
		if p, err := exec.LookPath(t); err == nil {
			return p
		}
	}
	return ""
}

// probe makes one HTTPS request to the agent with curl and reports whether the
// handshake verified. With bundleOnly empty curl uses the distro's built-in
// defaults; otherwise it trusts that one file and no certificate directory.
// The environment is emptied so no CA variable can stand in for the system
// store.
func probe(a *agentProc, bundleOnly string) probeResult {
	args := []string{"-sS", "--noproxy", "*"}
	if bundleOnly != "" {
		args = append(args, "--cacert", bundleOnly, "--capath", "/nonexistent")
	}
	args = append(args, fmt.Sprintf("https://localhost:%d/", a.port))
	cmd := exec.Command("curl", args...)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=/nonexistent"}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if err == nil {
		if got := stdout.String(); got != a.name {
			return probeResult{detail: fmt.Sprintf("handshake ok but reached %q, not %q", got, a.name)}
		}
		return probeResult{ok: true}
	}
	// curl follows its one-line diagnosis with a paragraph of advice.
	reason, _, _ := strings.Cut(strings.TrimSpace(stderr.String()), "\n")
	return probeResult{detail: fmt.Sprintf("%v: %s", err, reason)}
}
