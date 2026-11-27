//go:build linux

package bpfsafety

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/cilium/ebpf"
	"golang.org/x/sys/unix"
)

const (
	reaperStateFileName      = "reaper_state"
	kallsymsSnapshotFileName = "kallsyms_snapshot"
	legacyStampFileName      = "first_free_boottime"

	maxSnapshotEntries = 512
	kallsymsPath       = "/proc/kallsyms"

	stateCleanSnapshot  uint64 = 1 << 0
	stateDeadPass1      uint64 = 1 << 1
	stateDeadSnapshot   uint64 = 1 << 2
	stateStalePermanent uint64 = 1 << 3
	stateForcedRebuild  uint64 = 1 << 4
)

// trampolineKey computes Linux's bpf_trampoline_compute_key value for a
// tracing/LSM attachment: (btf_obj_id << 32) | 0x80000000 | btf_id.
// For vmlinux targets on Linux 6.0+, btf_obj_id is 1.
func trampolineKey(btfObjID, btfID uint32) uint64 {
	if btfID == 0 {
		return 0
	}
	obj := btfObjID
	if obj == 0 {
		obj = 1
	}
	return (uint64(obj) << 32) | 0x80000000 | uint64(btfID)
}

type trampImageRef struct {
	Key  uint64
	Addr uint64
}

type reaperState struct {
	Flags           uint64
	SnapshotCount   uint64
	FirstSeenBootNs uint64
	LastProbeBootNs uint64
}

func encodeReaperState(s reaperState) [32]byte {
	var b [32]byte
	binary.LittleEndian.PutUint64(b[0:8], s.Flags)
	binary.LittleEndian.PutUint64(b[8:16], s.SnapshotCount)
	binary.LittleEndian.PutUint64(b[16:24], s.FirstSeenBootNs)
	binary.LittleEndian.PutUint64(b[24:32], s.LastProbeBootNs)
	return b
}

func decodeReaperState(b []byte) reaperState {
	var s reaperState
	if len(b) >= 32 {
		s.Flags = binary.LittleEndian.Uint64(b[0:8])
		s.SnapshotCount = binary.LittleEndian.Uint64(b[8:16])
		s.FirstSeenBootNs = binary.LittleEndian.Uint64(b[16:24])
		s.LastProbeBootNs = binary.LittleEndian.Uint64(b[24:32])
	}
	return s
}

func isMetadataFile(name string) bool {
	return name == reaperStateFileName || name == kallsymsSnapshotFileName || name == legacyStampFileName
}

// stateStore persists reaperState and kallsyms snapshots inside a grace
// directory. On real bpffs (BPF_FS_MAGIC), it uses pinned BPF_MAP_TYPE_ARRAY
// maps so no extra host filesystem mount is required. On non-bpffs test
// directories (t.TempDir()), it uses regular files.
type stateStore interface {
	readState(dir string) (reaperState, bool, error)
	writeState(dir string, s reaperState) error
	readSnapshot(dir string, count uint64) ([]trampImageRef, error)
	writeSnapshot(dir string, refs []trampImageRef) error
}

type autoStateStore struct{}

func (autoStateStore) isBPFFS(dir string) bool {
	var stfs unix.Statfs_t
	return unix.Statfs(dir, &stfs) == nil && stfs.Type == unix.BPF_FS_MAGIC
}

func (a autoStateStore) readState(dir string) (reaperState, bool, error) {
	p := filepath.Join(dir, reaperStateFileName)
	if a.isBPFFS(dir) {
		m, err := ebpf.LoadPinnedMap(p, nil)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return reaperState{}, false, nil
			}
			return reaperState{}, false, err
		}
		defer func() { _ = m.Close() }()
		var buf [32]byte
		if err := m.Lookup(uint32(0), &buf); err != nil {
			return reaperState{}, false, err
		}
		return decodeReaperState(buf[:]), true, nil
	}
	b, err := os.ReadFile(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return reaperState{}, false, nil
		}
		return reaperState{}, false, err
	}
	if len(b) < 32 {
		return reaperState{}, false, nil
	}
	return decodeReaperState(b), true, nil
}

