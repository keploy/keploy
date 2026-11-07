package utils

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
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

// GetAvailablePort must not hand back a port from the kernel's local port
// range.
//
// The oracle is read straight from /proc, NOT from ephemeralPortRange: using
// the production helper as its own oracle makes the test self-referential, and
// a helper that mis-reports the range (say 32768-40000) would then pass while
// handing out 40001-65534 — most of it inside the real range, i.e. exactly the
// bug this test exists to catch.
func TestGetAvailablePortAvoidsTheEphemeralRange(t *testing.T) {
	raw, err := os.ReadFile("/proc/sys/net/ipv4/ip_local_port_range")
	if err != nil {
		t.Skipf("no ip_local_port_range on this platform (%v); GetAvailablePort "+
			"correctly falls back to :0 there and there is nothing to assert", err)
	}
	f := strings.Fields(string(raw))
	if len(f) != 2 {
		t.Fatalf("unexpected ip_local_port_range content %q", raw)
	}
	lo64, err1 := strconv.ParseUint(f[0], 10, 32)
	hi64, err2 := strconv.ParseUint(f[1], 10, 32)
	if err1 != nil || err2 != nil {
		t.Fatalf("unparseable ip_local_port_range %q", raw)
	}
	lo, hi := uint32(lo64), uint32(hi64)
	if hi >= 65535 {
		t.Skipf("kernel range reaches %d, leaving no space above it; the fallback is "+
			"correct here and there is nothing to assert", hi)
	}

	for i := 0; i < 50; i++ {
		port, err := GetAvailablePort()
		if err != nil {
			t.Fatalf("GetAvailablePort: %v", err)
		}
		if port == 0 {
			t.Fatal("GetAvailablePort returned port 0")
		}
		if port >= lo && port <= hi {
			t.Fatalf("GetAvailablePort returned %d, inside the kernel's local port range "+
				"%d-%d read from /proc: every other bind(0) allocator on the machine — a "+
				"second keploy process, Docker's host-port publisher — draws from there "+
				"and can take it before the agent binds", port, lo, hi)
		}
	}
}

// The returned port must be one the function VERIFIED, not merely a number in
// the right band.
//
// Asserting "the returned port is bindable" is not enough: on an idle box
// almost any port above the ephemeral range binds, so that assertion holds by
// luck even if the bindability probe is deleted entirely. Instead, hold a block
// of candidates and require the function to return something outside it.
func TestGetAvailablePortSkipsPortsAlreadyHeld(t *testing.T) {
	if _, _, known := ephemeralPortRange(); !known {
		t.Skip("platform range unknown; GetAvailablePort falls back to :0 here")
	}
	_, hi, _ := ephemeralPortRange()
	if hi >= 65535 {
		t.Skip("no space above the ephemeral range on this host")
	}

	// Occupy a contiguous block so a probe-less implementation, which returns
	// whatever candidate it lands on, has a high chance of returning a held one.
	held := map[uint32]bool{}
	var lns []net.Listener
	for p := hi + 1; p <= 65535 && len(lns) < 2000; p++ {
		ln, err := net.Listen("tcp", fmt.Sprintf(":%d", p))
		if err != nil {
			continue
		}
		lns = append(lns, ln)
		held[p] = true
	}
	defer func() {
		for _, ln := range lns {
			_ = ln.Close()
		}
	}()
	if len(held) < 100 {
		t.Skipf("could only hold %d ports; too few to discriminate", len(held))
	}

	for i := 0; i < 200; i++ {
		port, err := GetAvailablePort()
		if err != nil {
			t.Fatalf("GetAvailablePort: %v", err)
		}
		if held[port] {
			t.Fatalf("GetAvailablePort returned %d, a port THIS TEST is holding: the "+
				"candidate was never verified bindable, so callers are handed ports "+
				"that are already in use", port)
		}
	}
}

