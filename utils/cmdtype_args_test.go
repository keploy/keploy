package utils

import "testing"

// ShouldReexecWithSudo runs before cobra parses anything and long before any
// config file is read, so an explicit --cmd-type has to come out of raw argv.
// Without it, `--cmd-type docker-run -c "make up"` takes the native path,
// starts unprivileged, and dies later writing /proc/sys/kernel/perf_event_paranoid.
func TestExtractCmdTypeFromArgs(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"space separated", []string{"keploy", "record", "--cmd-type", "docker-compose", "-c", "make up"}, "docker-compose"},
		{"equals form", []string{"keploy", "record", "--cmd-type=docker-run"}, "docker-run"},
		{"normalised", []string{"keploy", "record", "--cmd-type", "  Docker-Compose "}, "docker-compose"},
		{"absent", []string{"keploy", "record", "-c", "make up"}, ""},
		{"explicitly empty", []string{"keploy", "record", "--cmd-type="}, ""},
		{"dangling at end", []string{"keploy", "record", "--cmd-type"}, ""},
		// Must not confuse itself with a value that merely contains the name.
		{"not confused by the command", []string{"keploy", "record", "-c", "echo --cmd-type docker-run"}, ""},

		// cli/provider's aliasNormalizeFunc maps cmdType -> cmd-type, so cobra
		// accepts the camelCase spelling and Changed("cmd-type") is true for
		// it. Missing it here would let --cmdType silently skip the privilege
		// decision — and the repo's own CI scripts use camelCase aliases.
		{"camelCase alias", []string{"keploy", "record", "--cmdType", "docker-compose"}, "docker-compose"},
		{"camelCase equals form", []string{"keploy", "record", "--cmdType=docker-run"}, "docker-run"},

		// cobra resolves a repeated flag to the last occurrence; disagreeing
		// would mean the privilege decision and the run mode come from
		// different values.
		{"last occurrence wins", []string{"keploy", "record", "--cmd-type", "native", "--cmd-type", "docker-compose"}, "docker-compose"},
		{"last wins across spellings", []string{"keploy", "record", "--cmdType", "docker-compose", "--cmd-type", "native"}, "native"},

		// Everything after -- is positional, not a flag.
		{"terminator", []string{"keploy", "record", "--", "--cmd-type", "docker-run"}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ExtractCmdTypeFromArgs(tc.args); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}
