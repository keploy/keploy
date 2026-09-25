// Package token mints and carries the per-session token that guards the
// agent's control-plane HTTP API.
//
// It deliberately imports nothing from the rest of keploy. The token is needed
// by the agent that serves the API (pkg/agent/routes), by the CLI-side client
// that calls it (pkg/platform/http) and by the code that launches the agent in
// a container (pkg/platform/docker), and those packages already depend on each
// other. A leaf package is what lets all three share one value.
package token

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Env carries the per-session control-plane token from the keploy CLI to the
// agent it starts.
//
// The token is never passed as a flag value: argv is world-readable through
// /proc/<pid>/cmdline and `ps`, which would hand it to exactly the local users
// it exists to keep out. In a container the environment is the vehicle, and
// the docker or compose client is handed the variable BY NAME (`-e NAME`,
// `environment: [NAME]`) with the value in its own environment rather than
// `-e NAME=value` in argv, for that same reason. Natively the environment
// cannot be used at all — the CLI starts the agent through sudo, which resets
// it — so the token goes in a 0600 file and only its path travels in argv.
// See WriteFile.
const Env = "KEPLOY_AGENT_TOKEN"

// ProbePath is the path the keploy CLI uses to check that an agent is really
// enforcing the token it was handed, appended to the agent's base URL.
//
// It is deliberately a route the agent does not serve. routes.Authenticate is
// router-level middleware, so it runs before chi matches anything: a guarded
// agent answers 401 here and an unguarded one falls through to chi's 404.
// Neither reaches a handler, so the check cannot touch the session.
//
// Shared from this leaf package because both sides need to agree on it — the
// client to send it, and the agent to recognise it as its own health check
// rather than report it as an intruder.
const ProbePath = "/__keploy_auth_probe"

// MockAgentTokenEnv carries the same token the other way: out to the test
// runner the user wraps with `keploy mock record|replay`, which drives the
// per-test scope API at {KEPLOY_MOCK_AGENT}/agent/scope/*.
//
// That caller is not keploy. It is the user's own test process, and it is an
// advertised integration point, so guarding the control plane without handing
// it a credential would simply break it. It is exported only in mock mode,
// where the wrapped process IS the intended API client — in record and test
// mode the application under test is handed nothing.
//
// A separate variable from Env, not the same one: Env is what an agent reads
// to learn the token it must enforce, and a process that found Env in its
// environment would adopt it as its own rather than present it.
const MockAgentTokenEnv = "KEPLOY_MOCK_AGENT_TOKEN"

// New returns a fresh 256-bit control-plane token.
func New() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generating agent control-plane token: %w", err)
	}
	return hex.EncodeToString(b), nil
}

var (
	sessionMu sync.Mutex
	session   string
	resolved  bool
)

// Session is the token this process presents to an agent and passes down to
// any agent it starts. One value per process, resolved on first use: an
// inherited token if the process was started with one, a freshly minted token
// otherwise.
//
// Resolved lazily rather than in an init call placed somewhere in startup: the
// client that sends the token and the code that passes it to the agent live in
// different packages and run in an order that varies by mode, and a token that
// depended on that ordering would surface as an auth failure at runtime in
// whichever mode happened to read it first.
//
// A generation failure is not fatal. The agent then starts without a token and
// behaves exactly as it did before authentication existed, saying so loudly at
// startup, rather than leaving the user unable to record at all.
func Session() string {
	sessionMu.Lock()
	defer sessionMu.Unlock()

	if !resolved {
		session = resolveSession(os.Getenv)
		resolved = true
	}
	return session
}

// Adopt fixes the session token to the one this process was given rather than
// one it minted. The agent calls it once it knows which token it is enforcing —
// the value from its token file, or the empty string when it was started
// without one — so that a later Session() in the agent returns that, instead of
// a freshly minted 256-bit string that would look entirely valid and match
// nothing a client sends.
func Adopt(tok string) {
	sessionMu.Lock()
	defer sessionMu.Unlock()

	if tok != "" {
		session = tok
	}
	// Marked resolved even for an empty token, which is the case this exists
	// for: an agent that was started without one must report that it has none,
	// not mint a replacement on the next call. An empty token never overwrites
	// one already adopted.
	resolved = true
}

// resolveSession holds the logic Session memoises, with the environment
// injected so it can be exercised without touching the process-wide memo.
func resolveSession(getenv func(string) string) string {
	if inherited := getenv(Env); inherited != "" {
		return inherited
	}
	tok, err := New()
	if err != nil {
		return ""
	}
	return tok
}

// FromEnv returns the token the process that started this agent passed down,
// or "" when there wasn't one.
//
// The agent must not fall back to minting its own: a token no client knows
// would reject every request rather than protect anything. An agent started
// without a token serves its control plane unauthenticated, and says so.
func FromEnv() string { return os.Getenv(Env) }

// fileMu guards the session's token file and the private directory it lives
// in.
var (
	fileMu sync.Mutex
	// dir is this process's private directory for the token file, created on
	// first use.
	dir string
)

// fileName is the token file's name inside the private directory. It can be
// fixed because the directory is what nobody else can write into; see
// WriteFile.
const fileName = "agent-token"