// Successive callers must not all be handed the SAME port.
//
// Scanning upward from a fixed floor returns the lowest free port every time,
// so two keploy processes started moments apart — the ordinary CI pattern —
// are handed an identical number, each having verified it free. Whichever
// binds second loses, and the agent's bind retry cannot recover because the
// winner is a live holder, not a departing one.
//
// Sequential rather than concurrent: concurrent callers are separated anyway
// by the brief hold of the verification listener, so a concurrent test passes
// even with a fixed floor and proves nothing.
func TestGetAvailablePortDoesNotHandEveryCallerTheSamePort(t *testing.T) {
	const n = 12
	seen := map[uint32]int{}
	for i := 0; i < n; i++ {
		p, err := GetAvailablePort()
		if err != nil {
			t.Fatalf("GetAvailablePort: %v", err)
		}
		seen[p]++
	}
	if len(seen) < 2 {
		t.Fatalf("%d successive calls returned %d distinct port(s): candidates are not "+
			"randomised, so keploy processes started moments apart are handed the same "+
			"port and one of them fails to bind", n, len(seen))
	}
}

func TestEphemeralPortRangeIsSane(t *testing.T) {
	lo, hi, known := ephemeralPortRange()
	if !known {
		// Correct on any platform without ip_local_port_range. It MUST report
		// unknown rather than guessing: macOS and Windows both default to
		// 49152-65535, so assuming the Linux range would allocate entirely
		// inside the real ephemeral range there.
		if lo != 0 || hi != 0 {
			t.Fatalf("unknown range still reported %d-%d; callers must not act on it", lo, hi)
		}
		return
	}
	if lo == 0 || hi == 0 || hi < lo || hi > 65535 {
		t.Fatalf("ephemeralPortRange returned %d-%d, want a sane range", lo, hi)
	}
}

// The fallback branches are unreachable on a normal Linux box — the /proc read
// always succeeds — so they are tested through the pure parser instead. Without
// this, a broken default would be dead code that no test can reach.
func TestParseEphemeralPortRange(t *testing.T) {
	cases := []struct {
		name   string
		in     string
		lo, hi uint32
	}{
		{"normal", "32768\t60999\n", 32768, 60999},
		{"widened", "1024 65000\n", 1024, 65000},
		{"one field", "32768\n", defaultEphemeralLo, defaultEphemeralHi},
		{"three fields", "1 2 3\n", defaultEphemeralLo, defaultEphemeralHi},
		{"empty", "", defaultEphemeralLo, defaultEphemeralHi},
		{"non-numeric", "lo hi\n", defaultEphemeralLo, defaultEphemeralHi},
		{"inverted", "60999 32768\n", defaultEphemeralLo, defaultEphemeralHi},
		{"zero low", "0 60999\n", defaultEphemeralLo, defaultEphemeralHi},
		{"out of range high", "32768 70000\n", defaultEphemeralLo, defaultEphemeralHi},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lo, hi, ok := parseEphemeralPortRange(tc.in)
			if !ok {
				t.Fatal("parseEphemeralPortRange reported unknown; it is only called " +
					"when the file was read, so it must always resolve to a range")
			}
			if lo != tc.lo || hi != tc.hi {
				t.Fatalf("parseEphemeralPortRange(%q) = %d-%d, want %d-%d", tc.in, lo, hi, tc.lo, tc.hi)
			}
		})
	}

	// The Linux default must leave usable space above it, or GetAvailablePort
	// has nowhere to draw from on a host whose file is malformed and silently
	// falls back to the racy :0 path. This constrains the LINUX default only —
	// unknown platforms report known=false and fall back deliberately.
	if defaultEphemeralHi >= 65535 {
		t.Fatalf("Linux default high-water mark %d leaves no room above it",
			defaultEphemeralHi)
	}
}

// The agent, proxy and DNS ports are all allocated before ANY of them is bound,
// so a later draw can legitimately return a port an earlier one already
// claimed — isPortAvailable says yes, because nothing is listening on it yet.
// Narrowing allocation to above the ephemeral range shrinks the pool and makes
// that coincidence correspondingly more likely.
func TestEnsureAvailablePortsHonoursTheExclusionSet(t *testing.T) {
	// A port that IS free: without the exclusion set it is returned unchanged.
	free, err := GetAvailablePort()
	if err != nil {
		t.Fatalf("GetAvailablePort: %v", err)
	}

	if got, err := EnsureAvailablePorts(free); err != nil || got != free {
		t.Fatalf("precondition: a free port must be returned unchanged, got %d (%v)", got, err)
	}

	got, err := EnsureAvailablePorts(free, free)
	if err != nil {
		t.Fatalf("EnsureAvailablePorts: %v", err)
	}
	if got == free {
		t.Fatalf("EnsureAvailablePorts returned %d, which the caller had already claimed: "+
			"nothing is bound to it yet, so isPortAvailable says free and two subsystems "+
			"end up racing for the same port", got)
	}
}

