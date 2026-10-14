//go:build linux

package linux

import (
	"context"
	"os"
	"strings"
	"testing"

	"go.uber.org/zap"
	"golang.org/x/sys/unix"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

func unixEvent(kind uint32, p string, ino uint64) unixConnectEvent {
	e := unixConnectEvent{Kind: kind, PeerIno: ino}
	b := []byte(p)
	if len(p) > 0 && p[0] == '@' {
		b = append([]byte{0}, p[1:]...)
	}
	copy(e.Path[:], b)
	e.PathLen = uint32(len(b))
	return e
}

// The allowlist: buses, the journal, the Docker daemon and the name services
// glibc asks on a host lookup; nothing else.
func TestUnixAllowlist(t *testing.T) {
	for _, p := range []string{
		"/run/dbus/system_bus_socket", "/run/user/1000/bus", "@/tmp/dbus-AbCdEf",
		"/tmp/.X11-unix/X0", "@/tmp/.X11-unix/X1", "/run/user/1000/wayland-0",
		"/run/systemd/journal/socket", "/run/systemd/journal/stdout", "/dev/log",
		"/var/run/docker.sock", "/run/user/1000/docker.sock",
		"/var/run/nscd/socket", "/var/lib/sss/pipes/nss", "/run/systemd/resolve/io.systemd.Resolve",
		"/run/avahi-daemon/socket", "/run/systemd/userdb/io.systemd.Home",
	} {
		if !unixAllowed(p) {
			t.Errorf("%s is not allowed, want it allowed", p)
		}
	}
	for _, p := range []string{
		"/var/run/postgresql/.s.PGSQL.5432", "/var/run/mysqld/mysqld.sock", "/tmp/redis.sock",
		"@keploy-test", "/run/user/1000/bus/x", "", "/var/run/docker.sock.bak",
	} {
		if unixAllowed(p) {
			t.Errorf("%q is allowed, want it reported", p)
		}
	}
}

// A report is counted unless allowlisted, or a datagram peer a member holds;
// an abstract name reads as "@name"; the lost reports add to the count.
func TestUnixConnectJudge(t *testing.T) {
	u := &unixConn{known: map[uint64]bool{}}
	none := func(uint32) bool { return false }
	if u.judge(unixEvent(1, "/run/systemd/journal/socket", 0), none) {
		t.Fatal("an allowlisted socket was counted")
	}
	if !u.judge(unixEvent(1, "/var/run/postgresql/.s.PGSQL.5432", 0), none) {
		t.Fatal("libpq's socket was not counted")
	}
	if !u.judge(unixEvent(1, "@abstract-dep", 0), none) {
		t.Fatal("an abstract name was not counted")
	}

	// A datagram peer this process holds: owned by a member when this process
	// is one, not otherwise.
	fd, err := unix.Socket(unix.AF_UNIX, unix.SOCK_DGRAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		t.Fatal(err)
	}
	self := uint32(os.Getpid())
	isSelf := func(tgid uint32) bool { return tgid == self }
	if u.judge(unixEvent(unixConnectDgram, "/tmp/app.dgram", st.Ino), isSelf) {
		t.Fatalf("a datagram socket a member (pid %d) holds was counted", self)
	}
	u2 := &unixConn{known: map[uint64]bool{}}
	if !u2.judge(unixEvent(unixConnectDgram, "/tmp/other.dgram", st.Ino), none) {
		t.Fatal("a datagram socket no member holds was not counted")
	}

	got := u.snapshot()
	if len(got) != 1 || got[0].Kind != models.CaptureLossUnixSocket || got[0].Count != 2 || got[0].What != "@abstract-dep" {
		t.Fatalf("snapshot %+v, want 2 unix_socket losses, the latest @abstract-dep", got)
	}
}

// With the proven switch off nothing loads, attaches or warns: nothing reads
// the report without it. (Loading needs root and keploy's objects: a load
// tried here fails, and the report would be left off, not absent.)
func TestTheUnixReportStaysUnloadedWithoutTheSwitch(t *testing.T) {
	t.Setenv(provenswitch.Env, "0")
	h := &Hooks{logger: zap.NewNop()}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.startUnixConn(ctx)
	if h.unix != nil {
		t.Fatalf("with the switch off the report was started: %+v", h.unix)
	}
	if err := h.CaptureLossesErr(); err != nil {
		t.Fatalf("with the switch off, CaptureLossesErr = %v, want nil", err)
	}
}

// A report that could not load under the switch is unknown, never 0.
func TestAUnixReportThatCouldNotLoadIsAnError(t *testing.T) {
	h := &Hooks{logger: zap.NewNop(), unix: &unixConn{off: "no trampoline"}}
	err := h.CaptureLossesErr()
	if err == nil || !strings.Contains(err.Error(), "no trampoline") {
		t.Fatalf("CaptureLossesErr = %v, want the reason it could not load", err)
	}
	if got := h.unixLosses(); len(got) != 0 {
		t.Fatalf("an unloaded report counted %+v", got)
	}
}
