//go:build linux && (amd64 || arm64)

package cbshim

// exec_helpers.go — production-tested helpers for opening userspace
// executables for uprobe attachment. Port of the patterns used by
// enterprise's TLSUprobeLoader (pkg/agent/proxy/tls_loader.go in the
// enterprise repo). Kept verbatim where possible so the two loaders
// stay aligned for an eventual unified base.

import (
	"debug/elf"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/cilium/ebpf/link"
)

// openExecutableLenient is a defensive wrapper around link.OpenExecutable.
// The kernel's perf_event_open path refuses to attach uprobes to files
// without an executable mode bit, even when the file itself contains
// valid ELF code (some package managers ship libraries 0644). Rather
// than failing the whole attach, we try to recover via:
//
//  1. Hardlink the target into a per-agent tempdir, chmod the link
//     +x — preserves the original inode (so uprobes attached via
//     either path hit the same code) without touching the source
//     library's mode bits.
//  2. If the hardlink path fails (cross-filesystem, quota, permission)
//     fall back to in-place chmod. This DOES modify the source file's
//     mode, but the inode is preserved so any live mappings stay
//     valid.
//
// Refuses to chmod non-regular files (sockets, devices, symlinks to
// such) — avoids escalating privilege via a hostile symlink.
func openExecutableLenient(path string) (*link.Executable, error) {
	ex, err := link.OpenExecutable(path)
	if err == nil {
		return ex, nil
	}
	if !strings.Contains(err.Error(), "is not executable") {
		return nil, err
	}
	info, statErr := os.Stat(path)
	if statErr != nil {
		return nil, fmt.Errorf("%w; stat failed: %v", err, statErr)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%w; target is not a regular file (mode=%v)", err, info.Mode())
	}

	linkPath, linkErr := hardlinkForUprobe(path)
	if linkErr == nil {
		if chmodErr := os.Chmod(linkPath, info.Mode()|0o111); chmodErr != nil {
			_ = os.Remove(linkPath)
			return nil, fmt.Errorf("%w; chmod +x on hardlink failed: %v", err, chmodErr)
		}
		ex2, openErr := link.OpenExecutable(linkPath)
		if openErr == nil {
			return ex2, nil
		}
		_ = os.Remove(linkPath)
		// fall through to in-place chmod
	}

	if chmodErr := os.Chmod(path, info.Mode()|0o111); chmodErr != nil {
		return nil, fmt.Errorf("%w; chmod +x failed: %v", err, chmodErr)
	}
	return link.OpenExecutable(path)
}

// hardlinkForUprobe creates a hardlink of target inside a per-agent
// tempdir. The returned path shares the original inode (so uprobes
// attached via either path hit the same code) but is independently
// chmod-able. Returns an error if the link crosses a filesystem
// boundary or the tempdir cannot be created.
//
// Name pattern is "ino-<inode>" so repeated lookups of the same target
// reuse a single link instead of accumulating one entry per call.
// Inode-recycling is handled: if a stale link with the same name
// points at a different inode (left over from a previous agent run
// after the kernel recycled the number), we remove and recreate.
func hardlinkForUprobe(target string) (string, error) {
	dir := filepath.Join(os.TempDir(), fmt.Sprintf("keploy-uprobe-%d", os.Getpid()))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return "", err
	}
	info, err := os.Stat(target)
	if err != nil {
		return "", err
	}
	sys, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return "", fmt.Errorf("unexpected FileInfo.Sys type")
	}
	linkPath := filepath.Join(dir, fmt.Sprintf("ino-%d", sys.Ino))
	if existing, statErr := os.Stat(linkPath); statErr == nil {
		if esys, ok := existing.Sys().(*syscall.Stat_t); ok && esys.Ino == sys.Ino {
			return linkPath, nil
		}
		_ = os.Remove(linkPath)
	}
	if err := os.Link(target, linkPath); err != nil {
		return "", err
	}
	return linkPath, nil
}

// hasELFSymbol checks whether the ELF file at path exports a given
// symbol in either the dynamic symbol table (for shared libraries) or
// the regular symbol table (for statically-linked binaries). Used by
// the loader to skip libraries that lack X509_digest before incurring
// the cost of an uprobe attach attempt — turns an opaque libbpf
// "no such symbol" error into a clean upfront skip.
//
// Returns false on any open / parse error: a file we can't read can't
// be a useful uprobe target.
func hasELFSymbol(path, symbolName string) bool {
	f, err := elf.Open(path)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()

	if dynsyms, err := f.DynamicSymbols(); err == nil {
		for _, s := range dynsyms {
			if s.Name == symbolName {
				return true
			}
		}
	}
	if syms, err := f.Symbols(); err == nil {
		for _, s := range syms {
			if s.Name == symbolName {
				return true
			}
		}
	}
	return false
}

// hostVisiblePath resolves a path observed inside a target process's
// mount namespace to one we (the agent) can actually open. When the
// agent runs in a different mount namespace (e.g. sidecar to a
// containerized app), the literal path strings inside the target's
// /proc/<pid>/maps may not resolve in our own filesystem view; the
// kernel exposes /proc/<pid>/root/ as a stable bridge that walks the
// target's namespace and returns a file descriptor opaque to namespace
// differences.
//
// Returns the /proc-bridged path when it's stat-able, the literal
// path otherwise. The literal path is the right thing when the agent
// and target share the namespace (the common case).
func hostVisiblePath(pid int, path string) string {
	hostPath := fmt.Sprintf("/proc/%d/root%s", pid, path)
	if _, err := os.Stat(hostPath); err == nil {
		return hostPath
	}
	return path
}
