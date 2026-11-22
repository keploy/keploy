package routes

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// testChild is this test binary run again, as a child process, for one test.
type testChild struct {
	cmd  *exec.Cmd
	out  bytes.Buffer
	done chan struct{}
	err  error
}

// startTestChild runs this test binary again for the one test name, with env
// added to this process's environment and args after its -test flags. The
// child leaves nothing behind however its test ends:
//   - It has a process group of its own. t.Cleanup kills that group (the child
//     and whatever it started) if the child is still running, and waits for it.
//   - Its stdin is a pipe whose other end only this process holds, and
//     TestMain has it end when that pipe closes (endWithTheTest), which is when
//     this process ends, even by a kill or a -test.timeout panic, which run no
//     t.Cleanup.
func startTestChild(t *testing.T, name string, env []string, args ...string) *testChild {
	t.Helper()
	c := &testChild{done: make(chan struct{})}
	c.cmd = exec.Command(os.Args[0], append([]string{"-test.run=^" + name + "$", "-test.count=1", "-test.v"}, args...)...)
	c.cmd.Env = append(append(os.Environ(), testChildEnv+"=1"), env...)
	c.cmd.Stdout, c.cmd.Stderr = &c.out, &c.out
	stdin, held, err := os.Pipe()
	if err != nil {
		t.Fatalf("the child's stdin: %v", err)
	}
	c.cmd.Stdin = stdin
	ownProcessGroup(c.cmd)
	if err := c.cmd.Start(); err != nil {
		_ = stdin.Close()
		_ = held.Close()
		t.Fatalf("start the child for %s: %v", name, err)
	}
	_ = stdin.Close()
	go func() {
		c.err = c.cmd.Wait()
		_ = held.Close()
		close(c.done)
	}()
	t.Cleanup(func() {
		select {
		case <-c.done:
			return
		default:
		}
		killProcessGroup(c.cmd)
		select {
		case <-c.done:
		case <-time.After(30 * time.Second):
			t.Errorf("the child for %s (pid %d) was still running 30s after its process group was killed", name, c.cmd.Process.Pid)
		}
	})
	return c
}

// wait waits for the child to end and returns its combined output and how it
// ended.
func (c *testChild) wait() ([]byte, error) {
	<-c.done
	return c.out.Bytes(), c.err
}

// runTestChild runs the child startTestChild starts and waits for it.
func runTestChild(t *testing.T, name string, env []string, args ...string) ([]byte, error) {
	t.Helper()
	return startTestChild(t, name, env, args...).wait()
}

// endWithTheTest ends this process, a child startTestChild started, when its
// stdin closes: the test's process holds the pipe's other end, so it closes
// when that process ends, however it ends. TestMain calls it in every such
// child (testChildEnv).
func endWithTheTest() {
	go func() {
		_, _ = io.Copy(io.Discard, os.Stdin)
		fmt.Fprintln(os.Stderr, "the test that started this child ended, so the child ends")
		os.Exit(3)
	}()
}

const (
	// testChildEnv is 1 in every child startTestChild starts.
	testChildEnv = "KEPLOY_TEST_CHILD"
	// bootChildEnv, set to 1, makes a boot row's child run the row's start.
	bootChildEnv     = "KEPLOY_TEST_BOOT_CHILD"
	childRoleEnv     = "KEPLOY_TEST_CHILD_ROLE"
	childAddrFileEnv = "KEPLOY_TEST_CHILD_ADDR_FILE"
)

// holdAPort listens on loopback, as the boot rows' children hold the agent's
// port, writes the address to the file childAddrFileEnv names and answers
// every connection by closing it, for at most two minutes, so a row that
// fails leaves no process behind for good.
func holdAPort(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	file := os.Getenv(childAddrFileEnv)
	if err := os.WriteFile(file+".tmp", []byte(ln.Addr().String()), 0o600); err != nil {
		t.Fatalf("write the address: %v", err)
	}
	if err := os.Rename(file+".tmp", file); err != nil {
		t.Fatalf("write the address: %v", err)
	}
	time.Sleep(2 * time.Minute)
}

// heldAddr waits for the address a child's holdAPort writes to file, failing
// the row if the child ends first.
func heldAddr(t *testing.T, file string, c *testChild) string {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		if b, err := os.ReadFile(file); err == nil {
			return string(b)
		}
		select {
		case <-c.done:
			out, err := c.wait()
			t.Fatalf("the child ended (%v) before a port was held:\n%s", err, out)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("no port was held within 30s")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// portLetGo waits until nothing accepts a connection at addr.
func portLetGo(t *testing.T, addr, what string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		c, err := net.DialTimeout("tcp", addr, time.Second)
		if err != nil {
			return
		}
		_ = c.Close()
		if time.Now().After(deadline) {
			t.Fatalf("%s: %s is still held 15s later", what, addr)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// A test killed while its child runs (a kill or a -test.timeout panic, which
// run no t.Cleanup) leaves no child behind: the child, holding a port as the
// boot rows' children hold the agent's, ends when its stdin closes with the
// killed test's process, and lets the port go.
func TestAChildOfAKilledTestEnds(t *testing.T) {
	const name = "TestAChildOfAKilledTestEnds"
	switch os.Getenv(childRoleEnv) {
	case "":
		file := filepath.Join(t.TempDir(), "addr")
		killed := startTestChild(t, name, []string{childRoleEnv + "=killed", childAddrFileEnv + "=" + file}, "-test.timeout=2m")
		addr := heldAddr(t, file, killed)
		if err := killed.cmd.Process.Kill(); err != nil {
			t.Fatalf("kill the test: %v", err)
		}
		if out, err := killed.wait(); err == nil {
			t.Fatalf("the test was not killed:\n%s", out)
		}
		portLetGo(t, addr, "the child of a killed test still runs")
	case "killed":
		startTestChild(t, name, []string{childRoleEnv + "=held"}, "-test.timeout=2m")
		time.Sleep(2 * time.Minute)
	case "held":
		holdAPort(t)
	}
}
