package utils

import "testing"

// EnsureRmBeforeName puts --rm before --name in a docker run command, for
// both spellings Docker accepts: "--name app" and "--name=app" (keploy #4981).
func TestEnsureRmBeforeName(t *testing.T) {
	tests := []struct {
		name string
		cmd  string
		want string
	}{
		{
			name: "two-word --name gets --rm",
			cmd:  "docker run --name app img",
			want: "docker run --rm --name app img",
		},
		{
			name: "--name=value gets --rm",
			cmd:  "docker run --name=app img",
			want: "docker run --rm --name=app img",
		},
		{
			name: "--name=value after other flags gets --rm",
			cmd:  "docker run -d -p 8080:8080 --name=app img",
			want: "docker run -d -p 8080:8080 --rm --name=app img",
		},
		{
			name: "--rm already before --name is kept as is",
			cmd:  "docker run --rm --name app img",
			want: "docker run --rm --name app img",
		},
		{
			name: "--rm already before --name=value is kept as is",
			cmd:  "docker run --rm --name=app img",
			want: "docker run --rm --name=app img",
		},
		{
			name: "no --name is left unchanged",
			cmd:  "docker run img",
			want: "docker run img",
		},
		{
			name: "an empty --name= is not a name",
			cmd:  "docker run --name= img",
			want: "docker run --name= img",
		},
		{
			name: "a flag that only starts with --name is not --name",
			cmd:  "docker run --namespace=x img",
			want: "docker run --namespace=x img",
		},
		{
			name: "--rm placed after --name is not duplicated",
			cmd:  "docker run --name app --rm img",
			want: "docker run --name app --rm img",
		},
		{
			name: "--rm placed after --name=value is not duplicated",
			cmd:  "docker run --name=app --rm img",
			want: "docker run --name=app --rm img",
		},
		{
			name: "--rm=true placed after --name=value is respected",
			cmd:  "docker run --name=app --rm=true img",
			want: "docker run --name=app --rm=true img",
		},
		{
			name: "--rm=false placed after --name=value is respected",
			cmd:  "docker run --name=app --rm=false img",
			want: "docker run --name=app --rm=false img",
		},
		{
			name: "post-image --name=value container arg is not modified",
			cmd:  "docker run myimg ./server --name=prod",
			want: "docker run myimg ./server --name=prod",
		},
		{
			name: "post-image --name container arg is not modified",
			cmd:  "docker run -d -p 8080:8080 myimg ./server --name prod",
			want: "docker run -d -p 8080:8080 myimg ./server --name prod",
		},
		{
			name: "post-image --rm container arg does not suppress docker --rm",
			cmd:  "docker run --name=app myimg ./server --rm",
			want: "docker run --rm --name=app myimg ./server --rm",
		},
		{
			name: "quoted flag value containing --name= is not modified",
			cmd:  `docker run -e APP_ARGS="foo --name=bar" myimg`,
			want: `docker run -e APP_ARGS="foo --name=bar" myimg`,
		},
		{
			name: "quoted flag value matching --name= exactly is consumed as flag value",
			cmd:  `docker run -e "--name=bar" myimg`,
			want: `docker run -e "--name=bar" myimg`,
		},
		{
			name: "quoted flag value before real --name= inserts --rm before real --name=",
			cmd:  `docker run -e APP_ARGS="foo --name=bar" --name=app myimg ./server --name=prod`,
			want: `docker run -e APP_ARGS="foo --name=bar" --rm --name=app myimg ./server --name=prod`,
		},
		{
			name: "single-quoted flag value containing --name= is not modified",
			cmd:  `docker run -e 'APP_ARGS=foo --name=bar' myimg`,
			want: `docker run -e 'APP_ARGS=foo --name=bar' myimg`,
		},
		{
			name: "tabs and newlines are preserved when inserting --rm",
			cmd:  "docker run\t-d\n\t--name=app\timg",
			want: "docker run\t-d\n\t--rm --name=app\timg",
		},
		{
			name: "attached short flag value before --name= is handled",
			cmd:  "docker run -p8080:80 --name=app img",
			want: "docker run -p8080:80 --rm --name=app img",
		},
		{
			name: "docker container run and sudo forms work",
			cmd:  "sudo docker container run --detach --name=app img",
			want: "sudo docker container run --detach --rm --name=app img",
		},
		{
			name: "podman run with global flag works",
			cmd:  "podman --connection dev run --name=app img",
			want: "podman --connection dev run --rm --name=app img",
		},
		{
			name: "unterminated quote leaves command unchanged",
			cmd:  `docker run -e "unterminated --name=app img`,
			want: `docker run -e "unterminated --name=app img`,
		},
		{
			name: "sudo with attached short value flag ending in flag letter works",
			cmd:  "sudo -uroot docker run --name=app img",
			want: "sudo -uroot docker run --rm --name=app img",
		},
		{
			name: "sudo with bundled short flags ending in value flag works",
			cmd:  "sudo -Eu root docker run --name=app img",
			want: "sudo -Eu root docker run --rm --name=app img",
		},
		{
			name: "podman -r (--remote) global boolean flag works",
			cmd:  "podman -r run --name=app img",
			want: "podman -r run --rm --name=app img",
		},
		{
			name: "podman -Dr bundled global boolean flags work",
			cmd:  "podman -Dr run --name=app img",
			want: "podman -Dr run --rm --name=app img",
		},
		{
			name: "empty command is left unchanged",
			cmd:  "",
			want: "",
		},
		{
			name: "non-run docker subcommand is left unchanged",
			cmd:  "docker start --name=app",
			want: "docker start --name=app",
		},
		{
			name: "non-docker command is left unchanged",
			cmd:  "echo --name=app",
			want: "echo --name=app",
		},
		{
			name: "dangling --name without value is left unchanged",
			cmd:  "docker run --name",
			want: "docker run --name",
		},
		{
			name: "empty quoted two-word --name is left unchanged",
			cmd:  `docker run --name "" img`,
			want: `docker run --name "" img`,
		},
		{
			name: "quoted --name=value gets --rm before it",
			cmd:  `docker run "--name=app" img`,
			want: `docker run --rm "--name=app" img`,
		},
		{
			name: "quoted value in --name= gets --rm before it",
			cmd:  `docker run --name="app" img`,
			want: `docker run --rm --name="app" img`,
		},
		{
			name: "short-flag cluster with trailing value flag before --name=",
			cmd:  "docker run -itp 8080:80 --name=app img",
			want: "docker run -itp 8080:80 --rm --name=app img",
		},
		{
			name: "short-flag boolean cluster before --name=",
			cmd:  "docker run -it --name=app img",
			want: "docker run -it --rm --name=app img",
		},
		{
			name: "escaped backslash inside double quotes before --name=",
			cmd:  `docker run -e FOO="a\\" --name=app img`,
			want: `docker run -e FOO="a\\" --rm --name=app img`,
		},
		{
			name: "line continuation before --name=",
			cmd:  "docker run -d \\\n--name=app img",
			want: "docker run -d \\\n--rm --name=app img",
		},
		{
			name: "empty --name= followed by real --name= inserts before real --name=",
			cmd:  "docker run --name= --name=app img",
			want: "docker run --name= --rm --name=app img",
		},
		{
			name: "duplicate --name= inserts once before first --name=",
			cmd:  "docker run --name=a --name=b img",
			want: "docker run --rm --name=a --name=b img",
		},
		{
			name: "leading env assignment before docker run works",
			cmd:  "DOCKER_HOST=unix:///var/run/docker.sock docker run --name=app img",
			want: "DOCKER_HOST=unix:///var/run/docker.sock docker run --rm --name=app img",
		},
		{
			name: "double-dash end-of-options stops scanning",
			cmd:  "docker run -- --name=app",
			want: "docker run -- --name=app",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := EnsureRmBeforeName(tt.cmd); got != tt.want {
				t.Errorf("EnsureRmBeforeName(%q) = %q, want %q", tt.cmd, got, tt.want)
			}
		})
	}
}
