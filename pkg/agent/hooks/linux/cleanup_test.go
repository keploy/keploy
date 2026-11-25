//go:build linux

package linux

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/cilium/ebpf"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// An orphan is found by the name the kernel keeps, at most 15 bytes: a
// program whose name is longer (k_udp_sock_release, k_ready_connect4) is
// matched by its cut name, never missed.
func TestAnOrphanIsMatchedByTheNameTheKernelKeeps(t *testing.T) {
	for in, want := range map[string]string{
		"k_connect4":         "k_connect4",
		"k_udp_sock_release": "k_udp_sock_rele",
		"k_ready_connect4":   "k_ready_connect",
		"k_ready_connect6":   "k_ready_connect",
	} {
		if got := kernelProgName(in); got != want {
			t.Errorf("kernelProgName(%q) = %q, want %q", in, got, want)
		}
	}
	for name := range keployBPFProgNames {
		if n := kernelProgName(name); len(n) > 15 {
			t.Errorf("%s: kept as %q, longer than the kernel keeps", name, n)
		}
	}
}

// The orphan cleanup's match handles both the kernel's 15-byte cut name and
// the full BTF function name that cilium/ebpf reports on BTF-enabled kernels.
func TestAnOrphanCleanupMatchesTheCutName(t *testing.T) {
	if !orphanNamed("k_ready_connect", "k_ready_connect4") ||
		!orphanNamed("k_udp_sock_rele", "k_udp_sock_release") ||
		!orphanNamed("k_app_conn_rele", "k_app_conn_release") ||
		!orphanNamed("k_app_conn_release", "k_app_conn_release") ||
		!orphanNamed("k_connect4", "k_connect4") {
		t.Fatal("an orphan attached under its cut or full BTF name is not matched")
	}
	if orphanNamed("k_connect6", "k_connect4") || orphanNamed("k_ready_connect4", "k_ready_connect6") {
		t.Fatal("another program matched")
	}
}

func TestCgroupOwnerMapNameFitsKernelLimit(t *testing.T) {
	if len(cgroupOwnerMapName) > 15 {
		t.Fatalf("cgroupOwnerMapName %q length %d exceeds BPF_OBJ_NAME_LEN-1 (15)", cgroupOwnerMapName, len(cgroupOwnerMapName))
	}
	if cgroupOwnerMapName != kernelProgName(cgroupOwnerMapName) {
		t.Fatalf("cgroupOwnerMapName %q would be truncated by kernel", cgroupOwnerMapName)
	}
}

func TestSelfOwnerMapRegistryTracksAndExcludes(t *testing.T) {
	const id ebpf.MapID = 987654
	registerSelfOwnerMapID(id)
	t.Cleanup(func() { unregisterSelfOwnerMapID(id) })

	snap := snapshotSelfOwnerMapIDs()
	if _, ok := snap[id]; !ok {
		t.Fatalf("expected map ID %d in selfOwnerMapIDs snapshot", id)
	}

	unregisterSelfOwnerMapID(id)
	snap = snapshotSelfOwnerMapIDs()
	if _, ok := snap[id]; ok {
		t.Fatalf("expected map ID %d removed after unregister", id)
	}
}

func TestCgroupCleanupSkipsWhenOtherOwnerMapExistsAcrossPIDNamespaces(t *testing.T) {
	queried := 0
	detached := 0
	deps := cgroupCleanupDeps{
		hasOtherOwner: func(cgroupID uint64, exclude map[ebpf.MapID]struct{}) (bool, error) {
			if cgroupID != 42 {
				t.Errorf("hasOtherOwner got cgroupID=%d, want 42", cgroupID)
			}
			if _, ok := exclude[7]; !ok {
				t.Errorf("hasOtherOwner missing self exclude ID 7")
			}
			return true, nil
		},
		// Simulate hostPID: false where /proc shows no sibling keploy.
		hasOtherProc: func(int) bool { return false },
		queryPrograms: func(uintptr, ebpf.AttachType) ([]attachedCgroupProg, error) {
			queried++
			return []attachedCgroupProg{{id: 100}}, nil
		},
		linkedPrograms: func(uint64) (cgroupLinkSnapshot, error) {
			return cgroupLinkSnapshot{}, nil
		},
		progNameByID: func(ebpf.ProgramID) (string, error) { return "k_sockops", nil },
		detachProgram: func(uintptr, ebpf.ProgramID, ebpf.AttachType) error {
			detached++
			return nil
		},
		selfPID: 1,
	}

	got := cleanOrphanedCgroupProgramsWithDeps(zap.NewNop(), 99, "/sys/fs/cgroup", 42, map[ebpf.MapID]struct{}{7: {}}, deps)
	if got != 0 || queried != 0 || detached != 0 {
		t.Fatalf("got cleaned=%d queried=%d detached=%d, want all 0 when another live cgroup owner exists", got, queried, detached)
	}
}