// A platform without ip_local_port_range must report UNKNOWN, not guess.
//
// This is the branch that decides whether the allocator acts at all, and on
// Linux the read always succeeds so it is dead code no ordinary test reaches.
// Guessing the Linux range there is actively harmful: macOS and Windows both
// default to 49152-65535, so 61000-65534 would be entirely INSIDE their real
// ephemeral range while shrinking the pool from 16384 to 4535 — strictly worse
// than the plain :0 the fallback gives them.
func TestEphemeralPortRangeReportsUnknownWhenUnreadable(t *testing.T) {
	lo, hi, known := ephemeralPortRangeFrom(func(string) ([]byte, error) {
		return nil, os.ErrNotExist
	})
	if known {
		t.Fatalf("reported a known range %d-%d on a platform with no "+
			"ip_local_port_range; GetAvailablePort would then allocate from a band that "+
			"is inside the real ephemeral range on macOS and Windows", lo, hi)
	}
	if lo != 0 || hi != 0 {
		t.Fatalf("unknown range still returned %d-%d; callers must not act on it", lo, hi)
	}

	// A readable-but-malformed file is a different case: the platform IS Linux,
	// so the documented default is the right answer.
	lo, hi, known = ephemeralPortRangeFrom(func(string) ([]byte, error) {
		return []byte("garbage\n"), nil
	})
	if !known || lo != defaultEphemeralLo || hi != defaultEphemeralHi {
		t.Fatalf("malformed content gave %d-%d known=%v, want the Linux default %d-%d known=true",
			lo, hi, known, defaultEphemeralLo, defaultEphemeralHi)
	}
}

func TestNetworkNameFromDockerRun(t *testing.T) {
	cases := []struct {
		name string
		cmd  string
		want string
	}{
		{"space form", "docker run --name app --network keploy-network img", "keploy-network"},
		{"equals form", "docker run --name=app --network=keploy-network img", "keploy-network"},
		{"net alias, space form", "docker run --net keploy-network img", "keploy-network"},
		{"net alias, equals form", "docker run --net=keploy-network img", "keploy-network"},
		{"no network", "docker run --name app img", ""},
		// An empty "--flag=" must not be taken as the value and stop the scan.
		{"empty equals then a real flag", "docker run --net= --network mynet img", "mynet"},
		{"flag with no value", "docker run --network", ""},
		{"empty", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := NetworkNameFromDockerRun(tc.cmd); got != tc.want {
				t.Fatalf("NetworkNameFromDockerRun(%q) = %q, want %q", tc.cmd, got, tc.want)
			}
		})
	}
}

// uniqueProcessGroups returns each process's group, as read, once: the
// interrupt signals groups, and a group signalled twice is waited on twice.
func TestUniqueProcessGroups(t *testing.T) {
	groups := map[int]int{10: 10, 11: 10, 12: 12, 13: 10}
	got, err := uniqueProcessGroups([]int{11, 12, 13, 10}, func(pid int) (int, error) { return groups[pid], nil })
	if err != nil || fmt.Sprint(got) != "[10 12]" {
		t.Fatalf("uniqueProcessGroups = %v, %v; want [10 12]", got, err)
	}
	failed := fmt.Errorf("process 12 is not in the process table")
	if _, err := uniqueProcessGroups([]int{10, 12}, func(pid int) (int, error) {
		if pid == 12 {
			return 0, failed
		}
		return groups[pid], nil
	}); err != failed {
		t.Fatalf("uniqueProcessGroups with an unreadable group returned %v, want %v", err, failed)
	}
}

