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

func TestWriteFileReadFile_RoundTripsThroughAPrivateFile(t *testing.T) {
	freshTokenDir(t)
	path, err := WriteFile()
	require.NoError(t, err)

	got, err := ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, Session(), got, "the file must carry this process's own session token")

	raw, err := os.ReadFile(path) //nolint:gosec // test-owned temp path
	require.NoError(t, err)
	require.Equal(t, Env+"="+Session()+"\n", string(raw))
}

func TestWriteFile_IsNotReadableByOtherUsers(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix permission bits are not the access control that applies on windows")
	}
	freshTokenDir(t)
	path, err := WriteFile()
	require.NoError(t, err)

	info, err := os.Stat(path)
	require.NoError(t, err)
	// The whole point of routing the token through a file instead of argv is
	// to keep it away from other local users. A group- or world-readable file
	// would hand it back to them.
	require.Equal(t, os.FileMode(0600), info.Mode().Perm(),
		"token file must be 0600; anything wider defeats the reason it is a file at all")
}

// TestWriteFile_KeepsTheFileWhereOtherUsersCannotPlantOne is the regression
// test for a real hole. The path is in the agent's argv for every local user to
// read, and the agent unlinks the file once it has read it. When the file sat
// directly in /tmp — sticky, but writable by everyone — anyone could create a
// file at that name the moment it was gone, and the next agent this process
// started was handed it: garbage, which left it serving unauthenticated, or a
// token of the planter's choosing, which it then enforced.
func TestWriteFile_KeepsTheFileWhereOtherUsersCannotPlantOne(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix permission bits are not the access control that applies on windows")
	}
	shared := sharedTempDir(t)
	freshTokenDir(t)

	path, err := WriteFile()
	require.NoError(t, err)

	parent := filepath.Dir(path)
	require.NotEqual(t, shared, parent,
		"the token file is directly in the world-writable temp directory, where anyone can plant one at its name")
	require.Equal(t, shared, filepath.Dir(parent), "the private directory belongs in the temp directory")
	info, err := os.Stat(parent)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o700), info.Mode().Perm(),
		"other users can create entries next to the token file, so they can plant one once the agent unlinks it")
}

func TestWriteFile_ReusesTheFileWhileItExists(t *testing.T) {
	freshTokenDir(t)
	first, err := WriteFile()
	require.NoError(t, err)

	second, err := WriteFile()
	require.NoError(t, err)
	require.Equal(t, first, second, "one live file per process; a second copy would leave the secret in another place")
}

func TestWriteFile_RecreatesTheFileAfterTheAgentConsumedIt(t *testing.T) {
	// A natively spawned agent unlinks the token file once it has read it, and
	// one CLI process can start more than one agent — a readiness retry starts
	// a second. Handing that one a path to a deleted file would fail it closed
	// for nothing.
	freshTokenDir(t)
	first, err := WriteFile()
	require.NoError(t, err)
	require.NoError(t, os.Remove(first))

	second, err := WriteFile()
	require.NoError(t, err)

	// Same place, because nobody else can create anything in that directory.
	require.Equal(t, first, second)
	got, err := ReadFile(second)
	require.NoError(t, err)
	require.Equal(t, Session(), got, "the replacement must carry the same session token the client is already sending")
}

func TestWriteFile_DoesNotWriteIntoADirectoryThatIsNoLongerItsOwn(t *testing.T) {
	// A temp cleaner can remove the private directory under a long run, and
	// then its name is free for anyone to take. Writing the token into
	// whatever stands there now would hand it to them.
	if runtime.GOOS == "windows" {
		t.Skip("unix permission bits are not the access control that applies on windows")
	}
	freshTokenDir(t)
	first, err := WriteFile()
	require.NoError(t, err)
	oldDir := filepath.Dir(first)

	require.NoError(t, os.RemoveAll(oldDir))
	require.NoError(t, os.Mkdir(oldDir, 0o755)) // someone else's directory, same name
	t.Cleanup(func() { _ = os.RemoveAll(oldDir) })

	second, err := WriteFile()
	require.NoError(t, err)
	require.NotEqual(t, oldDir, filepath.Dir(second), "the token was written into a directory this process did not create")
	entries, err := os.ReadDir(oldDir)
	require.NoError(t, err)
	require.Empty(t, entries)

	got, err := ReadFile(second)
	require.NoError(t, err)
	require.Equal(t, Session(), got)
}