func TestCgroupCleanupFailsClosedOnOwnerInspectionError(t *testing.T) {
	queried := 0
	detached := 0
	deps := cgroupCleanupDeps{
		hasOtherOwner: func(uint64, map[ebpf.MapID]struct{}) (bool, error) {
			return false, errors.New("permission denied")
		},
		hasOtherProc: func(int) bool { return false },
		queryPrograms: func(uintptr, ebpf.AttachType) ([]attachedCgroupProg, error) {
			queried++
			return []attachedCgroupProg{{id: 100}}, nil
		},
		linkedPrograms: func(uint64) (cgroupLinkSnapshot, error) {
			return cgroupLinkSnapshot{}, nil
		},
		progNameByID: func(ebpf.ProgramID) (string, error) { return "k_sockops", nil },
		detachProgram: func(uintptr, ebpf.ProgramID, ebpf.AttachType) error {
			detached++
			return nil
		},
		selfPID: 1,
	}

	got := cleanOrphanedCgroupProgramsWithDeps(zap.NewNop(), 99, "/sys/fs/cgroup", 42, nil, deps)
	if got != 0 || queried != 0 || detached != 0 {
		t.Fatalf("got cleaned=%d queried=%d detached=%d, want all 0 (fail-closed before query) on owner inspection error", got, queried, detached)
	}
}

func TestCgroupCleanupSkipsWhenOtherProcInSamePIDNamespace(t *testing.T) {
	queried := 0
	detached := 0
	deps := cgroupCleanupDeps{
		hasOtherOwner: func(uint64, map[ebpf.MapID]struct{}) (bool, error) {
			return false, nil // simulates older pre-fix keploy in same PID ns
		},
		hasOtherProc: func(selfPID int) bool { return selfPID == 55 },
		queryPrograms: func(uintptr, ebpf.AttachType) ([]attachedCgroupProg, error) {
			queried++
			return []attachedCgroupProg{{id: 100}}, nil
		},
		linkedPrograms: func(uint64) (cgroupLinkSnapshot, error) {
			return cgroupLinkSnapshot{}, nil
		},
		progNameByID: func(ebpf.ProgramID) (string, error) { return "k_sockops", nil },
		detachProgram: func(uintptr, ebpf.ProgramID, ebpf.AttachType) error {
			detached++
			return nil
		},
		selfPID: 55,
	}

	got := cleanOrphanedCgroupProgramsWithDeps(zap.NewNop(), 99, "/sys/fs/cgroup", 42, nil, deps)
	if got != 0 || queried != 0 || detached != 0 {
		t.Fatalf("got cleaned=%d queried=%d detached=%d, want all 0 when another keploy exists in /proc", got, queried, detached)
	}
}

