package routes

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/agent/token"
	"go.uber.org/zap"
)

// ConsumeSessionToken is the single point where the agent decides whether it
// has a credential to enforce. If it returns "" the agent serves its whole
// control plane unauthenticated while the CLI keeps sending a header nothing
// checks — and nothing else in the process would notice.

func TestConsumeSessionToken_ReadsTheFileTheLauncherPassed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tok")
	require.NoError(t, os.WriteFile(path, []byte(token.Env+"=a-real-token\n"), 0600))

	got := ConsumeSessionToken(zap.NewNop(), path)
	require.Equal(t, "a-real-token", got)
}

func TestConsumeSessionToken_UnlinksTheFileOnceItHasBeenRead(t *testing.T) {
	// The token's whole reason for being in a file rather than the environment
	// is that argv and env are readable; leaving it on disk for the rest of the
	// run gives that back.
	path := filepath.Join(t.TempDir(), "tok")
	require.NoError(t, os.WriteFile(path, []byte(token.Env+"=a-real-token\n"), 0600))

	ConsumeSessionToken(zap.NewNop(), path)

	_, err := os.Stat(path)
	require.True(t, os.IsNotExist(err), "the session token is still on disk after the agent read it")
}

func TestConsumeSessionToken_FallsBackToTheEnvironmentForContainerModes(t *testing.T) {
	// In docker there is no sudo in between, so the token arrives as an env
	// var from --env-file / compose env_file.
	t.Setenv(token.Env, "token-from-the-container-env")

	require.Equal(t, "token-from-the-container-env", ConsumeSessionToken(zap.NewNop(), ""))
}

func TestConsumeSessionToken_PrefersTheFileOverTheEnvironment(t *testing.T) {
	t.Setenv(token.Env, "stale-env-token")
	path := filepath.Join(t.TempDir(), "tok")
	require.NoError(t, os.WriteFile(path, []byte(token.Env+"=fresh-file-token\n"), 0600))

	require.Equal(t, "fresh-file-token", ConsumeSessionToken(zap.NewNop(), path))
}

func TestConsumeSessionToken_FallsBackToTheEnvironmentWhenTheFileIsUnusable(t *testing.T) {
	// A bad path must not cost the agent a token it also has in the
	// environment — that would serve the control plane open.
	t.Setenv(token.Env, "token-from-the-container-env")

	require.Equal(t, "token-from-the-container-env",
		ConsumeSessionToken(zap.NewNop(), filepath.Join(t.TempDir(), "absent")))
}

func TestConsumeSessionToken_ReportsNoTokenWhenThereIsNone(t *testing.T) {
	t.Setenv(token.Env, "")

	require.Empty(t, ConsumeSessionToken(zap.NewNop(), ""),
		"a token invented here would be enforced against clients that cannot know it")
}

func TestConsumeSessionToken_MakesTheProcessAgreeWithWhatItEnforces(t *testing.T) {
	// Without the Adopt, a later token.Session() in the agent mints an
	// unrelated value that looks entirely valid and matches nothing.
	path := filepath.Join(t.TempDir(), "tok")
	require.NoError(t, os.WriteFile(path, []byte(token.Env+"=the-enforced-token\n"), 0600))

	got := ConsumeSessionToken(zap.NewNop(), path)
	require.Equal(t, got, token.Session())
}
