// Command init is the first process of the kernel-floor VMs that
// .github/scripts/kernel-floors/boot.sh boots (.github/workflows/kernel-floors.yml).
//
// It mounts what keploy's agent finds on a host (proc, sysfs, devtmpfs, a tmpfs
// /tmp and /run, the cgroup2 hierarchy, bpffs, tracefs and debugfs), brings the
// loopback interface up, says which kernel it runs on, runs /kf/run.sh (the
// tests boot.sh put in the initramfs) and says how that ended, prints the
// kernel's log, and powers the VM off. Every line of its own starts with "KF ",
// and boot.sh's check reads them, and the tests' own lines, from the console:
// nothing here decides a verdict, and nothing waits on a clock.
//
// The standard library only, built static (CGO_ENABLED=0): the VM has no libc
// of its own until boot.sh's userland step copies one in, and this must run
// before anything else does.
package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"unsafe"
)

func say(format string, a ...any) {
	fmt.Printf("KF "+format+"\n", a...)
}

type mount struct {
	source, target, fstype string
	flags                  uintptr
	data                   string
}

const (
	nosuid = syscall.MS_NOSUID
	nodev  = syscall.MS_NODEV
	noexec = syscall.MS_NOEXEC
)

// mounts are mounted in order: /dev first, so that the console can be opened
// from it, and /sys before what lives under it.
var mounts = []mount{
	{"devtmpfs", "/dev", "devtmpfs", nosuid, "mode=0755"},
	{"proc", "/proc", "proc", nosuid | nodev | noexec, ""},
	{"sysfs", "/sys", "sysfs", nosuid | nodev | noexec, ""},
	{"tmpfs", "/dev/shm", "tmpfs", nosuid | nodev, "mode=1777"},
	{"tmpfs", "/tmp", "tmpfs", nosuid | nodev, "mode=1777"},
	{"tmpfs", "/run", "tmpfs", nosuid | nodev, "mode=0755"},
	{"cgroup2", "/sys/fs/cgroup", "cgroup2", nosuid | nodev | noexec, ""},
	{"bpf", "/sys/fs/bpf", "bpf", nosuid | nodev | noexec, "mode=0700"},
	{"tracefs", "/sys/kernel/tracing", "tracefs", nosuid | nodev | noexec, ""},
	{"debugfs", "/sys/kernel/debug", "debugfs", nosuid | nodev | noexec, ""},
}

func main() {
	if err := setup(); err != nil {
		say("setup failed: %v", err)
	} else {
		run()
	}
	dmesg()
	syscall.Sync()
	// The VM's end: boot.sh boots with -no-reboot, so a power-off (and, were
	// it refused, the panic of init returning, under panic=-1) ends QEMU.
	if err := syscall.Reboot(syscall.LINUX_REBOOT_CMD_POWER_OFF); err != nil {
		say("power off: %v", err)
	}
}

func setup() error {
	for i, m := range mounts {
		if err := os.MkdirAll(m.target, 0o755); err != nil {
			return fmt.Errorf("mkdir %s: %w", m.target, err)
		}
		if err := syscall.Mount(m.source, m.target, m.fstype, m.flags, m.data); err != nil {
			return fmt.Errorf("mount %s on %s: %w", m.fstype, m.target, err)
		}
		if i == 0 {
			console()
		}
	}
	if err := loopbackUp(); err != nil {
		return fmt.Errorf("bring lo up: %w", err)
	}
	_ = syscall.Sethostname([]byte("kernel-floor"))
	var u syscall.Utsname
	if err := syscall.Uname(&u); err != nil {
		return fmt.Errorf("uname: %w", err)
	}
	// The line the workflow's proof names: the guest's own kernel.
	say("kernel %s on %s", field(&u.Release), field(&u.Machine))
	say("version %s", field(&u.Version))
	if b, err := os.ReadFile("/proc/cmdline"); err == nil {
		say("cmdline %s", strings.TrimSpace(string(b)))
	}
	return nil
}

// console makes fds 0, 1 and 2 the console, whatever the kernel handed init:
// a kernel whose built-in initramfs has no /dev/console starts init with none.
func console() {
	f, err := os.OpenFile("/dev/console", os.O_RDWR, 0)
	if err != nil {
		return
	}
	for fd := 0; fd <= 2; fd++ {
		_ = syscall.Dup3(int(f.Fd()), fd, 0)
	}
	_ = f.Close()
}

// field is a utsname field as a string. Its element type is int8 on some
// architectures and uint8 on others; both are a byte.
func field[T any](f *T) string {
	b := unsafe.Slice((*byte)(unsafe.Pointer(f)), unsafe.Sizeof(*f))
	if i := bytes.IndexByte(b, 0); i >= 0 {
		b = b[:i]
	}
	return string(b)
}

// loopbackUp sets IFF_UP on lo, as a host's network setup does: the tests
// connect to and NAT 127.0.0.1.
func loopbackUp() error {
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_DGRAM|syscall.SOCK_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer syscall.Close(fd)
	// struct ifreq: the name, then a union whose first member here is the
	// flags (short).
	var ifr [40]byte
	copy(ifr[:], "lo")
	ioctl := func(req uintptr) error {
		if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), req, uintptr(unsafe.Pointer(&ifr[0]))); e != 0 {
			return e
		}
		return nil
	}
	if err := ioctl(syscall.SIOCGIFFLAGS); err != nil {
		return err
	}
	flags := *(*uint16)(unsafe.Pointer(&ifr[16])) | syscall.IFF_UP | syscall.IFF_RUNNING
	*(*uint16)(unsafe.Pointer(&ifr[16])) = flags
	return ioctl(syscall.SIOCSIFFLAGS)
}

// run runs /kf/run.sh to its end and says how it ended. As PID 1 it reaps
// every process that ends meanwhile, the orphans the tests leave as well as
// the script.
func run() {
	cmd := exec.Command("/bin/sh", "/kf/run.sh")
	cmd.Dir = "/tmp"
	cmd.Env = []string{
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"HOME=/root",
		"TMPDIR=/tmp",
		"USER=root",
	}
	cmd.Stdin = nil
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		say("run.sh did not start: %v", err)
		return
	}
	for {
		var ws syscall.WaitStatus
		pid, err := syscall.Wait4(-1, &ws, 0, nil)
		if err == syscall.EINTR {
			continue
		}
		if err != nil {
			say("wait for run.sh: %v", err)
			return
		}
		if pid != cmd.Process.Pid {
			continue
		}
		switch {
		case ws.Exited():
			say("run.sh exited %d", ws.ExitStatus())
		case ws.Signaled():
			say("run.sh killed by %v", ws.Signal())
		default:
			say("run.sh ended: status %#x", uint32(ws))
		}
		return
	}
}

// dmesg prints the kernel's log, every level of it: the VM boots with a quiet
// console, so that the kernel's lines do not break the tests' up, and a warning
// the kernel printed meanwhile is here.
func dmesg() {
	const (
		readAll    = 3
		sizeBuffer = 10
	)
	n, err := syscall.Klogctl(sizeBuffer, nil)
	if err != nil || n <= 0 {
		say("dmesg: %v", err)
		return
	}
	buf := make([]byte, n)
	n, err = syscall.Klogctl(readAll, buf)
	if err != nil {
		say("dmesg: %v", err)
		return
	}
	say("dmesg begins")
	for _, line := range strings.Split(strings.TrimRight(string(buf[:n]), "\n"), "\n") {
		say("dmesg %s", line)
	}
	say("dmesg ends")
}