// The shell runs `docker  run` and `docker<TAB>run` as `docker run`, so the
// kind is the same.
func TestFindDockerCmdReadsWordsNotSpacing(t *testing.T) {
	for cmd, want := range map[string]CmdType{
		"docker  run --name app img":     DockerRun,
		"docker\trun --name app img":     DockerRun,
		"  sudo   docker   compose  up ": DockerCompose,
		"podman\n run app":               DockerRun,
		"docker  start -a app":           DockerStart,
		"python app.py":                  Native,
	} {
		if got := FindDockerCmd(cmd); got != want {
			t.Errorf("FindDockerCmd(%q) = %q, want %q", cmd, got, want)
		}
	}
}

// keploy's flags for the container go right after its engine's run
// subcommand, wherever that is.
func TestRunSubcommandEnd(t *testing.T) {
	for cmd, want := range map[string]string{
		"docker run img":                            "docker run",
		"sudo docker run img":                       "sudo docker run",
		"docker container run img":                  "docker container run",
		"sudo -u docker docker run img":             "sudo -u docker docker run",
		"DOCKER_CONFIG=/srv/docker docker run img":  "DOCKER_CONFIG=/srv/docker docker run",
		"/run/current-system/sw/bin/docker run img": "/run/current-system/sw/bin/docker run",
		"docker pull img && docker run img":         "docker pull img && docker run",
		"podman\n  run img":                         "podman\n  run",
		`C:\Docker\docker.exe run img`:              `C:\Docker\docker.exe run`,
		"docker start -a app":                       "",
		"docker --log-level debug run img":          "",
		"./run.sh":                                  "",
	} {
		at := RunSubcommandEnd(cmd)
		got := ""
		if at >= 0 {
			got = cmd[:at]
		}
		if got != want {
			t.Errorf("RunSubcommandEnd(%q) ends after %q, want after %q", cmd, got, want)
		}
	}
}