// WriteFile stores the session token in a private file and returns its path.
//
// The file, not the environment, is what carries the token to a natively
// spawned agent. On Linux the CLI usually starts the agent as `sudo keploy
// agent`, and sudo's default env_reset drops unknown variables, so an
// environment-only handoff arrives empty on the single most common path — the
// agent would fall back to serving unauthenticated while the client dutifully
// sent a token nothing checked. The file survives sudo because only its path
// travels, in argv, and a path is not a secret.
//
// It is also why the file never sits directly in the temp directory. The path
// is in the agent's argv for every local user to read, and the agent unlinks
// the file once it has read it. In /tmp, which is sticky but world-writable,
// anyone could then create a file at that same name, and the next agent this
// process started would be handed it: garbage, to fail its token check, or a
// token of their own choosing, to enforce against the real client. So the file
// lives in a directory created for this process by os.MkdirTemp — 0700, owned
// by this user, under an unpredictable name — in which nobody else can create
// anything. Inside it, re-creating the file at the same path once the agent
// has consumed it is safe, and that is what happens. If the directory itself
// is gone, or whatever now has its name is not private to this user (a temp
// cleaner removed it and someone else took the name), a new one is created
// under a new name instead.
//
// Cleanup removes the directory, and everything left in it, when the process
// is done.
func WriteFile() (string, error) {
	fileMu.Lock()
	defer fileMu.Unlock()

	tok := Session()
	if tok == "" {
		return "", fmt.Errorf("no agent control-plane token to write")
	}
	if !ownDir() {
		d, err := os.MkdirTemp("", "keploy-agent-*")
		if err != nil {
			return "", fmt.Errorf("creating a private directory for the agent control-plane token: %w", err)
		}
		dir = d
	}

	path := filepath.Join(dir, fileName)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, fs.ErrExist) {
		// Not consumed yet — the agent it was written for never read it. It
		// holds this process's token, and nothing else can have put it here.
		return path, nil
	}
	if err != nil {
		return "", fmt.Errorf("creating agent control-plane token file: %w", err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.WriteString(Env + "=" + tok + "\n"); err != nil {
		_ = os.Remove(path)
		return "", fmt.Errorf("writing agent control-plane token file: %w", err)
	}
	return path, nil
}

// ownDir reports whether dir is still a directory only this user can enter.
// Anyone else who took its name after it was removed cannot have made it so.
// Must be called with fileMu held.
func ownDir() bool {
	if dir == "" {
		return false
	}
	fi, err := os.Lstat(dir)
	return err == nil && fi.IsDir() && isPrivate(fi)
}

// ReadFile returns the token from a file written by WriteFile.
func ReadFile(path string) (string, error) {
	b, err := os.ReadFile(path) //nolint:gosec // the path comes from this process's own launcher, via argv
	if err != nil {
		return "", fmt.Errorf("reading agent control-plane token file: %w", err)
	}
	line := strings.TrimSpace(string(b))
	tok := strings.TrimPrefix(line, Env+"=")
	if tok == line {
		return "", fmt.Errorf("agent control-plane token file %q is not in %s=<token> form", path, Env)
	}
	if tok == "" {
		return "", fmt.Errorf("agent control-plane token file %q holds an empty token", path)
	}
	return tok, nil
}

// Cleanup removes this process's private token directory and anything still
// in it.
//
// A natively spawned agent unlinks its token file once it has read it, but one
// that never got that far leaves it behind, and the directory outlives both.
// Every keploy binary whose commands are built by cli.Root gets this for free:
// Root registers it to run when the command finishes, however it finishes. A
// program that starts agents through pkg/platform/http without cli.Root must
// call Cleanup itself on its way out; nothing else removes the directory.
//
// A directory at that name that is no longer private to this user is left
// alone: it is not the one this process made, and removing it is not this
// process's call to make.
func Cleanup() error {
	fileMu.Lock()
	defer fileMu.Unlock()

	if dir == "" {
		return nil
	}
	d, ours := dir, ownDir()
	dir = ""
	if !ours {
		return nil
	}
	if err := os.RemoveAll(d); err != nil {
		return fmt.Errorf("removing the agent control-plane token directory: %w", err)
	}
	return nil
}

// launchMu guards the record of the agents this process launched.
var (
	launchMu  sync.Mutex
	launches  = map[string]uint64{}
	launchSeq uint64
)

// RecordLaunch notes that this process is starting an agent it will reach at
// agentURI, and handing it the session token: natively through --token-file,
// or through a docker or compose client given the token by name.
//
// It is what the client's check that its agent enforces the token keys on
// (pkg.verifyControlPlaneGuarded). Recorded for the launch, not for a handoff
// that went through: a token file that could not be written, or a launcher
// that stopped passing one, leaves an agent this process started serving open,
// and that is exactly what the check exists to catch. What it must not check
// is an agent this process never launched — a Kubernetes sidecar that
// k8s-proxy drives, an agent image that predates the token. That agent was
// never handed anything, and checking it for the token this process minted can
// only raise a false alarm about a handoff that never happened.
func RecordLaunch(agentURI string) {
	launchMu.Lock()
	defer launchMu.Unlock()

	launchSeq++
	launches[agentURI] = launchSeq
}

// Launched reports whether this process launched the agent at agentURI, and
// which launch that was.
//
// The number is new for every agent started there: a docker compose test run
// starts a new agent for each test-set, at the same address, from the compose
// file generated once for the session. A check that remembers per launch
// rather than per address therefore still sees each of those agents as the
// new agent it is.
func Launched(agentURI string) (uint64, bool) {
	launchMu.Lock()
	defer launchMu.Unlock()

	seq, ok := launches[agentURI]
	return seq, ok
}