func TestCgroupCleanupNeverDetachesBPFLinkPrograms(t *testing.T) {
	core, logs := observer.New(zap.InfoLevel)
	logger := zap.New(core)

	var detachedIDs []ebpf.ProgramID
	deps := cgroupCleanupDeps{
		hasOtherOwner: func(uint64, map[ebpf.MapID]struct{}) (bool, error) { return false, nil },
		hasOtherProc:  func(int) bool { return false },
		queryPrograms: func(_ uintptr, at ebpf.AttachType) ([]attachedCgroupProg, error) {
			switch at {
			case ebpf.AttachCGroupSockOps:
				// Prog 101 reported with linkID directly from BPF_PROG_QUERY;
				// Prog 102 has linkID=0 from query, matched via linkedPrograms.exact.
				return []attachedCgroupProg{
					{id: 101, linkID: 5001},
					{id: 102, linkID: 0},
				}, nil
			case ebpf.AttachCGroupInet4Connect:
				// Prog 103 has linkID=0 from query, matched via linkedPrograms.anyCgroup.
				// Prog 104 is a true legacy BPF_PROG_ATTACH orphan (no bpf_link).
				return []attachedCgroupProg{
					{id: 103, linkID: 0},
					{id: 104, linkID: 0},
				}, nil
			default:
				return nil, nil
			}
		},
		linkedPrograms: func(cgID uint64) (cgroupLinkSnapshot, error) {
			if cgID != 42 {
				t.Errorf("linkedPrograms got cgroupID=%d, want 42", cgID)
			}
			return cgroupLinkSnapshot{
				exact: map[cgroupAttachKey]bool{
					{attachType: ebpf.AttachCGroupSockOps, progID: 102}: true,
				},
				anyCgroup: map[ebpf.ProgramID]bool{
					103: true,
				},
			}, nil
		},
		progNameByID: func(id ebpf.ProgramID) (string, error) {
			switch id {
			case 101, 102:
				return "k_sockops", nil
			case 103, 104:
				return "k_connect4", nil
			default:
				return "", errors.New("unknown")
			}
		},
		detachProgram: func(_ uintptr, id ebpf.ProgramID, _ ebpf.AttachType) error {
			detachedIDs = append(detachedIDs, id)
			return nil
		},
		selfPID: 1,
	}

	got := cleanOrphanedCgroupProgramsWithDeps(logger, 99, "/sys/fs/cgroup", 42, nil, deps)
	if got != 1 {
		t.Fatalf("cleaned = %d, want 1 (only legacy prog 104)", got)
	}
	if len(detachedIDs) != 1 || detachedIDs[0] != 104 {
		t.Fatalf("detachedIDs = %v, want [104]", detachedIDs)
	}
	for _, entry := range logs.All() {
		if entry.Message == "Detaching orphaned BPF program from cgroup" {
			if pid := entry.ContextMap()["progID"]; pid != uint32(104) {
				t.Fatalf("emitted detach INFO log for bpf_link prog %v (%T), want uint32(104)", pid, pid)
			}
		}
	}
}

func TestCgroupCleanupEmitsNoDetachLogsWhenOnlyBPFLinksPresent(t *testing.T) {
	core, logs := observer.New(zap.InfoLevel)
	logger := zap.New(core)

	detached := 0
	deps := cgroupCleanupDeps{
		hasOtherOwner: func(uint64, map[ebpf.MapID]struct{}) (bool, error) { return false, nil },
		hasOtherProc:  func(int) bool { return false },
		queryPrograms: func(_ uintptr, at ebpf.AttachType) ([]attachedCgroupProg, error) {
			if at == ebpf.AttachCGroupSockOps {
				return []attachedCgroupProg{{id: 201, linkID: 0}}, nil
			}
			return nil, nil
		},
		linkedPrograms: func(uint64) (cgroupLinkSnapshot, error) {
			return cgroupLinkSnapshot{
				exact: map[cgroupAttachKey]bool{
					{attachType: ebpf.AttachCGroupSockOps, progID: 201}: true,
				},
			}, nil
		},
		progNameByID: func(ebpf.ProgramID) (string, error) { return "k_sockops", nil },
		detachProgram: func(uintptr, ebpf.ProgramID, ebpf.AttachType) error {
			detached++
			return nil
		},
		selfPID: 1,
	}

	got := cleanOrphanedCgroupProgramsWithDeps(logger, 99, "/sys/fs/cgroup", 42, nil, deps)
	if got != 0 || detached != 0 {
		t.Fatalf("got cleaned=%d detached=%d, want 0", got, detached)
	}
	if logs.Len() != 0 {
		t.Fatalf("expected 0 INFO logs when only bpf_link programs are present, got %d: %v", logs.Len(), logs.All())
	}
}

