package routes

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/agent/token"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// consumeSessionToken is the single point where the agent decides whether it
// has a credential to enforce. With no token it refuses (ErrNoToken), whatever
// the opt-out says; only Guard reads the opt-out, the one case in which the
// agent serves its control plane to any caller.

func TestConsumeSessionToken_ReadsTheFileTheLauncherPassed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tok")
	require.NoError(t, os.WriteFile(path, []byte(token.Env+"=a-real-token\n"), 0600))

	got, err := consumeSessionToken(zap.NewNop(), path, false)
	require.NoError(t, err)
	require.Equal(t, "a-real-token", got)
}

func TestConsumeSessionToken_UnlinksTheFileOnceItHasBeenRead(t *testing.T) {
	// The token's whole reason for being in a file rather than the environment
	// is that argv and env are readable; leaving it on disk for the rest of the
	// run gives that back.
	path := filepath.Join(t.TempDir(), "tok")
	require.NoError(t, os.WriteFile(path, []byte(token.Env+"=a-real-token\n"), 0600))

	_, err := consumeSessionToken(zap.NewNop(), path, false)
	require.NoError(t, err)

	_, err = os.Stat(path)
	require.True(t, os.IsNotExist(err), "the session token is still on disk after the agent read it")
}

func TestConsumeSessionToken_ReadsTheEnvironmentForContainerModes(t *testing.T) {
	// In docker there is no sudo in between, so the token arrives as an env
	// var the docker or compose client passed through by name.
	t.Setenv(token.Env, "token-from-the-container-env")

	got, err := consumeSessionToken(zap.NewNop(), "", true)
	require.NoError(t, err)
	require.Equal(t, "token-from-the-container-env", got)
}

func TestConsumeSessionToken_PrefersTheFileOverTheEnvironment(t *testing.T) {
	t.Setenv(token.Env, "stale-env-token")
	path := filepath.Join(t.TempDir(), "tok")
	require.NoError(t, os.WriteFile(path, []byte(token.Env+"=fresh-file-token\n"), 0600))

	got, err := consumeSessionToken(zap.NewNop(), path, false)
	require.NoError(t, err)
	require.Equal(t, "fresh-file-token", got)
}

// TestConsumeSessionToken_RefusesATokenFileItCannotUse pins failing closed.
// Only a launcher that hands its agent a token passes --token-file, so a file
// that yields none means the handoff broke or something else is at that path.
// Falling back served the control plane open on garbage — and a planted
// `KEPLOY_AGENT_TOKEN=<theirs>` got enforced against the real client.
func TestConsumeSessionToken_RefusesATokenFileItCannotUse(t *testing.T) {
	dir := t.TempDir()
	garbage := filepath.Join(dir, "garbage")
	require.NoError(t, os.WriteFile(garbage, []byte("garbage\n"), 0600))

	for name, path := range map[string]string{
		"missing":          filepath.Join(dir, "absent"),
		"not a token file": garbage,
	} {
		t.Run(name, func(t *testing.T) {
			// A token in the environment must not rescue it either: that is
			// exactly the fallback that served the control plane open.
			t.Setenv(token.Env, "token-from-the-environment")

			got, err := consumeSessionToken(zap.NewNop(), path, false)
			require.Error(t, err)
			require.Empty(t, got)
			require.Contains(t, err.Error(), "will not start")
		})
	}
}

func TestConsumeSessionToken_RefusesToStartWithoutAToken(t *testing.T) {
	t.Setenv(token.Env, "")
	t.Setenv(token.InsecureNoAuthEnv, "")

	got, err := consumeSessionToken(zap.NewNop(), "", false)
	require.ErrorIs(t, err, ErrNoToken, "an agent started with no token must not start")
	require.Empty(t, got, "a token invented here would be enforced against clients that cannot know it")
}

// TestConsumeSessionToken_LeavesTheOptOutToGuard: consumeSessionToken refuses
// an agent with no token whatever the opt-out says, and says nothing: only
// Guard reads the opt-out.
func TestConsumeSessionToken_LeavesTheOptOutToGuard(t *testing.T) {
	t.Setenv(token.Env, "")
	t.Setenv(token.InsecureNoAuthEnv, "true")
	core, logs := observer.New(zapcore.DebugLevel)

	got, err := consumeSessionToken(zap.New(core), "", true)
	require.ErrorIs(t, err, ErrNoToken)
	require.Empty(t, got)
	require.Zero(t, logs.Len())
}

