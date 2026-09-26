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

// ConsumeSessionToken is the single point where the agent decides whether it
// has a credential to enforce. If it returns "" the agent serves its whole
// control plane unauthenticated while the CLI keeps sending a header nothing
// checks — and nothing else in the process would notice.

func TestConsumeSessionToken_ReadsTheFileTheLauncherPassed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tok")
	require.NoError(t, os.WriteFile(path, []byte(token.Env+"=a-real-token\n"), 0600))

	got, err := ConsumeSessionToken(zap.NewNop(), path, false)
	require.NoError(t, err)
	require.Equal(t, "a-real-token", got)
}

func TestConsumeSessionToken_UnlinksTheFileOnceItHasBeenRead(t *testing.T) {
	// The token's whole reason for being in a file rather than the environment
	// is that argv and env are readable; leaving it on disk for the rest of the
	// run gives that back.
	path := filepath.Join(t.TempDir(), "tok")
	require.NoError(t, os.WriteFile(path, []byte(token.Env+"=a-real-token\n"), 0600))

	_, err := ConsumeSessionToken(zap.NewNop(), path, false)
	require.NoError(t, err)

	_, err = os.Stat(path)
	require.True(t, os.IsNotExist(err), "the session token is still on disk after the agent read it")
}

func TestConsumeSessionToken_ReadsTheEnvironmentForContainerModes(t *testing.T) {
	// In docker there is no sudo in between, so the token arrives as an env
	// var the docker or compose client passed through by name.
	t.Setenv(token.Env, "token-from-the-container-env")

	got, err := ConsumeSessionToken(zap.NewNop(), "", true)
	require.NoError(t, err)
	require.Equal(t, "token-from-the-container-env", got)
}

func TestConsumeSessionToken_PrefersTheFileOverTheEnvironment(t *testing.T) {
	t.Setenv(token.Env, "stale-env-token")
	path := filepath.Join(t.TempDir(), "tok")
	require.NoError(t, os.WriteFile(path, []byte(token.Env+"=fresh-file-token\n"), 0600))

	got, err := ConsumeSessionToken(zap.NewNop(), path, false)
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

			got, err := ConsumeSessionToken(zap.NewNop(), path, false)
			require.Error(t, err)
			require.Empty(t, got)
			require.Contains(t, err.Error(), "will not start")
		})
	}
}

func TestConsumeSessionToken_ReportsNoTokenWhenThereIsNone(t *testing.T) {
	t.Setenv(token.Env, "")

	got, err := ConsumeSessionToken(zap.NewNop(), "", false)
	require.NoError(t, err, "an agent started with no handoff at all still serves, as before the token existed")
	require.Empty(t, got, "a token invented here would be enforced against clients that cannot know it")
}

func TestConsumeSessionToken_MakesTheProcessAgreeWithWhatItEnforces(t *testing.T) {
	// Without the Adopt, a later token.Session() in the agent mints an
	// unrelated value that looks entirely valid and matches nothing.
	path := filepath.Join(t.TempDir(), "tok")
	require.NoError(t, os.WriteFile(path, []byte(token.Env+"=the-enforced-token\n"), 0600))

	got, err := ConsumeSessionToken(zap.NewNop(), path, false)
	require.NoError(t, err)
	require.Equal(t, got, token.Session())
}

// TestConsumeSessionToken_SaysWhatIsTrueForTheAgentItIs pins the startup
// message for an agent with no token. On a laptop or CI runner that is a
// handoff that did not happen, and a warning naming the handoffs is right. In a
// pod, only k8s-proxy hands an agent its token, and only in a sidecar install:
// a sidecar, replay or sandbox pod it injected before its per-sidecar token
// release, any such pod in a DaemonSet install, or one injected with
// proxy.keployAgentControlPlaneAuth=false, has none. The local warning there
// names remedies that do not exist in a pod, so the in-cluster agent says, once
// and at info, which pods have no token and why — and must not give the old
// reason, a trusted cluster network, which k8s-proxy's tokens made false.
// keploy running natively as root in a CI pod starts its agent without sudo,
// so that agent sees the pod's environment too, and it must still get the
// warning.
func TestConsumeSessionToken_SaysWhatIsTrueForTheAgentItIs(t *testing.T) {
	const warning = "no --token-file"
	// In full: a substring and a list of forbidden words would let any other
	// explanation through, the old "the cluster network is trusted" included.
	const inCluster = "agent control-plane API is running without authentication: this in-cluster agent was started " +
		"without a control-plane token (in a sidecar install, k8s-proxy supplies one to the recording sidecars and " +
		"the replay and sandbox pods it injects, from its per-sidecar token release on; pods injected before that " +
		"release, pods in a DaemonSet install, and pods injected with proxy.keployAgentControlPlaneAuth=false have " +
		"none), so anything that can reach this pod's agent port can use the API"
	for _, tc := range []struct {
		name      string
		isDocker  bool
		k8sHost   string
		wantLevel zapcore.Level
		wantText  string // contained in the message
		wantExact bool   // and is the whole of it
	}{
		{"native agent", false, "", zapcore.WarnLevel, warning, false},
		{"docker agent keploy launched", true, "", zapcore.WarnLevel, warning, false},
		{"native agent keploy launched inside a CI pod", false, "10.96.0.1", zapcore.WarnLevel, warning, false},
		{"in-cluster agent", true, "10.96.0.1", zapcore.InfoLevel, inCluster, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(token.Env, "")
			t.Setenv("KUBERNETES_SERVICE_HOST", tc.k8sHost)
			core, logs := observer.New(zapcore.DebugLevel)

			_, err := ConsumeSessionToken(zap.New(core), "", tc.isDocker)
			require.NoError(t, err)

			require.Equal(t, 1, logs.Len(), "the agent must say, once, that its control plane is unauthenticated")
			entry := logs.All()[0]
			require.Equal(t, tc.wantLevel, entry.Level)
			if tc.wantExact {
				require.Equal(t, tc.wantText, entry.Message)
			} else {
				require.Contains(t, entry.Message, tc.wantText)
			}
		})
	}
}

// TestConsumeSessionToken_InClusterAgentKeepsTheTokenItWasGiven: in a sidecar
// install k8s-proxy puts a KEPLOY_AGENT_TOKEN on every agent it injects (the
// recording sidecars, and the replay and sandbox pods it creates), and that
// agent is in-cluster by every test ConsumeSessionToken applies. It must
// enforce that token and say nothing, not take the tokenless in-cluster path:
// checking for the pod before reading the environment would serve every such
// agent open while k8s-proxy went on presenting a token nothing checks.
func TestConsumeSessionToken_InClusterAgentKeepsTheTokenItWasGiven(t *testing.T) {
	t.Setenv(token.Env, "token-k8s-proxy-minted")
	t.Setenv("KUBERNETES_SERVICE_HOST", "10.96.0.1")
	core, logs := observer.New(zapcore.DebugLevel)

	got, err := ConsumeSessionToken(zap.New(core), "", true)
	require.NoError(t, err)
	require.Equal(t, "token-k8s-proxy-minted", got)
	require.Equal(t, got, token.Session())
	require.Zero(t, logs.Len(), "an agent enforcing its token has nothing to say about running without one")
}
