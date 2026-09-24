package token

import (
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNew_IsAFreshHexEncoded256BitSecret(t *testing.T) {
	first, err := New()
	require.NoError(t, err)

	raw, err := hex.DecodeString(first)
	require.NoError(t, err, "token must be hex so it survives an env var and a docker env file unescaped")
	require.Len(t, raw, 32, "a guessable token is no token; keep it at 256 bits")

	second, err := New()
	require.NoError(t, err)
	require.NotEqual(t, first, second, "every call must mint a new token, or one leak compromises every later session")
}

func TestResolveSession_PrefersAnInheritedToken(t *testing.T) {
	// The agent side of a spawn must adopt the launcher's token rather than
	// mint its own: a token no client knows rejects every request.
	got := resolveSession(func(k string) string {
		require.Equal(t, Env, k)
		return "inherited-token"
	})
	require.Equal(t, "inherited-token", got)
}

func TestResolveSession_MintsWhenNothingWasInherited(t *testing.T) {
	got := resolveSession(func(string) string { return "" })
	raw, err := hex.DecodeString(got)
	require.NoError(t, err)
	require.Len(t, raw, 32)
}

func TestWriteFileReadFile_RoundTripsThroughAPrivateEnvFile(t *testing.T) {
	path, err := WriteFile()
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.Remove(path) })

	got, err := ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, Session(), got, "the file must carry this process's own session token")

	// Docker env-file format, so `docker run --env-file` and a compose
	// `env_file:` can consume the very same file.
	raw, err := os.ReadFile(path) //nolint:gosec // test-owned temp path
	require.NoError(t, err)
	require.Equal(t, Env+"="+Session()+"\n", string(raw))
}

func TestWriteFile_IsNotReadableByOtherUsers(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix permission bits are not the access control that applies on windows")
	}
	path, err := WriteFile()
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.Remove(path) })

	info, err := os.Stat(path)
	require.NoError(t, err)
	// The whole point of routing the token through a file instead of argv is
	// to keep it away from other local users. A group- or world-readable file
	// would hand it back to them.
	require.Equal(t, os.FileMode(0600), info.Mode().Perm(),
		"token file must be 0600; anything wider defeats the reason it is a file at all")
}

func TestWriteFile_ReusesTheFileWhileItExists(t *testing.T) {
	first, err := WriteFile()
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.Remove(first) })

	second, err := WriteFile()
	require.NoError(t, err)
	require.Equal(t, first, second, "one live file per process; a second copy would leave the secret in another place")
}

func TestWriteFile_MintsAFreshFileAfterTheAgentConsumedTheLastOne(t *testing.T) {
	// A natively spawned agent unlinks the token file once it has read it, and
	// one CLI process can start more than one agent — `keploy rerecord` runs a
	// record and then a test. Handing the second agent a path to a deleted
	// file would bring it up unauthenticated without saying anything.
	first, err := WriteFile()
	require.NoError(t, err)
	require.NoError(t, os.Remove(first))

	second, err := WriteFile()
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.Remove(second) })

	require.NotEqual(t, first, second, "re-creating at the remembered path would reopen the symlink race CreateTemp avoids")

	got, err := ReadFile(second)
	require.NoError(t, err)
	require.Equal(t, Session(), got, "the replacement must carry the same session token the client is already sending")
}

func TestReadFile_RejectsContentItCannotTrust(t *testing.T) {
	dir := t.TempDir()
	tests := []struct {
		name    string
		content string
	}{
		{"not env-file form", "just-the-raw-token\n"},
		{"wrong variable", "SOMETHING_ELSE=abc\n"},
		{"empty token", Env + "=\n"},
		{"empty file", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(dir, strings.ReplaceAll(tt.name, " ", "-"))
			require.NoError(t, os.WriteFile(path, []byte(tt.content), 0600))

			_, err := ReadFile(path)
			// Returning "" with a nil error would start the agent
			// unauthenticated while the client kept sending a token, so the
			// mismatch has to surface as an error.
			require.Error(t, err)
		})
	}
}