func TestCgroupCleanupFailsClosedOnLinkedProgramsError(t *testing.T) {
	detached := 0
	deps := cgroupCleanupDeps{
		hasOtherOwner: func(uint64, map[ebpf.MapID]struct{}) (bool, error) { return false, nil },
		hasOtherProc:  func(int) bool { return false },
		queryPrograms: func(_ uintptr, at ebpf.AttachType) ([]attachedCgroupProg, error) {
			if at == ebpf.AttachCGroupSockOps {
				return []attachedCgroupProg{{id: 301}}, nil
			}
			return nil, nil
		},
		linkedPrograms: func(uint64) (cgroupLinkSnapshot, error) {
			return cgroupLinkSnapshot{}, errors.New("EPERM")
		},
		progNameByID: func(ebpf.ProgramID) (string, error) { return "k_sockops", nil },
		detachProgram: func(uintptr, ebpf.ProgramID, ebpf.AttachType) error {
			detached++
			return nil
		},
		selfPID: 1,
	}

	got := cleanOrphanedCgroupProgramsWithDeps(zap.NewNop(), 99, "/sys/fs/cgroup", 42, nil, deps)
	if got != 0 || detached != 0 {
		t.Fatalf("got cleaned=%d detached=%d, want 0 (fail-closed) when linkedPrograms errors", got, detached)
	}
}

func TestCgroupCleanupPostQueryRecheckPreventsConcurrentStartupDetach(t *testing.T) {
	for _, tc := range []struct {
		name           string
		ownerSecond    bool
		ownerSecondErr error
		procSecond     bool
	}{
		{name: "owner_appeared_mid_scan", ownerSecond: true},
		{name: "owner_check_errored_mid_scan", ownerSecondErr: errors.New("EPERM")},
		{name: "proc_appeared_mid_scan", procSecond: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ownerCalls := 0
			procCalls := 0
			detached := 0
			deps := cgroupCleanupDeps{
				hasOtherOwner: func(uint64, map[ebpf.MapID]struct{}) (bool, error) {
					ownerCalls++
					if ownerCalls == 1 {
						return false, nil
					}
					return tc.ownerSecond, tc.ownerSecondErr
				},
				hasOtherProc: func(int) bool {
					procCalls++
					if procCalls == 1 {
						return false
					}
					return tc.procSecond
				},
				queryPrograms: func(_ uintptr, at ebpf.AttachType) ([]attachedCgroupProg, error) {
					if at == ebpf.AttachCGroupSockOps {
						return []attachedCgroupProg{{id: 401}}, nil
					}
					return nil, nil
				},
				linkedPrograms: func(uint64) (cgroupLinkSnapshot, error) {
					return cgroupLinkSnapshot{}, nil
				},
				progNameByID: func(ebpf.ProgramID) (string, error) { return "k_sockops", nil },
				detachProgram: func(uintptr, ebpf.ProgramID, ebpf.AttachType) error {
					detached++
					return nil
				},
				selfPID: 1,
			}

			got := cleanOrphanedCgroupProgramsWithDeps(zap.NewNop(), 99, "/sys/fs/cgroup", 42, nil, deps)
			if got != 0 || detached != 0 {
				t.Fatalf("got cleaned=%d detached=%d, want 0 when concurrent agent appeared before detach", got, detached)
			}
			if ownerCalls < 2 {
				t.Fatalf("expected post-query hasOtherOwner re-check (calls=%d)", ownerCalls)
			}
		})
	}
}

func TestCgroupCleanupDetachesTrulyOrphanedLegacyPrograms(t *testing.T) {
	detached := map[ebpf.ProgramID]ebpf.AttachType{}
	deps := cgroupCleanupDeps{
		hasOtherOwner: func(uint64, map[ebpf.MapID]struct{}) (bool, error) { return false, nil },
		hasOtherProc:  func(int) bool { return false },
		queryPrograms: func(_ uintptr, at ebpf.AttachType) ([]attachedCgroupProg, error) {
			switch at {
			case ebpf.AttachCGroupSockOps:
				return []attachedCgroupProg{{id: 501}, {id: 599}}, nil
			case ebpf.AttachCgroupInetSockRelease:
				// 502 = full BTF name k_app_conn_release; 503 = truncated 15-byte k_udp_sock_rele.
				return []attachedCgroupProg{{id: 502}, {id: 503}}, nil
			default:
				return nil, nil
			}
		},
		linkedPrograms: func(uint64) (cgroupLinkSnapshot, error) {
			return cgroupLinkSnapshot{}, nil
		},
		progNameByID: func(id ebpf.ProgramID) (string, error) {
			switch id {
			case 501:
				return "k_sockops", nil
			case 502:
				return "k_app_conn_release", nil
			case 503:
				return "k_udp_sock_rele", nil
			case 599:
				return "unrelated_prog", nil
			default:
				return "", errors.New("unknown")
			}
		},
		detachProgram: func(_ uintptr, id ebpf.ProgramID, at ebpf.AttachType) error {
			detached[id] = at
			return nil
		},
		selfPID: 1,
	}

	got := cleanOrphanedCgroupProgramsWithDeps(zap.NewNop(), 99, "/sys/fs/cgroup", 42, nil, deps)
	if got != 3 {
		t.Fatalf("cleaned = %d, want 3 (501, 502, 503)", got)
	}
	if _, ok := detached[599]; ok {
		t.Fatal("detached unrelated program 599")
	}
	for _, id := range []ebpf.ProgramID{501, 502, 503} {
		if _, ok := detached[id]; !ok {
			t.Fatalf("expected legacy orphan %d to be detached, detached=%v", id, detached)
		}
	}
}

