//go:build linux

package linux

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/unix"
)

// BPF attach types for sk_skb programs (from include/uapi/linux/bpf.h).
const (
	BPF_SK_SKB_STREAM_PARSER  = 25
	BPF_SK_SKB_STREAM_VERDICT = 26
)

// attachSkSkb attaches an sk_skb program to a sockmap/sockhash.
// Equivalent to bpf(BPF_PROG_ATTACH, {target_fd=mapFD, attach_bpf_fd=progFD, attach_type=..}).
func attachSkSkb(progFD, mapFD, attachType int) error {
	attr := struct {
		targetFD    uint32
		attachBpfFD uint32
		attachType  uint32
		attachFlags uint32
	}{
		targetFD:    uint32(mapFD),
		attachBpfFD: uint32(progFD),
		attachType:  uint32(attachType),
		attachFlags: 0,
	}

	_, _, errno := unix.Syscall(
		unix.SYS_BPF,
		8, // BPF_PROG_ATTACH
		uintptr(unsafe.Pointer(&attr)),
		unsafe.Sizeof(attr),
	)
	if errno != 0 {
		return fmt.Errorf("bpf_prog_attach(type=%d): %w", attachType, errno)
	}
	return nil
}

// detachSkSkb detaches an sk_skb program from a sockmap/sockhash.
func detachSkSkb(progFD, mapFD, attachType int) error {
	attr := struct {
		targetFD    uint32
		attachBpfFD uint32
		attachType  uint32
		attachFlags uint32
	}{
		targetFD:    uint32(mapFD),
		attachBpfFD: uint32(progFD),
		attachType:  uint32(attachType),
		attachFlags: 0,
	}

	_, _, errno := unix.Syscall(
		unix.SYS_BPF,
		9, // BPF_PROG_DETACH
		uintptr(unsafe.Pointer(&attr)),
		unsafe.Sizeof(attr),
	)
	if errno != 0 {
		return fmt.Errorf("bpf_prog_detach(type=%d): %w", attachType, errno)
	}
	return nil
}
