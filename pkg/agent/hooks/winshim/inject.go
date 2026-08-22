//go:build windows && amd64

package winshim

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"
	"unsafe"

	"go.uber.org/zap"
	"golang.org/x/sys/windows"
)

// This file is the Windows equivalent of DYLD_INSERT_LIBRARIES.
//
// macOS can load a shim into an application purely by setting an environment
// variable, so the client only has to rewrite the command line. Windows has no
// such mechanism that works without registry-wide, machine-wide configuration
// (AppInit_DLLs, which needs Administrator and affects every process on the
// system). The unprivileged equivalent is to start the process suspended, map
// the DLL into it, wait for it to arm, and only then let it run — which is what
// StartInstrumented does.
//
// Only the FIRST process needs this treatment. The shim hooks CreateProcessW/A,
// so it carries itself into every descendant on its own, exactly as dyld carries
// an inserted library down a process tree. That matters because the process
// keploy starts is `cmd /C <user command>`, and the application itself is that
// shell's child.

// armTimeout bounds how long the launcher waits for an injected process to
// report that its hooks are live. Bounded so a process that cannot arm delays
// the run instead of hanging it. Kept in sync with ARM_TIMEOUT_MS in
// shim/keploy_winshim.c.
const armTimeout = 5 * time.Second

// StartInstrumented starts cmd with the interception shim injected.
//
// It replaces cmd.Start() for the application under test. The process is created
// suspended so that the shim is in place before a single instruction of the
// application runs — otherwise an app that connects immediately at startup would
// race the injection and lose its first connections.
//
// Injection is best-effort in one direction only: if the DLL cannot be mapped,
// the process is still resumed and runs uninstrumented rather than being left
// suspended or killed. A missed recording is recoverable; a wedged application
// is not.
func StartInstrumented(logger *zap.Logger, cmd *exec.Cmd, dllPath string) error {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= windows.CREATE_SUSPENDED

	if err := cmd.Start(); err != nil {
		return err
	}
	pid := uint32(cmd.Process.Pid)

	// Whatever happens below, the process must end up running — or, if it truly
	// cannot be resumed, be killed.
	//
	// A process left suspended is the one outcome worse than an uninstrumented
	// one: it never runs, never exits, and keploy waits on it forever with
	// nothing to show the user. Killing it surfaces as an application that
	// failed to start, which is at least diagnosable.
	defer func() {
		if err := resumeProcess(pid); err == nil {
			return
		} else if retryErr := resumeProcess(pid); retryErr == nil {
			logger.Debug("resumed the application on the second attempt", zap.Uint32("pid", pid))
			return
		} else {
			logger.Error("could not resume the application after injecting the interception shim; killing it rather than leaving it suspended forever",
				zap.Uint32("pid", pid), zap.Error(retryErr))
		}
		if killErr := cmd.Process.Kill(); killErr != nil {
			logger.Error("failed to kill the suspended application; it may need to be ended manually",
				zap.Uint32("pid", pid), zap.Error(killErr))
		}
	}()

	if err := injectInto(pid, dllPath); err != nil {
		logger.Warn("could not inject the Keploy interception shim into the application; it will run without interception, so no traffic will be recorded or mocked",
			zap.Uint32("pid", pid), zap.String("shim", dllPath), zap.Error(err))
		return nil
	}
	logger.Debug("injected the Keploy interception shim", zap.Uint32("pid", pid), zap.String("shim", dllPath))
	return nil
}

// injectInto maps dllPath into the process and waits for its shim to arm.
//
// The technique is the standard unprivileged one: write the DLL's path into the
// target's address space, then run LoadLibraryA on it in a remote thread.
// kernel32 is loaded at the same base address in every process in a session, so
// the local address of LoadLibraryA is valid in the target too.
func injectInto(pid uint32, dllPath string) error {
	proc, err := windows.OpenProcess(
		windows.PROCESS_CREATE_THREAD|windows.PROCESS_QUERY_INFORMATION|
			windows.PROCESS_VM_OPERATION|windows.PROCESS_VM_WRITE|windows.PROCESS_VM_READ,
		false, pid)
	if err != nil {
		return fmt.Errorf("OpenProcess: %w", err)
	}
	defer func() { _ = windows.CloseHandle(proc) }()

	// Create the arm event before injecting, so the shim cannot signal into a
	// void and we cannot miss it.
	armed, err := createArmEvent(pid)
	if err != nil {
		return fmt.Errorf("create arm event: %w", err)
	}
	defer func() { _ = windows.CloseHandle(armed) }()

	path := append([]byte(dllPath), 0)
	remote, err := virtualAllocEx(proc, uintptr(len(path)))
	if err != nil {
		return fmt.Errorf("VirtualAllocEx: %w", err)
	}
	defer func() { _ = virtualFreeEx(proc, remote) }()

	if err := windows.WriteProcessMemory(proc, remote, &path[0], uintptr(len(path)), nil); err != nil {
		return fmt.Errorf("WriteProcessMemory: %w", err)
	}

	kernel32 := windows.NewLazySystemDLL("kernel32.dll")
	loadLibrary := kernel32.NewProc("LoadLibraryA")
	if err := loadLibrary.Find(); err != nil {
		return fmt.Errorf("resolve LoadLibraryA: %w", err)
	}

	thread, err := createRemoteThread(proc, loadLibrary.Addr(), remote)
	if err != nil {
		// The usual cause is an architecture mismatch: a 32-bit application
		// cannot load this 64-bit shim.
		return fmt.Errorf("CreateRemoteThread: %w", err)
	}
	defer func() { _ = windows.CloseHandle(thread) }()

	if _, err := windows.WaitForSingleObject(thread, uint32(armTimeout/time.Millisecond)); err != nil {
		return fmt.Errorf("waiting for the remote loader thread: %w", err)
	}

	// The DLL is mapped, but its hooks are installed on a thread of its own (see
	// armThread in the shim). Wait for that before releasing the application.
	state, err := windows.WaitForSingleObject(armed, uint32(armTimeout/time.Millisecond))
	if err != nil {
		return fmt.Errorf("waiting for the shim to arm: %w", err)
	}
	if state != windows.WAIT_OBJECT_0 {
		return fmt.Errorf("the interception shim did not arm within %s", armTimeout)
	}
	return nil
}

