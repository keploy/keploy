package utils

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// InitialPIDNSInode is the inode of the initial PID namespace's nsfs entry
// (PROC_PID_INIT_INO, include/linux/proc_ns.h), which the kernel fixes: the
// namespace whose process numbers keploy's eBPF programs report, a kernel
// pid (agent.NetworkAddress.KernelPid, OutgoingOptions.SrcPid).
const InitialPIDNSInode = 0xEFFFFFFC

// KernelPidsLocalIn reports whether a kernel pid names, in procRoot (this
// process's /proc), the process it names in the kernel: only when this
// process runs in the initial PID namespace, which its own namespace entry
// (procRoot/self/ns/pid) says. In a PID namespace of its own (a container
// without the host's, --pid=host or hostPID) /proc numbers processes as that
// namespace does, and a kernel pid there names another process, or none. It
// never reads the pid's own entry to tell: an entry by that number exists
// whenever the numbers collide. Where it cannot read the entry (no /proc, or
// not Linux, whose processes keploy reads otherwise: KernelPidsAreLocal) it
// is false.
func KernelPidsLocalIn(procRoot string) bool {
	link, err := os.Readlink(filepath.Join(procRoot, "self", "ns", "pid"))
	return err == nil && link == fmt.Sprintf("pid:[%d]", uint32(InitialPIDNSInode))
}

// kernelPidsLocal is KernelPidsLocalIn of this process's /proc, read once: a
// process never changes PID namespace.
var kernelPidsLocal = sync.OnceValue(func() bool { return KernelPidsLocalIn("/proc") })