// A release lookup that GitHub did not answer with a release is an error, not
// an empty release: a rate-limited answer decoded to a tag of "", which read
// as a newer version than the running one, and the update went ahead.
func TestGetGitHubRelease_RefusesAnythingButARelease(t *testing.T) {
	for name, answer := range map[string]func(http.ResponseWriter){
		"rate limited": func(w http.ResponseWriter) {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"message":"API rate limit exceeded"}`))
		},
		"no tag": func(w http.ResponseWriter) { _, _ = w.Write([]byte(`{"body":"notes"}`)) },
		"an error that looks like a release": func(w http.ResponseWriter) {
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(`{"tag_name":"v9.9.9"}`))
		},
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { answer(w) }))
			defer srv.Close()
			if got, err := getGitHubRelease(context.Background(), zap.NewNop(), srv.URL); err == nil {
				t.Errorf("getGitHubRelease = %+v; want an error", got)
			}
		})
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"tag_name":"v3.6.200","assets":[{"name":"keploy_linux_amd64.tar.gz","digest":"sha256:ab"}]}`))
	}))
	defer srv.Close()
	got, err := getGitHubRelease(context.Background(), zap.NewNop(), srv.URL)
	if err != nil || got.TagName != "v3.6.200" || len(got.Assets) != 1 || got.Assets[0].Digest != "sha256:ab" {
		t.Errorf("getGitHubRelease = %+v, %v; want the release with its asset digest", got, err)
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
			name:       "valid http URL",
			currentURL: "http://example.com/api/v1",
			ipAddress:  "192.168.1.100",
			want:       "http://192.168.1.100/api/v1",
			wantErr:    false,
		},
		{
			name:       "valid http URL with port",
			currentURL: "http://example.com:8080/api/v1",
			ipAddress:  "10.0.0.1",
			want:       "http://10.0.0.1:8080/api/v1",
			wantErr:    false,
		},
		{
			name:       "empty ip address",
			currentURL: "http://example.com/api/v1",
			ipAddress:  "",
			want:       "http://example.com/api/v1",
			wantErr:    true,
		},
		{
			name:       "invalid URL",
			currentURL: "://invalid-url",
			ipAddress:  "10.0.0.1",
			want:       "://invalid-url",
			wantErr:    true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ReplaceHost(tc.currentURL, tc.ipAddress)
			if (err != nil) != tc.wantErr {
				t.Fatalf("ReplaceHost(%q, %q) error = %v, wantErr = %v", tc.currentURL, tc.ipAddress, err, tc.wantErr)
			}
			if got != tc.want {
				t.Errorf("ReplaceHost(%q, %q) = %q, want %q", tc.currentURL, tc.ipAddress, got, tc.want)
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
			name:      "valid authority with hostname",
			authority: "localhost:50051",
			ipAddress: "127.0.0.1",
			want:      "127.0.0.1:50051",
			wantErr:   false,
		},
		{
			name:      "valid authority with ipv6",
			authority: "[::1]:50051",
			ipAddress: "10.0.0.1",
			want:      "10.0.0.1:50051",
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
			name:      "invalid authority format without port",
			authority: "localhost",
			ipAddress: "127.0.0.1",
			want:      "localhost",
			wantErr:   true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ReplaceGrpcHost(tc.authority, tc.ipAddress)
			if (err != nil) != tc.wantErr {
				t.Fatalf("ReplaceGrpcHost(%q, %q) error = %v, wantErr = %v", tc.authority, tc.ipAddress, err, tc.wantErr)
			}
			if got != tc.want {
				t.Errorf("ReplaceGrpcHost(%q, %q) = %q, want %q", tc.authority, tc.ipAddress, got, tc.want)
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
			name:      "authority with port",
			authority: "localhost:50051",
			port:      "9090",
			want:      "localhost:9090",
			wantErr:   false,
		},
		{
			name:      "authority without port fallback",
			authority: "localhost",
			port:      "9090",
			want:      "localhost:9090",
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
				t.Fatalf("ReplaceGrpcPort(%q, %q) error = %v, wantErr = %v", tc.authority, tc.port, err, tc.wantErr)
			}
			if got != tc.want {
				t.Errorf("ReplaceGrpcPort(%q, %q) = %q, want %q", tc.authority, tc.port, got, tc.want)
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
			name:       "valid baseURL replacement",
			currentURL: "http://old-domain.com:8080/api/v1/resource?id=123",
			baseURL:    "https://new-domain.com:9000",
			want:       "https://new-domain.com:9000/api/v1/resource?id=123",
			wantErr:    false,
		},
		{
			name:       "empty baseURL",
			currentURL: "http://old-domain.com/api",
			baseURL:    "",
			want:       "http://old-domain.com/api",
			wantErr:    true,
		},
		{
			name:       "invalid currentURL",
			currentURL: "://invalid-url",
			baseURL:    "http://new-domain.com",
			want:       "://invalid-url",
			wantErr:    true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ReplaceBaseURL(tc.currentURL, tc.baseURL)
			if (err != nil) != tc.wantErr {
				t.Fatalf("ReplaceBaseURL(%q, %q) error = %v, wantErr = %v", tc.currentURL, tc.baseURL, err, tc.wantErr)
			}
			if got != tc.want {
				t.Errorf("ReplaceBaseURL(%q, %q) = %q, want %q", tc.currentURL, tc.baseURL, got, tc.want)
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
			name:       "URL without port",
			currentURL: "http://example.com/api",
			port:       "8080",
			want:       "http://example.com:8080/api",
			wantErr:    false,
		},
		{
			name:       "URL with existing port",
			currentURL: "http://example.com:8080/api",
			port:       "9090",
			want:       "http://example.com:9090/api",
			wantErr:    false,
		},
		{
			name:       "empty port",
			currentURL: "http://example.com/api",
			port:       "",
			want:       "http://example.com/api",
			wantErr:    true,
		},
		{
			name:       "invalid URL",
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
				t.Fatalf("ReplacePort(%q, %q) error = %v, wantErr = %v", tc.currentURL, tc.port, err, tc.wantErr)
			}
			if got != tc.want {
				t.Errorf("ReplacePort(%q, %q) = %q, want %q", tc.currentURL, tc.port, got, tc.want)
			}
		})
	}
}