func (a autoStateStore) writeState(dir string, s reaperState) error {
	p := filepath.Join(dir, reaperStateFileName)
	buf := encodeReaperState(s)
	if a.isBPFFS(dir) {
		m, err := ebpf.LoadPinnedMap(p, nil)
		if err == nil {
			defer func() { _ = m.Close() }()
			return m.Update(uint32(0), buf[:], ebpf.UpdateAny)
		}
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		m, err = ebpf.NewMap(&ebpf.MapSpec{
			Name:       "grace_state",
			Type:       ebpf.Array,
			KeySize:    4,
			ValueSize:  32,
			MaxEntries: 1,
		})
		if err != nil {
			return err
		}
		if err := m.Update(uint32(0), buf[:], ebpf.UpdateAny); err != nil {
			_ = m.Close()
			return err
		}
		if err := m.Pin(p); err != nil {
			_ = m.Close()
			return err
		}
		return m.Close()
	}
	return os.WriteFile(p, buf[:], 0o600)
}

func (a autoStateStore) writeSnapshot(dir string, refs []trampImageRef) error {
	if len(refs) > maxSnapshotEntries {
		return fmt.Errorf("kallsyms snapshot exceeds cap (%d > %d)", len(refs), maxSnapshotEntries)
	}
	p := filepath.Join(dir, kallsymsSnapshotFileName)
	if a.isBPFFS(dir) {
		m, err := ebpf.LoadPinnedMap(p, nil)
		if err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				return err
			}
			m, err = ebpf.NewMap(&ebpf.MapSpec{
				Name:       "grace_ksym",
				Type:       ebpf.Array,
				KeySize:    4,
				ValueSize:  16,
				MaxEntries: maxSnapshotEntries,
			})
			if err != nil {
				return err
			}
			for i, ref := range refs {
				var val [16]byte
				binary.LittleEndian.PutUint64(val[0:8], ref.Key)
				binary.LittleEndian.PutUint64(val[8:16], ref.Addr)
				if uerr := m.Update(uint32(i), val[:], ebpf.UpdateAny); uerr != nil {
					_ = m.Close()
					return uerr
				}
			}
			if perr := m.Pin(p); perr != nil {
				_ = m.Close()
				return perr
			}
			return m.Close()
		}
		defer func() { _ = m.Close() }()
		for i, ref := range refs {
			var val [16]byte
			binary.LittleEndian.PutUint64(val[0:8], ref.Key)
			binary.LittleEndian.PutUint64(val[8:16], ref.Addr)
			if uerr := m.Update(uint32(i), val[:], ebpf.UpdateAny); uerr != nil {
				return uerr
			}
		}
		return nil
	}
	buf := make([]byte, len(refs)*16)
	for i, ref := range refs {
		binary.LittleEndian.PutUint64(buf[i*16:i*16+8], ref.Key)
		binary.LittleEndian.PutUint64(buf[i*16+8:i*16+16], ref.Addr)
	}
	return os.WriteFile(p, buf, 0o600)
}

