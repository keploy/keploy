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
	"fmt"
	"os"
	"strings"
	"sync"
)

// Env carries the per-session control-plane token from the keploy CLI to the
// agent it starts.
//
// The token is never passed as a flag value: argv is world-readable through
// /proc/<pid>/cmdline and `ps`, which would hand it to exactly the local users
// it exists to keep out. In a container the environment is the vehicle, filled
// from a private env file rather than from `-e NAME=value` for that same
// reason. Natively the environment cannot be used at all — the CLI starts the
// agent through sudo, which resets it — so the token goes in a 0600 file and
// only its path travels in argv. See WriteFile.
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

// fileMu guards the session's token file.
var (
	fileMu   sync.Mutex
	filePath string
)

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
// Written in docker's env-file format so the same file can be handed to
// `docker run --env-file` and to a compose `env_file:` without a second
// representation of the same secret.
//
// Once docker has read it the token is in the container's Config.Env and shows
// up in `docker inspect`. That is not a boundary this could hold anyway —
// membership of the docker group is equivalent to root — and what the file
// avoids is the token appearing in argv and in a 0644 compose file, which are
// readable without it.
//
// os.CreateTemp creates the file 0600 and picks an unpredictable name with
// O_EXCL, so no other user can read it or race it into place.
//
// The path is reused while the file is still there, and a NEW temp file is
// minted when it is not. A natively spawned agent unlinks the file once it has
// read it, so a process that starts a second agent — `keploy rerecord` runs a
// record and then a test — would otherwise hand it a path to a deleted file
// and watch it come up unauthenticated. Re-creating at the remembered path
// instead of a fresh one would reintroduce the symlink race that CreateTemp's
// O_EXCL exists to avoid.
func WriteFile() (string, error) {
	fileMu.Lock()
	defer fileMu.Unlock()

	if filePath != "" {
		if _, err := os.Stat(filePath); err == nil {
			return filePath, nil
		}
	}

	tok := Session()
	if tok == "" {
		return "", fmt.Errorf("no agent control-plane token to write")
	}
	f, err := os.CreateTemp("", "keploy-agent-token-*")
	if err != nil {
		return "", fmt.Errorf("creating agent control-plane token file: %w", err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.WriteString(Env + "=" + tok + "\n"); err != nil {
		_ = os.Remove(f.Name())
		return "", fmt.Errorf("writing agent control-plane token file: %w", err)
	}
	filePath = f.Name()
	return filePath, nil
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

// RemoveFile deletes the session's token file if one is still on disk.
//
// A natively spawned agent unlinks the file itself once it has read it, but
// the container paths cannot: `docker run --env-file` and a compose
// `env_file:` are read by docker, not by keploy, and keploy does not get to
// see when. So the CLI clears it on the way out, rather than leaving a session
// token in the temp directory after the run — one more file per run, forever.
func RemoveFile() error {
	fileMu.Lock()
	defer fileMu.Unlock()

	if filePath == "" {
		return nil
	}
	err := os.Remove(filePath)
	filePath = ""
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removing agent control-plane token file: %w", err)
	}
	return nil
}
