package utils

import "testing"

func TestEnsureRmBeforeName(t *testing.T) {
	cases := []struct {
		name string
		cmd  string
		want string
	}{
		{"no name", "docker run -p 8080:8080 img", "docker run -p 8080:8080 img"},
		{"name gets rm", "docker run --name app img", "docker run --rm --name app img"},
		{"name equals form gets rm", "docker run --name=app img", "docker run --rm --name=app img"},
		{"rm before name is kept", "docker run --rm --name app img", "docker run --rm --name app img"},
		{"rm before name equals form is kept", "docker run --rm --name=app img", "docker run --rm --name=app img"},
		{"rm goes in front of name only", "docker run -p 8080:8080 --name app img", "docker run -p 8080:8080 --rm --name app img"},
		{"first name wins", "docker run --name a --name=b img", "docker run --rm --name a --name=b img"},
		{"other flags starting with --name are not a name", "docker run --name-prefix x img", "docker run --name-prefix x img"},
		{"empty", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := EnsureRmBeforeName(tc.cmd); got != tc.want {
				t.Errorf("EnsureRmBeforeName(%q) = %q, want %q", tc.cmd, got, tc.want)
			}
		})
	}
}
