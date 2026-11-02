//go:build linux

package proxy

import (
	"context"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"unsafe"

	"go.uber.org/zap"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/agent/proxy/fakeconn"
	"go.keploy.io/server/v3/pkg/agent/proxy/supervisor"
	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.keploy.io/server/v3/pkg/models"
)

// exitedChild runs a process to its end and returns its pid: reaped (gone
// from /proc) when reap is set, a zombie (state Z) otherwise, which the row's
// cleanup reaps.
func exitedChild(t *testing.T, reap bool) uint32 {
	t.Helper()
	cmd := exec.Command("true")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	if reap {
		if err := cmd.Wait(); err != nil {
			t.Fatal(err)
		}
		return uint32(pid)
	}
	t.Cleanup(func() { _ = cmd.Wait() })
	// Wait for its exit without reaping it (WNOWAIT): it stays a zombie.
	var info [128]byte
	if _, _, errno := syscall.Syscall6(syscall.SYS_WAITID, 1 /* P_PID */, uintptr(pid), uintptr(unsafe.Pointer(&info)), syscall.WEXITED|0x01000000 /* WNOWAIT */, 0, 0); errno != 0 {
		t.Fatalf("waitid: %v", errno)
	}
	return uint32(pid)
}

// The connection's first end is the application's stop, which records no
// fault (Session.EndFollowedStop), when the recording's stop had begun, or
// when it is the client's end and the client's process is exiting, gone, or
// unreadable; the client's end while its process runs on is the client's own
// giving up, and the upstream's end before the stop is the upstream's, however
// the client's process stands.
func TestPeerEndMarkerTellsTheApplicationsStopFromItsGivingUp(t *testing.T) {
	live := uint32(os.Getpid())
	gone := exitedChild(t, true)
	zombie := exitedChild(t, false)
	for _, tc := range []struct {
		name      string
		stopBegun bool
		clientPid uint32
		first     fakeconn.Direction
		wantStop  bool
	}{
		{"the client gives up while its process runs on", false, live, fakeconn.FromClient, false},
		{"the client's end after the stop began", true, live, fakeconn.FromClient, true},
		{"the upstream's end after the stop began", true, live, fakeconn.FromDest, true},
		{"the client's process has exited and been reaped", false, gone, fakeconn.FromClient, true},
		{"the client's process has exited, not yet reaped", false, zombie, fakeconn.FromClient, true},
		{"the client's process is not known to the agent", false, 0, fakeconn.FromClient, true},
		{"the upstream ends first, the client's process gone", false, gone, fakeconn.FromDest, false},
		{"the upstream ends first, the client's process unknown", false, 0, fakeconn.FromDest, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &Proxy{}
			if tc.stopBegun {
				p.NoteStopBegun()
			}
			sess := &supervisor.Session{}
			mark := p.peerEndMarker(sess, tc.clientPid)
			mark(tc.first, false)
			mark(1-tc.first, false)
			if got := sess.EndFollowedStop(); got != tc.wantStop {
				t.Fatalf("EndFollowedStop = %v, want %v", got, tc.wantStop)
			}
			if dir, ok := sess.FirstPeerEnded(); !ok || dir != tc.first {
				t.Fatalf("first end = %d, %v; want %d", dir, ok, tc.first)
			}
		})
	}
}

// The recording's stop has begun once keploy mock record's client says so
// (NoteStopBegun) or keploy record's graceful-shutdown notice arrives, and a
// new recording or mocking session starts with it not begun.
func TestStopBegunIsSetByEitherNoticeAndClearedByTheNextSession(t *testing.T) {
	t.Cleanup(func() { syncMock.Get().SetOutputChannel(nil) })
	cfg := config.New()
	cfg.Agent.Mode = models.MODE_RECORD
	p := New(zap.NewNop(), fakeDestInfo{}, cfg)
	p.nsswitchData = []byte("preseeded-by-test") // Mock leaves the host's DNS alone
	if p.StopBegun() {
		t.Fatal("a new proxy has its stop begun")
	}
	if err := p.SetGracefulShutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !p.StopBegun() {
		t.Fatal("keploy record's graceful-shutdown notice did not begin the stop")
	}
	if err := p.Record(context.Background(), make(chan *models.Mock, 16), models.OutgoingOptions{DisableMysqlAutoDetect: true}); err != nil {
		t.Fatal(err)
	}
	if p.StopBegun() {
		t.Fatal("a new recording started with the last one's stop begun")
	}
	p.NoteStopBegun()
	if !p.StopBegun() {
		t.Fatal("keploy mock record's stop notice did not begin the stop")
	}
	if p.IsGracefulShutdown() {
		t.Fatal("keploy mock record's stop notice set the graceful-shutdown flag, whose effects belong after its drain")
	}
	if err := p.Mock(context.Background(), models.OutgoingOptions{Mocking: true}); err != nil {
		t.Fatal(err)
	}
	if p.StopBegun() {
		t.Fatal("a new mocking session started with the last recording's stop begun")
	}
}
