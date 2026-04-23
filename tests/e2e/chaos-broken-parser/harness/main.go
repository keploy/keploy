// Command chaos-broken-parser-harness drives the e2e chaos test that
// proves invariants I1–I3 from PLAN.md: a parser panicking mid-stream
// must not break the application's database connection; the app's
// queries must continue to succeed via keploy's passthrough fallback
// (globalPassThrough).
//
// Shape:
//
//  1. Bring up the compose stack (postgres + psql-client sidecar).
//  2. (When V2 is wired — see broken_parser.go) start an in-process
//     keploy proxy with the broken Postgres parser registered at the
//     top of p.integrationsPriority. That parser panics on the first
//     chunk read; the supervisor recovers, flips
//     FallthroughToPassthrough, and recordViaSupervisor hands the real
//     sockets to globalPassThrough.
//  3. Fire 100 `SELECT 1` queries with 50ms spacing at the proxy port.
//     Count successes and failures.
//  4. Tail the keploy log collector for the canonical supervisor-
//     fallback message and for any escaped "panic" string.
//  5. Exit 0 iff: successes >= 99, >=1 supervisor-fallback log, 0
//     escaped panics.
//
// Current status: scaffolding. The V2 supervisor/relay/fakeconn
// packages referenced by broken_parser.go do not exist on this branch
// yet; see tests/e2e/chaos-broken-parser/README.md for the follow-up
// that wires them up. The harness itself is fully compilable so CI can
// catch bit-rot while the infrastructure lands.
package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// logSink is an io.Writer that both forwards to stderr and appends to
// an in-memory ring the harness scans for the supervisor-fallback
// marker and for any escaped "panic" token. The embedded mutex is the
// only serialization: this is driven by a single goroutine today but
// the API is race-safe for the "multiple proxy goroutines" case the
// follow-up (see README.md) will introduce.
type logSink struct {
	mu        sync.Mutex
	buf       bytes.Buffer
	forwardTo io.Writer
}

func newLogSink(forwardTo io.Writer) *logSink {
	return &logSink{forwardTo: forwardTo}
}

func (l *logSink) Write(p []byte) (int, error) {
	l.mu.Lock()
	l.buf.Write(p)
	l.mu.Unlock()
	if l.forwardTo != nil {
		_, _ = l.forwardTo.Write(p)
	}
	return len(p), nil
}

// snapshot returns the current log contents as a string. Safe to call
// concurrently with Write.
func (l *logSink) snapshot() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	// Copy so callers can strings.Contains without racing Write.
	return l.buf.String()
}

const (
	// fallbackMarker is the log substring emitted by the parser
	// supervisor when it recovers a panic and tells the dispatcher to
	// fall through to globalPassThrough. Must match the zap message in
	// pkg/agent/proxy/supervisor/ once that package lands — see
	// README.md for the authoritative reference.
	fallbackMarker = "parser supervisor triggered passthrough fallback"

	// panicMarker is any escaped panic that the supervisor failed to
	// catch. Its presence means I1 (supervisor as panic firewall) is
	// violated.
	panicMarker = "panic:"
)

// Config bundles the harness' tunables. Defaults target a local run;
// CI overrides via flags or env.
type Config struct {
	ComposeFile    string
	ComposeProject string
	PGHostPort     int
	QueryCount     int
	QuerySpacing   time.Duration
	BringUpTimeout time.Duration
	SuccessMinimum int
}

func defaultConfig() *Config {
	return &Config{
		ComposeFile:    findComposeFile(),
		ComposeProject: "chaos-broken-parser",
		PGHostPort:     envIntOr("PG_HOST_PORT", 55432),
		QueryCount:     100,
		QuerySpacing:   50 * time.Millisecond,
		BringUpTimeout: 90 * time.Second,
		SuccessMinimum: 99,
	}
}

func findComposeFile() string {
	// Prefer the canonical path relative to the harness source so `go
	// run ./tests/e2e/chaos-broken-parser/harness` from the repo root
	// Just Works; fall back to $PWD for containerized runs that chdir
	// into the test directory.
	candidates := []string{
		"tests/e2e/chaos-broken-parser/docker-compose.yml",
		"docker-compose.yml",
		"../docker-compose.yml",
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			abs, err := filepath.Abs(c)
			if err == nil {
				return abs
			}
			return c
		}
	}
	return "docker-compose.yml"
}

func envIntOr(key string, fallback int) int {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	var n int
	_, err := fmt.Sscanf(v, "%d", &n)
	if err != nil || n <= 0 {
		return fallback
	}
	return n
}