func TestWriteFile_DoesNotWriteIntoADirectoryAnotherUserOwns(t *testing.T) {
	// The same takeover by another user, who can make the directory 0700 just
	// as easily — but cannot make it this user's. Needs root to hand the
	// directory to someone else.
	if runtime.GOOS == "windows" || os.Geteuid() != 0 {
		t.Skip("needs root on unix to give a directory to another user")
	}
	freshTokenDir(t)
	first, err := WriteFile()
	require.NoError(t, err)
	oldDir := filepath.Dir(first)
	require.NoError(t, os.RemoveAll(oldDir))
	require.NoError(t, os.Mkdir(oldDir, 0o700))
	require.NoError(t, os.Chown(oldDir, 65534, 65534)) // nobody
	t.Cleanup(func() { _ = os.RemoveAll(oldDir) })

	second, err := WriteFile()
	require.NoError(t, err)
	require.NotEqual(t, oldDir, filepath.Dir(second), "the token was written into another user's directory")
}

func TestCleanup_RemovesTheDirectoryAndWhateverIsLeftInIt(t *testing.T) {
	// An agent that failed before it read its file leaves the token on disk.
	freshTokenDir(t)
	path, err := WriteFile()
	require.NoError(t, err)

	require.NoError(t, Cleanup())
	_, err = os.Stat(filepath.Dir(path))
	require.True(t, os.IsNotExist(err), "the session token directory outlived the run")

	// And a later agent in the same process still gets a token.
	again, err := WriteFile()
	require.NoError(t, err)
	got, err := ReadFile(again)
	require.NoError(t, err)
	require.Equal(t, Session(), got)
}

func TestCleanup_LeavesADirectoryThatIsNoLongerItsOwnAlone(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix permission bits are not the access control that applies on windows")
	}
	freshTokenDir(t)
	path, err := WriteFile()
	require.NoError(t, err)
	oldDir := filepath.Dir(path)

	require.NoError(t, os.RemoveAll(oldDir))
	require.NoError(t, os.Mkdir(oldDir, 0o755))
	theirs := filepath.Join(oldDir, "theirs")
	require.NoError(t, os.WriteFile(theirs, nil, 0o644))
	t.Cleanup(func() { _ = os.RemoveAll(oldDir) })

	require.NoError(t, Cleanup())
	_, err = os.Stat(theirs)
	require.NoError(t, err, "Cleanup removed a directory this process did not create")
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
	// for the token file, and a torn read of the directory would hand one of
	// them an empty path and an agent that refuses to start.
	freshTokenDir(t)
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

func TestLaunched_IsRecordedPerAgentAndRenewedForEachOne(t *testing.T) {
	const uri = "http://localhost:16789/agent"
	_, ok := Launched("http://localhost:1/never-launched")
	require.False(t, ok, "an agent this process never started was reported as launched by it")

	RecordLaunch(uri)
	first, ok := Launched(uri)
	require.True(t, ok)

	// A later agent at the same address — the next test-set's, under compose
	// — is a different agent, and must look like one.
	RecordLaunch(uri)
	second, ok := Launched(uri)
	require.True(t, ok)
	require.NotEqual(t, first, second)
}

// freshTokenDir starts a test with no private token directory and removes the
// one it created afterwards, so every test sees WriteFile's first-use path and
// none of them leave a token behind.
func freshTokenDir(t *testing.T) {
	t.Helper()
	require.NoError(t, Cleanup())
	t.Cleanup(func() { require.NoError(t, Cleanup()) })
}

// sharedTempDir points the process's temp directory at a sticky,
// world-writable directory, the way /tmp is.
func sharedTempDir(t *testing.T) string {
	t.Helper()
	shared := filepath.Join(t.TempDir(), "tmp")
	require.NoError(t, os.Mkdir(shared, 0o777))
	require.NoError(t, os.Chmod(shared, os.ModeSticky|0o777))
	t.Setenv("TMPDIR", shared)
	return shared
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
