package utils

import (
	"encoding/json"
	"errors"
	"io"
	"sort"
	"testing"
)

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

func TestIsShutdownError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil error", nil, false},
		{"io.EOF", io.EOF, true},
		{"io.ErrUnexpectedEOF", io.ErrUnexpectedEOF, true},
		{"wrapped EOF", errors.New("read tcp: EOF"), true},
		{"connection refused", errors.New("dial tcp 127.0.0.1:8080: connect: connection refused"), true},
		{"connection reset", errors.New("read: connection reset by peer"), true},
		{"broken pipe", errors.New("write: broken pipe"), true},
		{"use of closed network connection", errors.New("read tcp: use of closed network connection"), true},
		{"unrelated error", errors.New("invalid syntax"), false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := IsShutdownError(tc.err)
			if got != tc.want {
				t.Errorf("IsShutdownError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestReplaceHost(t *testing.T) {
	cases := []struct {
		name       string
		currentURL string
		ipAddress  string
		want       string
		wantErr    bool
	}{
		{
			name:       "valid http url",
			currentURL: "http://example.com:8080/path?query=1",
			ipAddress:  "127.0.0.1",
			want:       "http://127.0.0.1:8080/path?query=1",
			wantErr:    false,
		},
		{
			name:       "empty ip address",
			currentURL: "http://example.com/api",
			ipAddress:  "",
			want:       "http://example.com/api",
			wantErr:    true,
		},
		{
			name:       "invalid url",
			currentURL: "://invalid-url",
			ipAddress:  "127.0.0.1",
			want:       "://invalid-url",
			wantErr:    true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ReplaceHost(tc.currentURL, tc.ipAddress)
			if (err != nil) != tc.wantErr {
				t.Fatalf("ReplaceHost() error = %v, wantErr %v", err, tc.wantErr)
			}
			if got != tc.want {
				t.Errorf("ReplaceHost() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestReplaceGrpcHost(t *testing.T) {
	cases := []struct {
		name      string
		authority string
		ipAddress string
		want      string
		wantErr   bool
	}{
		{
			name:      "valid authority with port",
			authority: "localhost:50051",
			ipAddress: "10.0.0.2",
			want:      "10.0.0.2:50051",
			wantErr:   false,
		},
		{
			name:      "empty ip address",
			authority: "localhost:50051",
			ipAddress: "",
			want:      "localhost:50051",
			wantErr:   true,
		},
		{
			name:      "invalid authority without port",
			authority: "localhost",
			ipAddress: "10.0.0.2",
			want:      "localhost",
			wantErr:   true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ReplaceGrpcHost(tc.authority, tc.ipAddress)
			if (err != nil) != tc.wantErr {
				t.Fatalf("ReplaceGrpcHost() error = %v, wantErr %v", err, tc.wantErr)
			}
			if got != tc.want {
				t.Errorf("ReplaceGrpcHost() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestReplaceGrpcPort(t *testing.T) {
	cases := []struct {
		name      string
		authority string
		port      string
		want      string
		wantErr   bool
	}{
		{
			name:      "valid host and port",
			authority: "127.0.0.1:50051",
			port:      "9000",
			want:      "127.0.0.1:9000",
			wantErr:   false,
		},
		{
			name:      "host without port",
			authority: "localhost",
			port:      "9000",
			want:      "localhost:9000",
			wantErr:   false,
		},
		{
			name:      "empty port",
			authority: "localhost:50051",
			port:      "",
			want:      "localhost:50051",
			wantErr:   true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ReplaceGrpcPort(tc.authority, tc.port)
			if (err != nil) != tc.wantErr {
				t.Fatalf("ReplaceGrpcPort() error = %v, wantErr %v", err, tc.wantErr)
			}
			if got != tc.want {
				t.Errorf("ReplaceGrpcPort() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestReplaceBaseURL(t *testing.T) {
	cases := []struct {
		name       string
		currentURL string
		baseURL    string
		want       string
		wantErr    bool
	}{
		{
			name:       "valid replacement",
			currentURL: "http://old-domain.com:8080/v1/users?id=1",
			baseURL:    "https://new-domain.com",
			want:       "https://new-domain.com/v1/users?id=1",
			wantErr:    false,
		},
		{
			name:       "empty baseURL",
			currentURL: "http://old-domain.com/v1/users",
			baseURL:    "",
			want:       "http://old-domain.com/v1/users",
			wantErr:    true,
		},
		{
			name:       "invalid currentURL",
			currentURL: "://invalid-url",
			baseURL:    "https://new-domain.com",
			want:       "://invalid-url",
			wantErr:    true,
		},
		{
			name:       "invalid baseURL",
			currentURL: "http://old-domain.com/v1/users",
			baseURL:    "://invalid-base",
			want:       "http://old-domain.com/v1/users",
			wantErr:    true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ReplaceBaseURL(tc.currentURL, tc.baseURL)
			if (err != nil) != tc.wantErr {
				t.Fatalf("ReplaceBaseURL() error = %v, wantErr %v", err, tc.wantErr)
			}
			if got != tc.want {
				t.Errorf("ReplaceBaseURL() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestReplacePort(t *testing.T) {
	cases := []struct {
		name       string
		currentURL string
		port       string
		want       string
		wantErr    bool
	}{
		{
			name:       "replace existing port",
			currentURL: "http://localhost:8080/api",
			port:       "9090",
			want:       "http://localhost:9090/api",
			wantErr:    false,
		},
		{
			name:       "add port when none exists",
			currentURL: "http://localhost/api",
			port:       "3000",
			want:       "http://localhost:3000/api",
			wantErr:    false,
		},
		{
			name:       "empty port",
			currentURL: "http://localhost:8080/api",
			port:       "",
			want:       "http://localhost:8080/api",
			wantErr:    true,
		},
		{
			name:       "invalid url",
			currentURL: "://invalid-url",
			port:       "8080",
			want:       "://invalid-url",
			wantErr:    true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ReplacePort(tc.currentURL, tc.port)
			if (err != nil) != tc.wantErr {
				t.Fatalf("ReplacePort() error = %v, wantErr %v", err, tc.wantErr)
			}
			if got != tc.want {
				t.Errorf("ReplacePort() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestToInt(t *testing.T) {
	cases := []struct {
		name  string
		input interface{}
		want  int
	}{
		{"int", 42, 42},
		{"int64", int64(100), 100},
		{"int32", int32(50), 50},
		{"float32", float32(23.4), 23},
		{"float64", float64(88.9), 88},
		{"valid string", "123", 123},
		{"invalid string", "abc", 0},
		{"json.Number int", json.Number("456"), 456},
		{"json.Number float", json.Number("78.9"), 78},
		{"unsupported type bool", true, 0},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ToInt(tc.input)
			if got != tc.want {
				t.Errorf("ToInt(%v) = %v, want %v", tc.input, got, tc.want)
			}
		})
	}
}

func TestToString(t *testing.T) {
	cases := []struct {
		name  string
		input interface{}
		want  string
	}{
		{"int", 42, "42"},
		{"int64", int64(100), "100"},
		{"int32", int32(50), "50"},
		{"float64", float64(12.34), "12.34"},
		{"float32", float32(5.5), "5.5"},
		{"string", "hello", "hello"},
		{"unsupported bool", true, ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ToString(tc.input)
			if got != tc.want {
				t.Errorf("ToString(%v) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

func TestToFloat(t *testing.T) {
	cases := []struct {
		name  string
		input interface{}
		want  float64
	}{
		{"float64", 3.1415, 3.1415},
		{"int", 42, 42.0},
		{"valid string", "2.718", 2.718},
		{"invalid string", "invalid", 0.0},
		{"unsupported type", true, 0.0},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ToFloat(tc.input)
			if got != tc.want {
				t.Errorf("ToFloat(%v) = %v, want %v", tc.input, got, tc.want)
			}
		})
	}
}

func TestKeys(t *testing.T) {
	input := map[string][]string{
		"headerA": {"val1", "val2"},
		"headerB": {"val3"},
		"headerC": {},
	}

	got := Keys(input)
	sort.Strings(got)
	want := []string{"headerA", "headerB", "headerC"}

	if len(got) != len(want) {
		t.Fatalf("Keys() returned %d keys, want %d", len(got), len(want))
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("Keys()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestEnsureRmBeforeName(t *testing.T) {
	cases := []struct {
		name string
		cmd  string
		want string
	}{
		{
			name: "adds --rm before --name when --rm is absent",
			cmd:  "docker run -d --name my-container my-image",
			want: "docker run -d --rm --name my-container my-image",
		},
		{
			name: "does not duplicate --rm if already present",
			cmd:  "docker run -d --rm --name my-container my-image",
			want: "docker run -d --rm --name my-container my-image",
		},
		{
			name: "leaves command unchanged if --name is absent",
			cmd:  "docker run -d my-image",
			want: "docker run -d my-image",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := EnsureRmBeforeName(tc.cmd)
			if got != tc.want {
				t.Errorf("EnsureRmBeforeName(%q) = %q, want %q", tc.cmd, got, tc.want)
			}
		})
	}
}
