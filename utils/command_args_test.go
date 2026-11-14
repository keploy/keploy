package utils

import "testing"

// ShouldReexecWithSudo reads -c/--command out of raw argv before cobra parses
// anything, so it has to read the value cobra will run. Reading another one
// re-executes a native command under sudo, or starts a docker command
// unprivileged, which then fails writing /proc/sys/kernel/perf_event_paranoid.
func TestExtractCommandFromArgs(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"shorthand", []string{"keploy", "record", "-c", "python app.py"}, "python app.py"},
		{"long form", []string{"keploy", "record", "--command", "go run ."}, "go run ."},
		{"shorthand equals form", []string{"keploy", "record", "-c=npm start"}, "npm start"},
		{"long equals form", []string{"keploy", "record", "--command=cargo run"}, "cargo run"},
		{"absent", []string{"keploy", "record", "--delay", "10"}, ""},
		{"dangling at end", []string{"keploy", "record", "-c"}, ""},
		{"not confused by another flag", []string{"keploy", "record", "--cmd-type", "native", "--container-name", "app"}, ""},

		// pflag reads a shorthand's value joined to it: -c"docker compose up"
		// reaches keploy as one argument.
		{"shorthand joined value", []string{"keploy", "record", "-cdocker compose up"}, "docker compose up"},
		// ...and drops the "=" of -c=VALUE only when a value follows it.
		{"bare shorthand equals", []string{"keploy", "record", "-c="}, "="},

		// cobra resolves a repeated flag to its last occurrence.
		{"last occurrence wins", []string{"keploy", "record", "-c", "python app.py", "-c", "docker compose up"}, "docker compose up"},
		{"last wins across spellings", []string{"keploy", "record", "--command=docker compose up", "-c", "python app.py"}, "python app.py"},
		{"last wins when it is empty", []string{"keploy", "record", "-c", "docker compose up", "--command="}, ""},

		// A value is taken whole, even when it looks like a flag.
		{"value that looks like a flag", []string{"keploy", "record", "-c", "--command", "-c", "./app"}, "./app"},

		// Everything after -- is positional, not a flag.
		{"terminator", []string{"keploy", "record", "-c", "./app", "--", "-c", "docker compose up"}, "./app"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ExtractCommandFromArgs(tc.args); got != tc.want {
				t.Errorf("ExtractCommandFromArgs(%q) = %q, want %q", tc.args, got, tc.want)
			}
		})
	}
}
