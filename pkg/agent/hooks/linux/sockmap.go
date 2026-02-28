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
	"go.uber.org/zap"
)

// Pinned paths for sockmap BPF maps.
const (
	SockhashPinPath  = "/sys/fs/bpf/keploy_sockhash"
	CaptureRBPinPath = "/sys/fs/bpf/keploy_capture_rb"
	SockMetaPinPath  = "/sys/fs/bpf/keploy_sock_meta"
)

// sockmap holds references to the sockmap BPF objects so they can be
// cleaned up on unload.
type sockmap struct {
	sockhash  *ebpf.Map
	captureRB *ebpf.Map
	sockMeta  *ebpf.Map
	// sk_skb programs are attached to the sockhash via bpf_prog_attach
	parserFD  int
	verdictFD int
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

	// Attach sk_skb programs to the sockhash map.
	// bpf_prog_attach(parser, sockhash, BPF_SK_SKB_STREAM_PARSER)
	// bpf_prog_attach(verdict, sockhash, BPF_SK_SKB_STREAM_VERDICT)
	parserFD := objs.KeployStreamParser.FD()
	verdictFD := objs.KeployStreamVerdict.FD()
	mapFD := sm.sockhash.FD()

	if err := attachSkSkb(parserFD, mapFD, BPF_SK_SKB_STREAM_PARSER); err != nil {
		objs.Close()
		return nil, fmt.Errorf("failed to attach stream_parser to sockhash: %w", err)
	}
	sm.parserFD = parserFD
	logger.Info("Attached sk_skb stream_parser to sockhash")

	if err := attachSkSkb(verdictFD, mapFD, BPF_SK_SKB_STREAM_VERDICT); err != nil {
		objs.Close()
		return nil, fmt.Errorf("failed to attach stream_verdict to sockhash: %w", err)
	}
	sm.verdictFD = verdictFD
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
		if sm.parserFD > 0 {
			_ = detachSkSkb(sm.parserFD, sm.sockhash.FD(), BPF_SK_SKB_STREAM_PARSER)
		}
		if sm.verdictFD > 0 {
			_ = detachSkSkb(sm.verdictFD, sm.sockhash.FD(), BPF_SK_SKB_STREAM_VERDICT)
		}
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
