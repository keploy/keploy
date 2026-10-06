package docker

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/agent/token"
)

// TestSudoCannotKeepAgentToken: a sudo is refused exactly when it is known to
// start its command without the variable --preserve-env=NAME names, or to
// reject the flag, and every other sudo is let through.
func TestSudoCannotKeepAgentToken(t *testing.T) {
	for _, tc := range []struct {
		goos, version string
		// root is whether keploy is root: then the sudo is there for its
		// other options, and the refusal cannot tell keploy to run as root.
		root bool
		// refused is what the refusal names; empty when there is none.
		refused string
	}{
		{goos: "linux", version: "sudo-rs 0.2.5\n", refused: "sudo-rs 0.2.5"},
		{goos: "linux", version: "sudo-rs 0.2.6\n", refused: "sudo-rs 0.2.6"},
		{goos: "linux", version: "sudo-rs 0.1.9\n", refused: "sudo-rs 0.1.9"},
		{goos: "linux", version: "sudo-rs 0.2.7\n"},
		{goos: "linux", version: "sudo-rs 0.2.13\n"},
		{goos: "linux", version: "sudo-rs 0.10.0\n"},
		{goos: "linux", version: "sudo-rs 1.0.0\n"},
		{goos: "linux", version: "Sudo version 1.8.20p2\nSudoers policy plugin version 1.8.20p2\n", refused: "sudo 1.8.20"},
		{goos: "linux", version: "Sudo version 1.8.21\n"},
		// Whatever sudo says first, the version line decides.
		{goos: "linux", version: "sudo: unable to resolve host box: Name or service not known\nSudo version 1.8.20\n", refused: "sudo 1.8.20"},
		{goos: "linux", version: "warning: something\nsudo-rs 0.2.5\n", refused: "sudo-rs 0.2.5"},
		{goos: "linux", version: "Sudo version 1.9.16p2\nSudoers policy plugin version 1.9.16p2\n"},
		{goos: "linux", version: "Sudo version 1.10.0\n"},
		{goos: "darwin", version: "Sudo version 1.9.13p2\n"},
		// No sudo on PATH, or one that does not know --version: nothing says
		// it drops the variable.
		{goos: "linux", version: ""},
		{goos: "linux", version: "sudo: unknown option --version\n"},
		{goos: "windows", version: "", refused: "Windows"},
		{goos: "linux", version: "sudo-rs 0.2.5\n", root: true, refused: "sudo-rs 0.2.5"},
		{goos: "linux", version: "Sudo version 1.8.20\n", root: true, refused: "sudo 1.8.20"},
		{goos: "linux", version: "sudo-rs 0.2.7\n", root: true},
	} {
		err := sudoCannotKeepAgentToken(tc.goos, tc.version, tc.root)
		if tc.refused == "" {
			require.NoError(t, err, "%s %q", tc.goos, tc.version)
			continue
		}
		require.ErrorContains(t, err, tc.refused, "%s %q", tc.goos, tc.version)
		require.ErrorContains(t, err, token.Env, "the refusal does not say what sudo would drop")
		switch {
		case tc.goos == "windows":
			require.ErrorContains(t, err, "remove it from the command")
		case tc.root:
			require.NotContains(t, err.Error(), "as root instead", "keploy is root already")
			require.ErrorContains(t, err, "remove its other options")
		default:
			require.ErrorContains(t, err, "Run keploy itself as root instead")
		}
	}
}

// TestPrepareDockerCommand_RefusesASudoThatWouldDropTheToken: a keploy that is
// not root starts the agent container through `sudo --preserve-env=…
// docker`. sudo-rs before 0.2.7 ignores the flag, so docker would start the
// agent without a token, which then serves its control plane to anyone.
// PrepareDockerCommand refuses that sudo before anything runs, and asks no
// sudo anything when the alias does not go through one.
func TestPrepareDockerCommand_RefusesASudoThatWouldDropTheToken(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the alias runs through sh on unix; the windows arm hands it to cmd.exe")
	}
	for _, tc := range []struct {
		name, sudoVersion string
		root, noToken     bool
		refused           string
	}{
		{name: "sudo-rs 0.2.5", sudoVersion: "sudo-rs 0.2.5", refused: "sudo-rs 0.2.5"},
		{name: "sudo-rs 0.2.7", sudoVersion: "sudo-rs 0.2.7"},
		{name: "sudo 1.9.16p2", sudoVersion: "Sudo version 1.9.16p2"},
		{name: "root, sudo-rs 0.2.5", sudoVersion: "sudo-rs 0.2.5", root: true},
		// Nothing to keep, so nothing to refuse: sudo docker, as before.
		{name: "no token, sudo-rs 0.2.5", sudoVersion: "sudo-rs 0.2.5", noToken: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bin := t.TempDir()
			// A sudo that says what it is and keeps what --preserve-env names;
			// only the version decides.
			writeStub(t, bin, "sudo", `echo "$*" >> '`+bin+`/sudo.log'
if [ "$1" = --version ]; then echo "`+tc.sudoVersion+`" >&2; exit 0; fi
keep=${1#--preserve-env=}; shift
exec env -i PATH="$PATH" "$keep=$(printenv "$keep")" "$@"`)
			writeStub(t, bin, "docker", `printf %s "${`+token.Env+`-<unset>}" > '`+bin+`/docker.token'`)
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

			cmd, err := PrepareDockerCommand(context.Background(),
				linuxDockerClient(tc.root, !tc.noToken)+" container run --rm -e "+token.Env+" keploy-image")
			sudoLog, _ := os.ReadFile(filepath.Join(bin, "sudo.log"))
			if tc.root || tc.noToken {
				require.NoError(t, err)
				require.Empty(t, string(sudoLog), "keploy asked sudo what it is, with no token to hand through it")
				return
			}
			if tc.refused != "" {
				require.ErrorContains(t, err, tc.refused, "keploy would start the agent through a sudo that drops its token")
				require.Nil(t, cmd)
				require.Equal(t, "--version\n", string(sudoLog), "sudo was run for more than its version")
				return
			}
			require.NoError(t, err)
			require.Equal(t, "--version\n", string(sudoLog), "keploy did not ask the sudo it hands the token through what it is")
			out, err := cmd.CombinedOutput()
			require.NoError(t, err, "%s", out)
			got, err := os.ReadFile(filepath.Join(bin, "docker.token"))
			require.NoError(t, err, "docker never ran")
			require.Equal(t, token.Session(), string(got))
			sudoLog, _ = os.ReadFile(filepath.Join(bin, "sudo.log"))
			require.True(t, strings.HasSuffix(string(sudoLog), "--preserve-env="+token.Env+" docker container run --rm -e "+token.Env+" keploy-image\n"),
				"docker was not started through sudo --preserve-env: %s", sudoLog)
		})
	}
}
