//go:build linux

package utils

import (
	"os"
	"testing"
)

func TestIsCloudReplayCmd(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want bool
	}{
		{
			name: "cloud replay",
			args: []string{"keploy", "cloud", "replay"},
			want: true,
		},
		{
			name: "cloud replay with flags",
			args: []string{"keploy", "cloud", "replay", "--verbose"},
			want: true,
		},
		{
			name: "persistent flag before subcommands",
			args: []string{"keploy", "--debug", "cloud", "replay"},
			want: true,
		},
		{
			name: "persistent flag between cloud and replay",
			args: []string{"keploy", "cloud", "--debug", "replay"},
			want: true,
		},
		{
			name: "flag with value between cloud and replay",
			args: []string{"keploy", "cloud", "--config", "/tmp/cfg", "replay"},
			want: true,
		},
		{
			name: "flag with inline value between cloud and replay",
			args: []string{"keploy", "cloud", "--config=/tmp/cfg", "replay"},
			want: true,
		},
		{
			name: "cloud without replay",
			args: []string{"keploy", "cloud", "record"},
			want: false,
		},
		{
			name: "only cloud",
			args: []string{"keploy", "cloud"},
			want: false,
		},
		{
			name: "replay without cloud",
			args: []string{"keploy", "replay"},
			want: false,
		},
		{
			name: "no subcommand",
			args: []string{"keploy"},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isCloudReplayCmd(tt.args); got != tt.want {
				t.Errorf("isCloudReplayCmd(%v) = %v, want %v", tt.args, got, tt.want)
			}
		})
	}
}

// ShouldReexecWithSudo decides whether keploy needs root, from raw argv,
// before cobra or any config file is involved. An explicit --cmd-type has to
// win here or `--cmd-type docker-run -c "make up"` starts unprivileged and
// dies much later writing /proc/sys/kernel/perf_event_paranoid (#4399).
//
// Pins the wiring, not just the argv helper: dropping the ExtractCmdTypeFromArgs
// call would leave that helper's own test green while the feature stayed broken.
func TestShouldReexecWithSudo_HonoursExplicitCmdType(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("runs as root; ShouldReexecWithSudo short-circuits before the cmd-type check")
	}

	cases := []struct {
		name string
		args []string
		want bool
	}{
		// The feature: a command that does not look like docker, declared as docker.
		{"explicit docker-compose on a wrapper", []string{"keploy", "record", "--cmd-type", "docker-compose", "-c", "make up"}, true},
		{"explicit docker-run on a script", []string{"keploy", "record", "--cmd-type=docker-run", "-c", "./start.sh"}, true},
		// The inverse: a docker-looking command declared native still needs no root.
		{"explicit native on a docker command", []string{"keploy", "record", "--cmd-type", "native", "-c", "docker compose up"}, false},
		// Unchanged behaviour when the flag is absent.
		{"no flag, docker command", []string{"keploy", "record", "-c", "docker compose up"}, true},
		{"no flag, plain command", []string{"keploy", "record", "-c", "python app.py"}, false},
		// An unrecognised value falls through to sniffing; ValidateFlags reports it properly later.
		{"garbage falls back to sniffing", []string{"keploy", "record", "--cmd-type", "nope", "-c", "docker compose up"}, true},
		// The camelCase alias cobra also accepts must not skip this gate.
		{"camelCase alias on a wrapper", []string{"keploy", "record", "--cmdType", "docker-compose", "-c", "make up"}, true},
		{"camelCase alias native on a docker command", []string{"keploy", "record", "--cmdType=native", "-c", "docker compose up"}, false},
	}

	orig := os.Args
	defer func() { os.Args = orig }()

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			os.Args = tc.args
			if got := ShouldReexecWithSudo(); got != tc.want {
				t.Errorf("ShouldReexecWithSudo() = %v, want %v for %v", got, tc.want, tc.args)
			}
		})
	}
}