// createArmEvent creates the per-process event the shim signals once its hooks
// are live. The name is shared with armEventName in shim/keploy_winshim.c.
func createArmEvent(pid uint32) (windows.Handle, error) {
	name, err := windows.UTF16PtrFromString(fmt.Sprintf(`Local\keploy-shim-armed-%d`, pid))
	if err != nil {
		return 0, err
	}
	return windows.CreateEvent(nil, 1 /* manual reset */, 0, name)
}

// resumeProcess resumes every thread of a process created suspended.
//
// os/exec does not surface the process's initial thread handle, so the threads
// are enumerated instead. A freshly created suspended process has exactly one
// thread, but resuming all of them is both correct and robust.
func resumeProcess(pid uint32) error {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPTHREAD, 0)
	if err != nil {
		return fmt.Errorf("CreateToolhelp32Snapshot: %w", err)
	}
	defer func() { _ = windows.CloseHandle(snap) }()

	var entry windows.ThreadEntry32
	entry.Size = uint32(unsafe.Sizeof(entry))
	if err := windows.Thread32First(snap, &entry); err != nil {
		return fmt.Errorf("Thread32First: %w", err)
	}
	resumed := 0
	for {
		if entry.OwnerProcessID == pid {
			th, err := windows.OpenThread(windows.THREAD_SUSPEND_RESUME, false, entry.ThreadID)
			if err == nil {
				_, _ = windows.ResumeThread(th)
				_ = windows.CloseHandle(th)
				resumed++
			}
		}
		if err := windows.Thread32Next(snap, &entry); err != nil {
			break
		}
	}
	if resumed == 0 {
		return fmt.Errorf("no threads of process %d could be resumed", pid)
	}
	return nil
}

// virtualAllocEx and friends are not exposed by x/sys/windows, so they are
// resolved from kernel32 directly.
func virtualAllocEx(proc windows.Handle, size uintptr) (uintptr, error) {
	kernel32 := windows.NewLazySystemDLL("kernel32.dll")
	addr, _, err := kernel32.NewProc("VirtualAllocEx").Call(
		uintptr(proc), 0, size,
		windows.MEM_COMMIT|windows.MEM_RESERVE, windows.PAGE_READWRITE)
	if addr == 0 {
		return 0, err
	}
	return addr, nil
}

func virtualFreeEx(proc windows.Handle, addr uintptr) error {
	kernel32 := windows.NewLazySystemDLL("kernel32.dll")
	ret, _, err := kernel32.NewProc("VirtualFreeEx").Call(
		uintptr(proc), addr, 0, windows.MEM_RELEASE)
	if ret == 0 {
		return err
	}
	return nil
}

func createRemoteThread(proc windows.Handle, start, param uintptr) (windows.Handle, error) {
	kernel32 := windows.NewLazySystemDLL("kernel32.dll")
	h, _, err := kernel32.NewProc("CreateRemoteThread").Call(
		uintptr(proc), 0, 0, start, param, 0, 0)
	if h == 0 {
		return 0, err
	}
	return windows.Handle(h), nil
}

// PrepareApplicationEnv sets the shim's optional environment overrides for the
// application keploy is about to launch.
//
// The shim does not depend on these — it reads the control pipe name from the
// sidecar file next to the DLL — but a debug run wants the shim's own tracing,
// and that has nowhere else to come from.
func PrepareApplicationEnv(env []string, sessionDir string, debug bool) []string {
	if !debug {
		return env
	}
	if env == nil {
		env = os.Environ()
	}
	return append(env,
		EnvShimDebug+"=1",
		EnvShimLog+"="+ShimLogPath(sessionDir),
	)
}
