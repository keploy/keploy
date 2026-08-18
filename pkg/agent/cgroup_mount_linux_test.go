//go:build linux && cgroupmount_integration

// This integration test exercises the real mount(2) fallback that keploy uses
// on a legacy cgroup v1 host. It needs root/CAP_SYS_ADMIN and is therefore
// excluded from the normal `go test ./...` unit run by the cgroupmount_integration
// build tag; the e2e-cgroup-v2-selfmount workflow builds it with that tag and
// runs the resulting binary as root.

package agent

import (
	"os"
	"testing"

	"go.uber.org/zap"
	"golang.org/x/sys/unix"
)

// cgroup2SuperMagic is CGROUP2_SUPER_MAGIC from <linux/magic.h>; statfs reports
// it for a genuine cgroup2 mount.
const cgroup2SuperMagic = 0x63677270

// TestCgroupV2SelfMount verifies the syscall self-mount path end to end:
//   - mountCgroupV2 succeeds and returns the keploy mount point,
//   - the mount point is a real cgroup2 filesystem (statfs magic + a readable
//     cgroup.controllers file, both hallmarks of a cgroup2 root),
//   - DetectCgroupPath is idempotent: with a cgroup2 mount now present it
//     returns a path without erroring or stacking another mount.
//
// It runs on a cgroup v2 CI host too: mounting an additional view of the always
// present kernel v2 hierarchy at a fresh path succeeds regardless of the host's
// default cgroup mode, which is exactly the operation the legacy-v1 fallback
// performs.
func TestCgroupV2SelfMount(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root/CAP_SYS_ADMIN to mount cgroup2")
	}

	// Clean slate in case a previous run left the mount behind.
	_ = unix.Unmount(keployCgroupV2Mountpoint, unix.MNT_DETACH)

	path, err := mountCgroupV2(zap.NewNop())
	if err != nil {
		t.Fatalf("mountCgroupV2: %v", err)
	}
	t.Cleanup(func() {
		if err := unix.Unmount(keployCgroupV2Mountpoint, unix.MNT_DETACH); err != nil {
			t.Logf("cleanup unmount %s: %v", keployCgroupV2Mountpoint, err)
		}
	})
	if path != keployCgroupV2Mountpoint {
		t.Fatalf("mountCgroupV2 returned %q, want %q", path, keployCgroupV2Mountpoint)
	}

	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		t.Fatalf("statfs(%s): %v", path, err)
	}
	if int64(st.Type) != cgroup2SuperMagic {
		t.Fatalf("statfs type = %#x, want cgroup2 magic %#x — %s is not a cgroup2 mount", st.Type, cgroup2SuperMagic, path)
	}
	if _, err := os.ReadFile(path + "/cgroup.controllers"); err != nil {
		t.Fatalf("reading cgroup.controllers from %s (hallmark of a cgroup2 root): %v", path, err)
	}

	// Idempotency: a cgroup2 mount now exists, so DetectCgroupPath must find one
	// and return without error. (On a v2 CI host it returns the host's
	// /sys/fs/cgroup, scanned before our /run mount — either way, no re-mount and
	// no error, which is the property under test.)
	found, err := DetectCgroupPath(zap.NewNop())
	if err != nil {
		t.Fatalf("DetectCgroupPath after mount: %v", err)
	}
	if found == "" {
		t.Fatal("DetectCgroupPath returned an empty path with a cgroup2 mount present")
	}
}
