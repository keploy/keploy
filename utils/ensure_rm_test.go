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
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := EnsureRmBeforeName(tt.cmd); got != tt.want {
				t.Errorf("EnsureRmBeforeName(%q) = %q, want %q", tt.cmd, got, tt.want)
			}
		})
	}
}
