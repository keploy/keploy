//go:build linux

package linux

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// sysctlDir writes a net/ipv4 sysctl directory for readBindMovePorts: the
// two files as /proc/sys gives them, each one left out when its content is
// "-".
func sysctlDir(t *testing.T, portRange, unpriv string) string {
	t.Helper()
	dir := t.TempDir()
	for name, v := range map[string]string{"ip_local_port_range": portRange, "ip_unprivileged_port_start": unpriv} {
		if v == "-" {
			continue
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(v+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// The ports keploy/ebpf's find_free_port moves a bind to, from the
// network namespace's two sysctls: above the ephemeral range when it ends
// below 65535, else below it from the higher of 1024 and the unprivileged
// start, else none.
func TestTheBindMovePortsAreFindFreePortsChoice(t *testing.T) {
	rows := []struct {
		name, portRange, unpriv string
		first, last             int
	}{
		{"the default range: above it", "32768\t60999", "1024", 61000, 65535},
		{"one port above the range", "32768\t65534", "1024", 65535, 65535},
		{"a range to 65535: below it, from 1024", "40000\t65535", "1024", 1024, 39999},
		{"a range to 65535 with the unprivileged ports from 80: from 1024", "1025\t65535", "80", 1024, 1024},
		{"a range to 65535 with the unprivileged ports from 0: from 1024", "1025\t65535", "0", 1024, 1024},
		{"a range to 65535 with the unprivileged ports from 20000: from 20000", "40000\t65535", "20000", 20000, 39999},
		{"a range to 65535 with the unprivileged ports from 1025: from 1025", "1026\t65535", "1025", 1025, 1025},
		{"no room from the range", "1024\t65535", "1024", 0, 0},
		{"no room from the range with the unprivileged ports from 80", "1024\t65535", "80", 0, 0},
		{"no room from a raised unprivileged start", "20000\t65535", "20000", 0, 0},
		{"the unprivileged start does not move the ports above the range", "20000\t60999", "20000", 61000, 65535},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			p, err := readBindMovePorts(sysctlDir(t, r.portRange, r.unpriv))
			if err != nil {
				t.Fatal(err)
			}
			if p.first != r.first || p.last != r.last {
				t.Fatalf("range %q, unprivileged from %s: the binds move to %d-%d, want %d-%d", r.portRange, r.unpriv, p.first, p.last, r.first, r.last)
			}
		})
	}
}

// A sysctl that cannot be read, or reads as no port, is an error: the
// Warn then fails closed.
func TestTheBindMovePortsFailOnAnUnreadableSysctl(t *testing.T) {
	rows := []struct{ name, portRange, unpriv, want string }{
		{"no range", "-", "1024", "read net.ipv4.ip_local_port_range"},
		{"no unprivileged start", "32768\t60999", "-", "read net.ipv4.ip_unprivileged_port_start"},
		{"one port in the range", "32768", "1024", "want 2 port(s)"},
		{"three ports in the range", "1 2 3", "1024", "want 2 port(s)"},
		{"two unprivileged starts", "32768\t60999", "1024 1025", "want 1 port(s)"},
		{"no unprivileged start in the file", "32768\t60999", "", "want 1 port(s)"},
		{"a word for a port", "32768\tmany", "1024", `"many" is no port`},
		{"a port above 65535", "32768\t65536", "1024", `"65536" is no port`},
		{"a negative port", "-1\t60999", "1024", `"-1" is no port`},
		{"a negative unprivileged start", "32768\t60999", "-5", `"-5" is no port`},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			p, err := readBindMovePorts(sysctlDir(t, r.portRange, r.unpriv))
			if err == nil || !strings.Contains(err.Error(), r.want) {
				t.Fatalf("range %q, unprivileged %q: got %+v, %v; want an error with %q", r.portRange, r.unpriv, p, err, r.want)
			}
		})
	}
	// The ports at each end of what a sysctl may hold are read.
	p, err := readBindMovePorts(sysctlDir(t, "0\t65535", "0"))
	if err != nil || p.lo != 0 || p.hi != 65535 || p.unpriv != 0 {
		t.Fatalf("range 0-65535, unprivileged from 0: got %+v, %v", p, err)
	}
}

// warnBindFallback says once where the app's binds go when a post_bind
// hook did not attach: where they move, and that one which finds each port
// it tries there listened on is left alone and not recorded; or, with no
// room or a sysctl it cannot read, that they are left on the app's ports and
// those are not recorded.
func TestTheBindFallbackWarnSaysWhereTheBindsGo(t *testing.T) {
	attachErr := errors.New("operation not supported")
	rows := []struct {
		name, portRange, unpriv string
		want, not               []string
	}{
		{"room", "32768\t60999", "1024",
			[]string{"the post_bind4 cgroup hook did not attach", "a port in 61000-65535, outside net.ipv4.ip_local_port_range 32768-60999",
				"A bind that finds each port it tries there listened on is left on the port the app names, and that port's incoming traffic is not recorded"},
			[]string{"no port is left", "each bind is left", "cannot say"}},
		{"no room from the range", "1024\t65535", "1024",
			[]string{"no port is left outside net.ipv4.ip_local_port_range 1024-65535 from 1024 up", "net.ipv4.ip_unprivileged_port_start 1024)",
				"each bind is left on the port the app names", "incoming traffic is not recorded"},
			[]string{"a port in"}},
		{"no room from the range with the unprivileged ports from 80", "1024\t65535", "80",
			[]string{"no port is left outside net.ipv4.ip_local_port_range 1024-65535 from 1024 up", "net.ipv4.ip_unprivileged_port_start 80)",
				"incoming traffic is not recorded"},
			[]string{"a port in"}},
		{"no room from a raised unprivileged start", "20000\t65535", "20000",
			[]string{"no port is left outside net.ipv4.ip_local_port_range 20000-65535 from 20000 up", "net.ipv4.ip_unprivileged_port_start 20000)",
				"incoming traffic is not recorded"},
			[]string{"a port in"}},
		{"an unreadable sysctl", "32768\t60999", "-",
			[]string{"cannot say whether any port is left", "read net.ipv4.ip_unprivileged_port_start", "as not recorded"},
			[]string{"a port in"}},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			core, logs := observer.New(zapcore.WarnLevel)
			warnBindFallback(zap.New(core), "post_bind4", attachErr, sysctlDir(t, r.portRange, r.unpriv))
			all := logs.All()
			if len(all) != 1 {
				t.Fatalf("%d warnings, want 1: %v", len(all), all)
			}
			msg := all[0].Message
			for _, w := range r.want {
				if !strings.Contains(msg, w) {
					t.Errorf("the warning %q does not say %q", msg, w)
				}
			}
			for _, n := range r.not {
				if strings.Contains(msg, n) {
					t.Errorf("the warning %q says %q", msg, n)
				}
			}
			if got := all[0].ContextMap()["error"]; got != attachErr.Error() {
				t.Errorf("the warning's error is %v, want the attach's, %q", got, attachErr)
			}
		})
	}
}
