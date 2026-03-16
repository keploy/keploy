//go:build linux

// Package linux provides the sockmap BPF loader for low-latency mode.
// When --low-latency is enabled, this loads sk_skb stream_parser and
// stream_verdict programs, attaches them to a sockhash map, and pins
// the maps so the enterprise proxy can open them.
package linux

import (
	"fmt"
	"os"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"go.uber.org/zap"
	"golang.org/x/sys/unix"
)

// bpfFSPath is the standard mount point for bpffs.
const bpfFSPath = "/sys/fs/bpf"

// Pinned paths for sockmap BPF maps.
const (
	SockhashPinPath  = bpfFSPath + "/keploy_sockhash"
	CaptureRBPinPath = bpfFSPath + "/keploy_capture_rb"
	SockMetaPinPath  = bpfFSPath + "/keploy_sock_meta"
)

// ensureBPFFS checks that /sys/fs/bpf is a bpffs filesystem. When running
// inside Docker, /sys/fs/bpf may be a tmpfs or a bind-mounted host bpffs
// with restrictive permissions. In either case, mount a fresh bpffs on top.
func ensureBPFFS(logger *zap.Logger) {
	// Check if /sys/fs/bpf is already a bpffs (magic = 0xcafe4a11).
	var stat unix.Statfs_t
	if err := unix.Statfs(bpfFSPath, &stat); err == nil && stat.Type == 0xcafe4a11 {
		// Already a bpffs — check if it's writable.
		testPath := bpfFSPath + "/.keploy_write_test"
		f, err := os.Create(testPath)
		if err == nil {
			f.Close()
			os.Remove(testPath)
			return // bpffs is writable
		}
		logger.Debug("bpffs exists but is not writable, remounting",
			zap.String("path", bpfFSPath), zap.Error(err))
	}

	// Ensure the mount point directory exists.
	_ = os.MkdirAll(bpfFSPath, 0755)

	// Mount a fresh bpffs. This requires CAP_SYS_ADMIN + no AppArmor restriction.
	if err := unix.Mount("bpf", bpfFSPath, "bpf", 0, ""); err != nil {
		logger.Warn("Failed to mount bpffs — BPF map pinning will not work",
			zap.String("path", bpfFSPath), zap.Error(err))
		return
	}

	logger.Info("Mounted fresh bpffs for BPF map pinning", zap.String("path", bpfFSPath))
}

// sockmap holds references to the sockmap BPF objects so they can be
// cleaned up on unload.
type sockmap struct {
	sockhash  *ebpf.Map
	captureRB *ebpf.Map
	sockMeta  *ebpf.Map
	// sk_skb programs kept alive for detach
	parser  *ebpf.Program
	verdict *ebpf.Program
}

// loadSockmap loads the sk_skb BPF programs and sockmap maps, pins them
// to bpffs, and attaches the sk_skb programs to the sockhash.
// This is only called when LowLatency mode is enabled.
func (h *Hooks) loadSockmap(logger *zap.Logger) (*sockmap, error) {
	logger.Info("Loading sockmap BPF programs for low-latency mode")

	// Load the separately compiled sk_skb BPF spec.
	// This corresponds to keploy_sk_skb.c compiled via bpf2go as "skskb".
	spec, err := loadSkskb()
	if err != nil {
		return nil, fmt.Errorf("failed to load sk_skb BPF spec: %w", err)
	}

	var objs skskbObjects
	if err := spec.LoadAndAssign(&objs, nil); err != nil {
		return nil, fmt.Errorf("failed to load sk_skb BPF objects: %w", err)
	}

	sm := &sockmap{}

	// Get references to the maps
	sm.sockhash = objs.KeploySockhash
	sm.captureRB = objs.KeployCaptureRb
	sm.sockMeta = objs.KeploySockMeta

	// Keep program references for detach
	sm.parser = objs.KeployStreamParser
	sm.verdict = objs.KeployStreamVerdict

	// Clean stale pins
	_ = os.Remove(SockhashPinPath)
	_ = os.Remove(CaptureRBPinPath)
	_ = os.Remove(SockMetaPinPath)

	// Pin maps so the enterprise proxy can open them
	if err := sm.sockhash.Pin(SockhashPinPath); err != nil {
		objs.Close()
		return nil, fmt.Errorf("failed to pin sockhash: %w", err)
	}
	logger.Info("Pinned sockhash map", zap.String("path", SockhashPinPath))

	if err := sm.captureRB.Pin(CaptureRBPinPath); err != nil {
		objs.Close()
		return nil, fmt.Errorf("failed to pin capture ringbuf: %w", err)
	}
	logger.Info("Pinned capture ringbuf", zap.String("path", CaptureRBPinPath))

	if err := sm.sockMeta.Pin(SockMetaPinPath); err != nil {
		objs.Close()
		return nil, fmt.Errorf("failed to pin sock_meta: %w", err)
	}
	logger.Info("Pinned sock_meta map", zap.String("path", SockMetaPinPath))

	// Attach sk_skb programs to the sockhash map using cilium/ebpf's
	// RawAttachProgram, which correctly sizes the bpf_attr struct.
	mapFD := sm.sockhash.FD()

	if err := link.RawAttachProgram(link.RawAttachProgramOptions{
		Target:  mapFD,
		Program: sm.parser,
		Attach:  ebpf.AttachSkSKBStreamParser,
	}); err != nil {
		objs.Close()
		return nil, fmt.Errorf("failed to attach stream_parser to sockhash: %w", err)
	}
	logger.Info("Attached sk_skb stream_parser to sockhash")

	if err := link.RawAttachProgram(link.RawAttachProgramOptions{
		Target:  mapFD,
		Program: sm.verdict,
		Attach:  ebpf.AttachSkSKBStreamVerdict,
	}); err != nil {
		objs.Close()
		return nil, fmt.Errorf("failed to attach stream_verdict to sockhash: %w", err)
	}
	logger.Info("Attached sk_skb stream_verdict to sockhash")

	return sm, nil
}

// unloadSockmap cleans up sockmap BPF resources.
func (sm *sockmap) unload() {
	if sm == nil {
		return
	}
	// Detach sk_skb programs
	if sm.sockhash != nil {
		if sm.parser != nil {
			_ = link.RawDetachProgram(link.RawDetachProgramOptions{
				Target:  sm.sockhash.FD(),
				Program: sm.parser,
				Attach:  ebpf.AttachSkSKBStreamParser,
			})
		}
		if sm.verdict != nil {
			_ = link.RawDetachProgram(link.RawDetachProgramOptions{
				Target:  sm.sockhash.FD(),
				Program: sm.verdict,
				Attach:  ebpf.AttachSkSKBStreamVerdict,
			})
		}
	}

	// Close programs
	if sm.parser != nil {
		sm.parser.Close()
	}
	if sm.verdict != nil {
		sm.verdict.Close()
	}

	// Close maps (also unpins)
	if sm.sockhash != nil {
		sm.sockhash.Close()
	}
	if sm.captureRB != nil {
		sm.captureRB.Close()
	}
	if sm.sockMeta != nil {
		sm.sockMeta.Close()
	}

	// Remove pins
	_ = os.Remove(SockhashPinPath)
	_ = os.Remove(CaptureRBPinPath)
	_ = os.Remove(SockMetaPinPath)
}