func TestReadFile_ReportsAMissingFileRatherThanAnEmptyToken(t *testing.T) {
	_, err := ReadFile(filepath.Join(t.TempDir(), "absent"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "reading agent control-plane token file")
}

func TestReadFile_ToleratesTrailingWhitespace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "env")
	require.NoError(t, os.WriteFile(path, []byte(Env+"=abc123\r\n"), 0600))

	got, err := ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, "abc123", got)
}

func TestWriteFile_IsSafeUnderConcurrentUse(t *testing.T) {
	// fileMu exists because the record and replay setup paths can both reach
	// for the token file, and a torn read of filePath would hand one of them
	// an empty path and a silently unauthenticated agent.
	const goroutines = 16
	paths := make(chan string, goroutines)
	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p, err := WriteFile()
			if err != nil {
				paths <- "ERR:" + err.Error()
				return
			}
			paths <- p
		}()
	}
	wg.Wait()
	close(paths)

	first := ""
	for p := range paths {
		require.NotEmpty(t, p)
		require.False(t, strings.HasPrefix(p, "ERR:"), p)
		if first == "" {
			first = p
		}
		require.Equal(t, first, p, "concurrent callers disagreed on the token file")
	}
	t.Cleanup(func() { _ = RemoveFile() })

	got, err := ReadFile(first)
	require.NoError(t, err)
	require.Equal(t, Session(), got)
}

func TestAdopt_StopsTheAgentFromMintingAPhantomToken(t *testing.T) {
	// In the agent process the environment is empty (sudo reset it) and the
	// real token arrived in a file. Without Adopt, a later Session() would mint
	// a fresh 256-bit string that looks entirely valid and matches nothing the
	// client sends.
	restoreSession(t)

	Adopt("the-token-the-agent-is-enforcing")
	require.Equal(t, "the-token-the-agent-is-enforcing", Session())

	// An empty adopt must not clobber a token already in hand.
	Adopt("")
	require.Equal(t, "the-token-the-agent-is-enforcing", Session())
}

func TestAdopt_EmptyTokenMeansNoTokenRatherThanMintOne(t *testing.T) {
	// The agent started without a token serves unauthenticated and says so.
	// If Adopt("") left the memo unresolved, the next Session() in that process
	// would mint a token nothing knows — which reads as a working credential
	// and matches no request.
	restoreSession(t)
	resetSession(t)

	Adopt("")
	require.Empty(t, Session(), "the agent minted a token it was never given")
}

// TestSession_DoesNotRaceAdopt covers the window where one goroutine resolves
// the memo while another adopts into it. A reader that slipped between the two
// would see an empty token and quietly authenticate nothing.
func TestSession_DoesNotRaceAdopt(t *testing.T) {
	restoreSession(t)

	for i := 0; i < 200; i++ {
		resetSession(t)

		var wg sync.WaitGroup
		got := make(chan string, 2)
		wg.Add(2)
		go func() { defer wg.Done(); Adopt("adopted") }()
		go func() { defer wg.Done(); got <- Session() }()
		wg.Wait()
		close(got)

		for v := range got {
			require.NotEmpty(t, v, "Session() returned an empty token while Adopt was running")
		}

		// The falsifiable half, and the reason this is not just a -race
		// canary: once Adopt has returned, the adopted token is what every
		// later caller sees. A Session() that re-resolved, or that read a
		// value Adopt had not finished publishing, would mint or return
		// something else here.
		require.Equal(t, "adopted", Session(),
			"Adopt returned but the process still disagrees about which token it enforces")
	}
}

// restoreSession puts the process-wide token back after a test that changes it,
// so the rest of the package (and any order it runs in) is unaffected.
func restoreSession(t *testing.T) {
	t.Helper()
	sessionMu.Lock()
	prev, prevResolved := session, resolved
	sessionMu.Unlock()
	t.Cleanup(func() {
		sessionMu.Lock()
		session, resolved = prev, prevResolved
		sessionMu.Unlock()
	})
}

// resetSession returns the memo to its unresolved state.
func resetSession(t *testing.T) {
	t.Helper()
	sessionMu.Lock()
	session, resolved = "", false
	sessionMu.Unlock()
}
