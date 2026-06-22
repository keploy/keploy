package utils

import "testing"

func TestContainerNameFromDockerRun(t *testing.T) {
	cases := []struct {
		name string
		cmd  string
		want string
	}{
		{"space form", "docker run --rm --name dedup-go-test dedup-go:latest", "dedup-go-test"},
		{"equals form", "docker run --name=my-app img", "my-app"},
		{"name mid-flags", "docker run -d --name x --network y img", "x"},
		{"no name", "docker run --rm img", ""},
		{"empty", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ContainerNameFromDockerRun(tc.cmd); got != tc.want {
				t.Fatalf("ContainerNameFromDockerRun(%q) = %q, want %q", tc.cmd, got, tc.want)
			}
		})
	}
}
