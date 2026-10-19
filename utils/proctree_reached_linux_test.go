//go:build linux

package utils

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"testing/fstest"
	"time"

	"go.uber.org/zap"
)

// statusWith is a /proc/<pid>/status whose SigIgn mask is sigIgn.
func statusWith(sigIgn string) *fstest.MapFile {
	return &fstest.MapFile{Data: []byte("Name:\tapp\nState:\tS (sleeping)\nSigQ:\t0/63426\nSigPnd:\t0000000000000000\nShdPnd:\t0000000000000000\nSigBlk:\t0000000000000000\nSigIgn:\t" + sigIgn + "\nSigCgt:\t0000000180004002\n")}
}

// What a signal to a process group reaches is each process of that group,
// and whether it ignores the signal is its SigIgn bit, sig-1: a zombie, a
// process of another group, and one whose status cannot be read are not
// among them.
func TestGroupReachedIsTheGroupAndWhatItIgnores(t *testing.T) {
	proc := fstest.MapFS{
		"600/stat":   statLine(600, "sh", 'S', 500, 600),
		"600/status": statusWith("0000000000000000"),
		"601/stat":   statLine(601, "app", 'S', 600, 600),
		"601/status": statusWith("0000000000000002"), // SIGINT (2) ignored
		"602/stat":   statLine(602, "worker", 'Z', 601, 600),
		"602/status": statusWith("0000000000000000"),
		"603/stat":   statLine(603, "gone", 'S', 600, 600), // its status is gone
		"604/stat":   statLine(604, "term", 'S', 600, 600),
		"604/status": statusWith("0000000000004000"), // SIGTERM (15) ignored
		"605/stat":   statLine(605, "both", 'S', 600, 600),
		"605/status": statusWith("0000000000004002"),
		"700/stat":   statLine(700, "other", 'S', 1, 700),
		"700/status": statusWith("0000000000000000"),
		"self/stat":  statLine(500, "keploy", 'S', 1, 500),
	}
	got := map[int]bool{}
	for _, r := range groupReachedIn(proc, 600, syscall.SIGINT) {
		if r.Start != 77 {
			t.Errorf("process %d: start %d, want the stat's 77", r.PID, r.Start)
		}
		got[r.PID] = r.Ignores
	}
	want := map[int]bool{600: false, 601: true, 604: false, 605: true}
	if len(got) != len(want) {
		t.Fatalf("reached %v, want %v", got, want)
	}
	for pid, ignores := range want {
		if g, ok := got[pid]; !ok || g != ignores {
			t.Errorf("process %d: reached %v, ignoring SIGINT %v; want reached, ignoring %v", pid, ok, g, ignores)
		}
	}
	if r := groupReachedIn(proc, 600, syscall.SIGTERM); len(r) != 4 {
		t.Errorf("for SIGTERM, reached %+v, want the four", r)
	} else {
		for _, p := range r {
			if p.Ignores != (p.PID == 604 || p.PID == 605) {
				t.Errorf("for SIGTERM, process %d ignoring %v", p.PID, p.Ignores)
			}
		}
	}
}

// InterruptProcessTreeReached says which processes its signal reached: the
// whole group of the tree's leader, a member it did not start included, each
// named by its pid and start time, and whether it ignored the signal.
func TestInterruptProcessTreeReachedSaysWhatItReachedAndWhatIgnoredIt(t *testing.T) {
	leader := exec.Command("sleep", "600")
	leader.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} // as keploy starts a native command
	if err := leader.Start(); err != nil {
		t.Fatal(err)
	}
	reaped := make(chan struct{})
	go func() { _ = leader.Wait(); close(reaped) }()
	t.Cleanup(func() { _ = syscall.Kill(leader.Process.Pid, syscall.SIGKILL); <-reaped })
	member := exec.Command("sh", "-c", `trap "" INT; exec sleep 600`)
	member.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pgid: leader.Process.Pid}
	if err := member.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = member.Process.Kill(); _ = member.Wait() })
	// The member ignores SIGINT once it runs sleep.
	for deadline := time.Now().Add(time.Minute); ; time.Sleep(10 * time.Millisecond) {
		comm, _ := os.ReadFile("/proc/" + strconv.Itoa(member.Process.Pid) + "/comm")
		if strings.TrimSpace(string(comm)) == "sleep" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the member did not start sleep")
		}
	}
	startOf := func(pid int) uint64 {
		st, ok := ReadProcStat(pid)
		if !ok {
			t.Fatalf("process %d: no stat", pid)
		}
		return st.StartTime
	}
	want := map[Reached]bool{
		{PID: leader.Process.Pid, Start: startOf(leader.Process.Pid), Ignores: false}: true,
		{PID: member.Process.Pid, Start: startOf(member.Process.Pid), Ignores: true}:  true,
	}
	killed, reached, err := InterruptProcessTreeReached(zap.NewNop(), leader.Process.Pid, syscall.SIGINT)
	if err != nil {
		t.Fatal(err)
	}
	if killed != "" {
		t.Errorf("the leader exits on SIGINT, and the interrupt says it killed: %q", killed)
	}
	if len(reached) != len(want) {
		t.Fatalf("reached %+v, want %v", reached, want)
	}
	for _, r := range reached {
		if !want[r] {
			t.Errorf("reached %+v, want %v", r, want)
		}
	}
	<-reaped
	if st, ok := ReadProcStat(member.Process.Pid); !ok || st.State == 'Z' {
		t.Error("the member, which ignores SIGINT, did not outlive its leader")
	}
}
