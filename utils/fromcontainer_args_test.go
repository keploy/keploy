package utils

import "testing"

// The privilege decision is taken from raw argv, before cobra parses anything
// and before any config file is read. A --from-container run launches a
// container and therefore needs root for the same reason every other docker
// kind does; missing it here starts the run unprivileged and it dies later
// writing /proc/sys/kernel/perf_event_paranoid (#4399).
func TestExtractFromContainerFromArgs(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"absent", []string{"keploy", "mock", "record", "-c", "pytest"}, ""},
		{"spaced", []string{"keploy", "mock", "record", "--from-container", "api"}, "api"},
		{"equals", []string{"keploy", "mock", "record", "--from-container=api"}, "api"},
		{"camelCase alias, which the CI scripts use", []string{"keploy", "--fromContainer", "api"}, "api"},
		{"last occurrence wins, as cobra resolves it", []string{"keploy", "--from-container", "a", "--from-container", "b"}, "b"},
		{"nothing after the terminator", []string{"keploy", "--", "--from-container", "api"}, ""},
		// Docker names are case-sensitive, so lowercasing here would hand the
		// daemon a name that does not exist. ExtractCmdTypeFromArgs lowercases
		// because kind names are lowercase by definition; this is not that.
		{"case is preserved", []string{"keploy", "--from-container", "MyApp"}, "MyApp"},
		{"case is preserved with an underscore", []string{"keploy", "--from-container=Payments_API"}, "Payments_API"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ExtractFromContainerFromArgs(tc.args); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// --cmd-type names a kind, so it stays lowercased.
func TestExtractCmdTypeStillLowercases(t *testing.T) {
	if got := ExtractCmdTypeFromArgs([]string{"keploy", "--cmd-type", "Docker-Compose"}); got != "docker-compose" {
		t.Errorf("got %q, want docker-compose", got)
	}
}

// FromContainer is the one kind with no command string, so every check written
// against "there is a command here" has to skip it rather than run against "".
func TestHasUserCommand(t *testing.T) {
	for kind, want := range map[CmdType]bool{
		Native: true, DockerRun: true, DockerStart: true, DockerCompose: true,
		FromContainer: false, Empty: false,
	} {
		if got := HasUserCommand(kind); got != want {
			t.Errorf("HasUserCommand(%q) = %v, want %v", kind, got, want)
		}
	}
}

// Everything that makes a run "docker-shaped" keys off this: creating the
// docker client, re-execing as root, starting the agent in a container, and
// skipping the native sudo-permission dance.
func TestIsDockerCmdIncludesFromContainer(t *testing.T) {
	if !IsDockerCmd(FromContainer) {
		t.Fatal("FromContainer must be a docker kind, or the run degrades to native everywhere at once: nil docker client, no sudo re-exec, agent started as a native process")
	}
}
