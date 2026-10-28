package docker

import "testing"

func TestCurrentDockerContext(t *testing.T) {
	tests := []struct {
		name string
		out  string
		want string
	}{
		{"active context listed first", "colima\ttrue\ndefault\tfalse\n", "colima"},
		{"active context not listed first", "default\tfalse\ndesktop-linux\tfalse\ncolima\ttrue\n", "colima"},
		{"default context active", "default\ttrue\ncolima\tfalse\n", "default"},
		{"windows line endings", "default\tfalse\r\ncolima\ttrue\r\n", "colima"},
		{"no active context", "default\tfalse\ncolima\tfalse\n", ""},
		{"empty output", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := currentDockerContext([]byte(tt.out)); got != tt.want {
				t.Errorf("currentDockerContext(%q) = %q, want %q", tt.out, got, tt.want)
			}
		})
	}
}