func main() {
	cfg := defaultConfig()
	flag.StringVar(&cfg.ComposeFile, "compose-file", cfg.ComposeFile, "path to docker-compose.yml")
	flag.StringVar(&cfg.ComposeProject, "project", cfg.ComposeProject, "compose project name")
	flag.IntVar(&cfg.PGHostPort, "pg-host-port", cfg.PGHostPort, "host port forwarded to postgres:5432")
	flag.IntVar(&cfg.QueryCount, "queries", cfg.QueryCount, "number of SELECT queries to run")
	flag.DurationVar(&cfg.QuerySpacing, "query-spacing", cfg.QuerySpacing, "sleep between consecutive queries")
	flag.DurationVar(&cfg.BringUpTimeout, "up-timeout", cfg.BringUpTimeout, "deadline for postgres to become healthy")
	flag.IntVar(&cfg.SuccessMinimum, "min-successes", cfg.SuccessMinimum, "minimum successful queries required to pass")
	dryRun := flag.Bool("dry-run", false, "skip docker; just verify the harness compiles+parses flags")
	flag.Parse()

	if *dryRun {
		log.Printf("dry-run: config=%+v", cfg)
		log.Printf("dry-run: harness compiled and parsed flags; exiting 0")
		return
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	sink := newLogSink(os.Stderr)

	if err := run(ctx, cfg, sink); err != nil {
		log.Printf("FAIL: %v", err)
		os.Exit(1)
	}
	log.Printf("PASS: parser panic did not break DB connection; passthrough fallback observed")
}

// run is the testable top-level. It returns nil on pass and a
// descriptive error on fail — the exit-code translation is the
// caller's job.
func run(ctx context.Context, cfg *Config, sink *logSink) error {
	if err := ensureDockerAvailable(ctx); err != nil {
		return fmt.Errorf("docker prerequisite: %w", err)
	}

	log.Printf("bringing up compose stack (project=%s)", cfg.ComposeProject)
	if err := composeUp(ctx, cfg); err != nil {
		return fmt.Errorf("compose up: %w", err)
	}
	// Always try to tear the stack down — `docker compose down -v`
	// removes the volumes so back-to-back runs don't pick up stale
	// state from a previous init.sql execution.
	defer func() {
		// Use a detached context so Ctrl-C during cleanup still lets
		// us write the results.
		downCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := composeDown(downCtx, cfg); err != nil {
			log.Printf("warning: compose down: %v", err)
		}
	}()

	if err := waitForPostgres(ctx, cfg); err != nil {
		return fmt.Errorf("postgres readiness: %w", err)
	}

	// -----------------------------------------------------------------
	// V2 WIRING — see README.md ("Follow-up needed").
	//
	// The code below is the stub the follow-up replaces:
	//
	//   1. Build + start a keploy proxy in-process with the chaos
	//      Postgres parser (broken_parser.go, gated behind
	//      //go:build chaos_broken_parser) at the top of
	//      p.integrationsPriority.
	//   2. Point queries at the proxy listener rather than directly at
	//      the postgres sidecar.
	//
	// Until then, running the harness without the build tag exercises
	// only the docker-compose orchestration + query driver + log
	// assertion scaffolding. The final pass/fail criteria in
	// evaluate() already check the supervisor-fallback marker; the
	// follow-up just needs to ensure the supervisor actually emits it.
	// -----------------------------------------------------------------
	if err := startBrokenParserProxyIfEnabled(ctx, cfg, sink); err != nil {
		return fmt.Errorf("broken-parser proxy: %w", err)
	}

	successes, failures := driveQueries(ctx, cfg)

	return evaluate(cfg, sink, successes, failures)
}

// evaluate encodes the pass/fail predicate described in the package
// doc. Factored out so chaos_test.go can exercise it with synthetic
// counters if needed.
func evaluate(cfg *Config, sink *logSink, successes, failures int) error {
	logDump := sink.snapshot()
	sawFallback := strings.Contains(logDump, fallbackMarker)
	sawPanic := strings.Contains(logDump, panicMarker)

	log.Printf("query results: successes=%d failures=%d (min required=%d, of %d)",
		successes, failures, cfg.SuccessMinimum, cfg.QueryCount)
	log.Printf("log scan: fallback-marker=%v panic-escaped=%v", sawFallback, sawPanic)

	var failures2 []string
	if successes < cfg.SuccessMinimum {
		failures2 = append(failures2,
			fmt.Sprintf("invariant I3 violated: only %d/%d queries succeeded (need >= %d)",
				successes, cfg.QueryCount, cfg.SuccessMinimum))
	}
	if !sawFallback {
		failures2 = append(failures2,
			fmt.Sprintf("invariant I2 violated: no %q log observed — supervisor never fell through to passthrough", fallbackMarker))
	}
	if sawPanic {
		failures2 = append(failures2,
			"invariant I1 violated: escaped panic detected in keploy log output")
	}
	if len(failures2) > 0 {
		return fmt.Errorf("chaos assertions failed:\n  - %s", strings.Join(failures2, "\n  - "))
	}
	return nil
}

// driveQueries runs cfg.QueryCount SELECTs through the psql-client
// sidecar and returns (successCount, failureCount). The connection
// target is the postgres service — once the V2 proxy lands the
// follow-up swaps `-h postgres` for `-h <proxy-host> -p <proxy-port>`
// so the queries flow through keploy's fakeconn pipeline.
func driveQueries(ctx context.Context, cfg *Config) (successes, failures int) {
	var okCount, failCount atomic.Int64
	for i := 0; i < cfg.QueryCount; i++ {
		if ctx.Err() != nil {
			break
		}
		// SELECT <i>; so pg-side logs can distinguish the iterations
		// when debugging a flake. Use -Atc for terse, single-row,
		// no-header output.
		stmt := fmt.Sprintf("SELECT %d;", i+1)
		// PGPASSWORD must be exported *into the exec'd container*, not
		// into the host-side `docker` process — hence `-e PGPASSWORD=…`
		// after `exec -T` rather than `cmd.Env = …`.
		cmd := exec.CommandContext(ctx, "docker", "compose",
			"-f", cfg.ComposeFile,
			"-p", cfg.ComposeProject,
			"exec", "-T",
			"-e", "PGPASSWORD=chaos",
			"psql-client",
			"psql",
			"-h", "postgres",
			"-U", "chaos",
			"-d", "chaos",
			"-v", "ON_ERROR_STOP=1",
			"-Atc", stmt,
		)
		cmd.Env = os.Environ()
		if err := cmd.Run(); err != nil {
			failCount.Add(1)
			// Keep going — partial failure is expected during the
			// panic-recovery window and the success-floor assertion
			// tolerates up to (QueryCount - SuccessMinimum) losses.
		} else {
			okCount.Add(1)
		}
		if cfg.QuerySpacing > 0 {
			select {
			case <-ctx.Done():
				return int(okCount.Load()), int(failCount.Load())
			case <-time.After(cfg.QuerySpacing):
			}
		}
	}
	return int(okCount.Load()), int(failCount.Load())
}

// composeUp shells out to `docker compose up -d` (no --build because
// both images are pulled, not built in-tree).
func composeUp(ctx context.Context, cfg *Config) error {
	cmd := exec.CommandContext(ctx, "docker", "compose",
		"-f", cfg.ComposeFile,
		"-p", cfg.ComposeProject,
		"up", "-d", "--wait",
	)
	// Inherit the host env so PG_HOST_PORT flows through to the
	// compose file's ${PG_HOST_PORT:-55432} interpolation.
	cmd.Env = os.Environ()
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func composeDown(ctx context.Context, cfg *Config) error {
	cmd := exec.CommandContext(ctx, "docker", "compose",
		"-f", cfg.ComposeFile,
		"-p", cfg.ComposeProject,
		"down", "-v", "--remove-orphans",
	)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// waitForPostgres blocks until `docker compose ps --status=running`
// reports the postgres service as healthy, or cfg.BringUpTimeout
// elapses. `up -d --wait` already does this implicitly, so in practice
// this is a defence-in-depth check against compose versions that
// silently ignore --wait.
func waitForPostgres(ctx context.Context, cfg *Config) error {
	deadline := time.Now().Add(cfg.BringUpTimeout)
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if time.Now().After(deadline) {
			return errors.New("timed out waiting for postgres to be healthy")
		}
		out, err := exec.CommandContext(ctx, "docker", "compose",
			"-f", cfg.ComposeFile,
			"-p", cfg.ComposeProject,
			"ps", "--status=running", "postgres",
		).CombinedOutput()
		if err == nil && bytes.Contains(out, []byte("postgres")) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(1 * time.Second):
		}
	}
}

// ensureDockerAvailable returns an error if the docker CLI or the
// compose subcommand is missing. The test wrapper in chaos_test.go
// uses a similar probe to decide whether to SKIP rather than FAIL on
// environments without Docker.
func ensureDockerAvailable(ctx context.Context) error {
	if _, err := exec.LookPath("docker"); err != nil {
		return fmt.Errorf("docker CLI not in PATH: %w", err)
	}
	out, err := exec.CommandContext(ctx, "docker", "compose", "version").CombinedOutput()
	if err != nil {
		return fmt.Errorf("docker compose not available: %w (output: %s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}