func (a autoStateStore) readSnapshot(dir string, count uint64) ([]trampImageRef, error) {
	if count == 0 {
		return nil, nil
	}
	if count > maxSnapshotEntries {
		return nil, fmt.Errorf("invalid kallsyms snapshot count %d", count)
	}
	p := filepath.Join(dir, kallsymsSnapshotFileName)
	if a.isBPFFS(dir) {
		m, err := ebpf.LoadPinnedMap(p, nil)
		if err != nil {
			return nil, err
		}
		defer func() { _ = m.Close() }()
		out := make([]trampImageRef, 0, count)
		for i := uint32(0); i < uint32(count); i++ {
			var val [16]byte
			if err := m.Lookup(i, &val); err != nil {
				return nil, err
			}
			out = append(out, trampImageRef{
				Key:  binary.LittleEndian.Uint64(val[0:8]),
				Addr: binary.LittleEndian.Uint64(val[8:16]),
			})
		}
		return out, nil
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	if uint64(len(b)) < count*16 {
		return nil, fmt.Errorf("truncated kallsyms snapshot (%d bytes, need %d)", len(b), count*16)
	}
	out := make([]trampImageRef, 0, count)
	for i := uint64(0); i < count; i++ {
		out = append(out, trampImageRef{
			Key:  binary.LittleEndian.Uint64(b[i*16 : i*16+8]),
			Addr: binary.LittleEndian.Uint64(b[i*16+8 : i*16+16]),
		})
	}
	return out, nil
}

// kallsymsSysctlAllowsJIT reports whether kernel sysctls confirm BPF JIT
// trampoline symbols are exported to /proc/kallsyms. Returns false (triggering
// BPF_ENABLE_STATS fallback or fail-closed) if bpf_jit_kallsyms != 1 or
// bpf_jit_harden != 0 or either sysctl cannot be read.
func kallsymsSysctlAllowsJIT() bool {
	ks, err := os.ReadFile("/proc/sys/net/core/bpf_jit_kallsyms")
	if err != nil || strings.TrimSpace(string(ks)) != "1" {
		return false
	}
	hd, err := os.ReadFile("/proc/sys/net/core/bpf_jit_harden")
	if err != nil || strings.TrimSpace(string(hd)) != "0" {
		return false
	}
	return true
}

// parseKallsymsTrampolines scans path (normally /proc/kallsyms) and returns:
//   - tramps: map[trampolineKey][]imageAddr for every `bpf_trampoline_<key> [bpf]`
//     entry with a non-zero address;
//   - addrsVisible: true if /proc/kallsyms exposed non-zero kernel addresses
//     (i.e. kptr_restrict did not zero them out).
func parseKallsymsTrampolines(path string) (tramps map[uint64][]uint64, addrsVisible bool, err error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = f.Close() }()

	tramps = make(map[uint64][]uint64)
	sc := bufio.NewScanner(f)
	// Some kallsyms lines can be long; 128 KiB buffer avoids token-too-long.
	sc.Buffer(make([]byte, 0, 64*1024), 128*1024)
	for sc.Scan() {
		line := sc.Text()
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		addr, perr := strconv.ParseUint(fields[0], 16, 64)
		if perr != nil {
			continue
		}
		if addr != 0 {
			addrsVisible = true
		}
		if len(fields) >= 4 && fields[3] == "[bpf]" && strings.HasPrefix(fields[2], "bpf_trampoline_") {
			if addr == 0 {
				// kptr_restrict is hiding BPF trampoline addresses.
				return nil, false, nil
			}
			keyStr := strings.TrimPrefix(fields[2], "bpf_trampoline_")
			key, kerr := strconv.ParseUint(keyStr, 10, 64)
			if kerr != nil || key == 0 {
				continue
			}
			tramps[key] = append(tramps[key], addr)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, false, err
	}
	return tramps, addrsVisible, nil
}

// snapshotKeysFromKallsyms collects every (key, addr) entry in tramps whose key
// is in wantKeys.
func snapshotKeysFromKallsyms(tramps map[uint64][]uint64, wantKeys map[uint64]struct{}) []trampImageRef {
	var out []trampImageRef
	for k := range wantKeys {
		for _, addr := range tramps[k] {
			if addr != 0 {
				out = append(out, trampImageRef{Key: k, Addr: addr})
			}
		}
	}
	return out
}

// survivingSnapshotKeys returns the subset of snapshot entries whose exact
// (key, addr) is still present in currentTramps.
func survivingSnapshotKeys(snapshot []trampImageRef, currentTramps map[uint64][]uint64) []trampImageRef {
	var still []trampImageRef
	for _, ref := range snapshot {
		for _, curAddr := range currentTramps[ref.Key] {
			if curAddr == ref.Addr {
				still = append(still, ref)
				break
			}
		}
	}
	return still
}