func TestCleanOrphanedBPFPinsRespectsCrossPIDNSOwnerAndFailsClosed(t *testing.T) {
	dir := t.TempDir()
	pin := filepath.Join(dir, "keploy_sock_live")
	if err := os.WriteFile(pin, []byte("pin"), 0600); err != nil {
		t.Fatal(err)
	}
	patterns := []string{filepath.Join(dir, "keploy_sock*")}

	// 1. Live cross-PID-ns owner (hasOtherProc=false, hasOtherOwner=true): must preserve pin.
	cleanOrphanedBPFPinsWithDeps(zap.NewNop(), 1, func(int) bool { return false }, func() (bool, error) {
		return true, nil
	}, patterns)
	if _, err := os.Stat(pin); err != nil {
		t.Fatalf("pin removed while cross-PID-ns owner was alive: %v", err)
	}

	// 2. Owner inspection error: must fail closed and preserve pin.
	cleanOrphanedBPFPinsWithDeps(zap.NewNop(), 1, func(int) bool { return false }, func() (bool, error) {
		return false, errors.New("EPERM")
	}, patterns)
	if _, err := os.Stat(pin); err != nil {
		t.Fatalf("pin removed when owner inspection failed: %v", err)
	}

	// 3. No live owner anywhere: stale pin must be removed.
	cleanOrphanedBPFPinsWithDeps(zap.NewNop(), 1, func(int) bool { return false }, func() (bool, error) {
		return false, nil
	}, patterns)
	if _, err := os.Stat(pin); !os.IsNotExist(err) {
		t.Fatalf("stale pin was not removed when no owner existed: err=%v", err)
	}
}

func TestHasOtherKeployProcessesFailsClosedAndChecksOwnerMap(t *testing.T) {
	t.Run("owner map inspection error fails closed", func(t *testing.T) {
		got := hasOtherKeployProcessesWithDeps(1, func(int) bool { return false }, func() (bool, error) {
			return false, errors.New("EPERM: BPF_MAP_GET_NEXT_ID requires CAP_SYS_ADMIN")
		})
		if !got {
			t.Fatal("hasOtherKeployProcessesWithDeps returned false on owner map error, want true (fail closed)")
		}
	})

	t.Run("live cross-PID-ns owner map returns true", func(t *testing.T) {
		got := hasOtherKeployProcessesWithDeps(1, func(int) bool { return false }, func() (bool, error) {
			return true, nil
		})
		if !got {
			t.Fatal("hasOtherKeployProcessesWithDeps returned false with live owner map, want true")
		}
	})

	t.Run("no proc peer and no owner map returns false", func(t *testing.T) {
		got := hasOtherKeployProcessesWithDeps(1, func(int) bool { return false }, func() (bool, error) {
			return false, nil
		})
		if got {
			t.Fatal("hasOtherKeployProcessesWithDeps returned true with no proc peer and no owner map, want false")
		}
	})

	t.Run("proc peer returns true without querying owner map", func(t *testing.T) {
		ownerQueried := false
		got := hasOtherKeployProcessesWithDeps(42, func(pid int) bool { return pid == 42 }, func() (bool, error) {
			ownerQueried = true
			return false, nil
		})
		if !got {
			t.Fatal("hasOtherKeployProcessesWithDeps returned false with /proc peer, want true")
		}
		if ownerQueried {
			t.Fatal("expected /proc peer fast-path to return true without querying owner map")
		}
	})
}