func TestConsumeSessionToken_MakesTheProcessAgreeWithWhatItEnforces(t *testing.T) {
	// Without the Adopt, a later token.Session() in the agent mints an
	// unrelated value that looks entirely valid and matches nothing.
	path := filepath.Join(t.TempDir(), "tok")
	require.NoError(t, os.WriteFile(path, []byte(token.Env+"=the-enforced-token\n"), 0600))

	got, err := consumeSessionToken(zap.NewNop(), path, false)
	require.NoError(t, err)
	require.Equal(t, got, token.Session())
}

// TestConsumeSessionToken_SaysWhatIsTrueForTheAgentItIs pins why an agent
// with no token does not start, and what to do about it. On a laptop or CI
// runner that is a handoff that did not happen, and the remedy is to start it
// through keploy or hand it KEPLOY_AGENT_TOKEN. In a pod, only k8s-proxy hands
// an agent its token, so the remedy is k8s-proxy's. keploy running natively as
// root in a CI pod starts its agent without sudo, so that agent sees the pod's
// environment too, and it must still be told the local remedy. Only the local
// message names the opt-out: in a cluster it would leave the agent open to
// every pod. Nothing is logged here: the caller logs the refusal.
func TestConsumeSessionToken_SaysWhatIsTrueForTheAgentItIs(t *testing.T) {
	const local = "no control-plane token: start the agent through keploy, or set KEPLOY_AGENT_TOKEN"
	// In full for a pod's agent: a substring would let any other remedy
	// through, the opt-out included.
	const inCluster = "no control-plane token: this in-cluster agent was started without KEPLOY_AGENT_TOKEN. " +
		"k8s-proxy gives every agent it starts its own token; upgrade k8s-proxy to a release that does (see k8s-proxy's " +
		"UPGRADING.md). If you start this pod's agent yourself, give it KEPLOY_AGENT_TOKEN from a Secret " +
		"(valueFrom.secretKeyRef) and the same value to its client"
	for _, tc := range []struct {
		name     string
		isDocker bool
		k8sHost  string
		want     string
	}{
		{"native agent", false, "", local},
		{"docker agent keploy launched", true, "", local},
		{"native agent keploy launched inside a CI pod", false, "10.96.0.1", local},
		{"in-cluster agent", true, "10.96.0.1", inCluster},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(token.Env, "")
			t.Setenv(token.InsecureNoAuthEnv, "")
			t.Setenv("KUBERNETES_SERVICE_HOST", tc.k8sHost)
			core, logs := observer.New(zapcore.DebugLevel)

			got, err := consumeSessionToken(zap.New(core), "", tc.isDocker)
			require.ErrorIs(t, err, ErrNoToken)
			require.Empty(t, got)
			if tc.want == inCluster {
				require.Equal(t, inCluster, err.Error(), "an in-cluster agent is pointed at k8s-proxy, not the opt-out")
			} else {
				require.Contains(t, err.Error(), tc.want)
				require.Contains(t, err.Error(), token.InsecureNoAuthEnv+"=true")
			}
			require.Zero(t, logs.Len(), "the refusal is the caller's to log")
		})
	}
}

// TestConsumeSessionToken_InClusterAgentKeepsTheTokenItWasGiven: in a sidecar
// install k8s-proxy puts a KEPLOY_AGENT_TOKEN on every agent it injects (the
// recording sidecars, and the replay and sandbox pods it creates), and that
// agent is in-cluster by every test consumeSessionToken applies. It must
// enforce that token and say nothing, not take the no-token in-cluster path:
// checking for the pod before reading the environment would refuse to start
// every such agent, though k8s-proxy handed it a token.
func TestConsumeSessionToken_InClusterAgentKeepsTheTokenItWasGiven(t *testing.T) {
	t.Setenv(token.Env, "token-k8s-proxy-minted")
	t.Setenv("KUBERNETES_SERVICE_HOST", "10.96.0.1")
	core, logs := observer.New(zapcore.DebugLevel)

	got, err := consumeSessionToken(zap.New(core), "", true)
	require.NoError(t, err)
	require.Equal(t, "token-k8s-proxy-minted", got)
	require.Equal(t, got, token.Session())
	require.Zero(t, logs.Len(), "an agent enforcing its token has nothing to say about running without one")
}