func TestKeys(t *testing.T) {
	m := map[string][]string{
		"header-a": {"val1", "val2"},
		"header-b": {"val3"},
		"header-c": {},
	}
	got := Keys(m)
	sort.Strings(got)
	want := []string{"header-a", "header-b", "header-c"}
	if len(got) != len(want) {
		t.Fatalf("Keys() returned %d keys, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Keys()[%d] = %q, want %q", i, got[i], want[i])
		}
	}

	emptyGot := Keys(nil)
	if len(emptyGot) != 0 {
		t.Errorf("Keys(nil) = %v, want empty slice", emptyGot)
	}
}

func TestEnsureRmBeforeName(t *testing.T) {
	cases := []struct {
		name string
		cmd  string
		want string
	}{
		{
			name: "adds --rm before --name when absent",
			cmd:  "docker run -d --name my-app nginx",
			want: "docker run -d --rm --name my-app nginx",
		},
		{
			name: "preserves command when --rm already present",
			cmd:  "docker run -d --rm --name my-app nginx",
			want: "docker run -d --rm --name my-app nginx",
		},
		{
			name: "does nothing when --name is not present",
			cmd:  "docker run -d nginx",
			want: "docker run -d nginx",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := EnsureRmBeforeName(tc.cmd); got != tc.want {
				t.Errorf("EnsureRmBeforeName(%q) = %q, want %q", tc.cmd, got, tc.want)
			}
		})
	}
}

func TestIsDockerCmdAndHasUserCommand(t *testing.T) {
	cases := []struct {
		kind       CmdType
		isDocker   bool
		hasUserCmd bool
	}{
		{DockerRun, true, true},
		{DockerStart, true, true},
		{DockerCompose, true, true},
		{FromContainer, true, false},
		{Native, false, true},
		{Empty, false, false},
	}

	for _, tc := range cases {
		t.Run(string(tc.kind), func(t *testing.T) {
			if got := IsDockerCmd(tc.kind); got != tc.isDocker {
				t.Errorf("IsDockerCmd(%q) = %v, want %v", tc.kind, got, tc.isDocker)
			}
			if got := HasUserCommand(tc.kind); got != tc.hasUserCmd {
				t.Errorf("HasUserCommand(%q) = %v, want %v", tc.kind, got, tc.hasUserCmd)
			}
		})
	}
}

func TestHash(t *testing.T) {
	emptyHash := Hash([]byte(""))
	wantEmpty := "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	if emptyHash != wantEmpty {
		t.Errorf("Hash(\"\") = %q, want %q", emptyHash, wantEmpty)
	}

	dataHash := Hash([]byte("keploy"))
	if dataHash == emptyHash || len(dataHash) != 64 {
		t.Errorf("Hash(\"keploy\") = %q, want valid 64-char sha256 string", dataHash)
	}
}

func TestTrimSpaces(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "trims spaces around equal and comma",
			input: "foo = bar , baz = qux",
			want:  "foo=bar,baz=qux",
		},
		{
			name:  "respects escaped comma and equal",
			input: "foo\\,bar = 1",
			want:  "foo\\,bar=1",
		},
		{
			name:  "handles already trimmed string",
			input: "a=1,b=2",
			want:  "a=1,b=2",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := TrimSpaces(tc.input); got != tc.want {
				t.Errorf("TrimSpaces(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

func TestParseMetadata(t *testing.T) {
	t.Run("empty string returns nil", func(t *testing.T) {
		m, err := ParseMetadata("")
		if err != nil || m != nil {
			t.Fatalf("ParseMetadata(\"\") = %v, %v; want nil, nil", m, err)
		}
	})

	t.Run("valid key-value string parses into map", func(t *testing.T) {
		m, err := ParseMetadata("name=keploy,env=test")
		if err != nil {
			t.Fatalf("ParseMetadata failed: %v", err)
		}
		if m["name"] != "keploy" || m["env"] != "test" {
			t.Errorf("ParseMetadata returned unexpected map: %v", m)
		}
	})
}

func TestExtractFlagsFromArgs(t *testing.T) {
	t.Run("ExtractCmdTypeFromArgs", func(t *testing.T) {
		cases := []struct {
			args []string
			want string
		}{
			{[]string{"keploy", "record", "--cmd-type", "Docker"}, "docker"},
			{[]string{"keploy", "record", "--cmdType=DOCKER"}, "docker"},
			{[]string{"keploy", "record", "--cmd-type", "Native"}, "native"},
			{[]string{"keploy", "record"}, ""},
			{[]string{"keploy", "record", "--", "--cmd-type", "Docker"}, ""},
		}
		for _, tc := range cases {
			if got := ExtractCmdTypeFromArgs(tc.args); got != tc.want {
				t.Errorf("ExtractCmdTypeFromArgs(%v) = %q, want %q", tc.args, got, tc.want)
			}
		}
	})

	t.Run("ExtractFromContainerFromArgs", func(t *testing.T) {
		cases := []struct {
			args []string
			want string
		}{
			{[]string{"keploy", "record", "--from-container", "MyContainer"}, "MyContainer"},
			{[]string{"keploy", "record", "--fromContainer=App1"}, "App1"},
			{[]string{"keploy", "record"}, ""},
			{[]string{"keploy", "record", "--", "--from-container", "App1"}, ""},
		}
		for _, tc := range cases {
			if got := ExtractFromContainerFromArgs(tc.args); got != tc.want {
				t.Errorf("ExtractFromContainerFromArgs(%v) = %q, want %q", tc.args, got, tc.want)
			}
		}
	})

	t.Run("ExtractCommandFromArgs", func(t *testing.T) {
		cases := []struct {
			args []string
			want string
		}{
			{[]string{"keploy", "record", "-c", "python app.py"}, "python app.py"},
			{[]string{"keploy", "record", "--command", "go run ."}, "go run ."},
			{[]string{"keploy", "record", "-c=npm start"}, "npm start"},
			{[]string{"keploy", "record", "--command=cargo run"}, "cargo run"},
			{[]string{"keploy", "record"}, ""},
		}
		for _, tc := range cases {
			if got := ExtractCommandFromArgs(tc.args); got != tc.want {
				t.Errorf("ExtractCommandFromArgs(%v) = %q, want %q", tc.args, got, tc.want)
			}
		}
	})
}

func TestIsXMLResponse(t *testing.T) {
	cases := []struct {
		name string
		resp *models.HTTPResp
		want bool
	}{
		{
			name: "nil response",
			resp: nil,
			want: false,
		},
		{
			name: "response with no headers",
			resp: &models.HTTPResp{},
			want: false,
		},
		{
			name: "response with application/xml",
			resp: &models.HTTPResp{
				Header: map[string]string{"Content-Type": "application/xml; charset=utf-8"},
			},
			want: true,
		},
		{
			name: "response with text/xml",
			resp: &models.HTTPResp{
				Header: map[string]string{"Content-Type": "text/xml"},
			},
			want: true,
		},
		{
			name: "response with application/json",
			resp: &models.HTTPResp{
				Header: map[string]string{"Content-Type": "application/json"},
			},
			want: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsXMLResponse(tc.resp); got != tc.want {
				t.Errorf("IsXMLResponse() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestGetReqMeta(t *testing.T) {
	req, err := http.NewRequest("GET", "http://example.com/api/v1/users?page=1&sort=desc", nil)
	if err != nil {
		t.Fatalf("http.NewRequest failed: %v", err)
	}

	meta := GetReqMeta(req)
	if meta["method"] != "GET" {
		t.Errorf("meta[method] = %q, want 'GET'", meta["method"])
	}
	if meta["host"] != "example.com" {
		t.Errorf("meta[host] = %q, want 'example.com'", meta["host"])
	}
	if meta["path"] != "/api/v1/users" {
		t.Errorf("meta[path] = %q, want '/api/v1/users'", meta["path"])
	}
	queryKeys := meta["query keys"]
	if !strings.Contains(queryKeys, "page") || !strings.Contains(queryKeys, "sort") {
		t.Errorf("meta[query keys] = %q, want query parameter names 'page' and 'sort'", queryKeys)
	}
	if strings.Contains(queryKeys, "desc") || strings.Contains(queryKeys, "1") {
		t.Errorf("meta[query keys] = %q leaked query values", queryKeys)
	}
}

