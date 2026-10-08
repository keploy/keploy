//go:build linux || darwin

package utils

import "fmt"

// procTable is one snapshot of the process table: each process's parent and
// its process group, as the kernel had them when the snapshot was read. Each
// platform reads its own (proctree_linux.go, proctree_darwin.go); the tree
// InterruptProcessTree signals, and the group of each process in it, are then
// taken from the snapshot here, by one rule for both.
//
// The whole table is read once. The tree and the groups are not read process
// by process as the walk finds them: on Linux that read every process's
// status file once for each process found in the tree, and again for each
// one's group, all before the first signal, and on a host short of memory
// each of those reads waits on reclaim. The application went unsignalled for
// as long as that took.
type procTable struct {
	parents map[int]int // pid -> parent pid
	groups  map[int]int // pid -> process group id
}

func newProcTable(size int) procTable {
	return procTable{parents: make(map[int]int, size), groups: make(map[int]int, size)}
}

func (t procTable) add(pid, ppid, pgid int) {
	t.parents[pid] = ppid
	t.groups[pid] = pgid
}

// tree returns every descendant of root, and how to read each one's process
// group, both from the snapshot.
//
// The groups come from the snapshot, not from a lookup made later. A process
// that exited after the snapshot was read, or a zombie that getpgid(2)
// refuses with ESRCH, is still in the tree, and its group is still known. A
// pid the snapshot did not hold is an error, never a group of 0: signalled as
// -0, that is keploy's own process group.
func (t procTable) tree(root int) ([]int, func(pid int) (int, error)) {
	groupOf := func(pid int) (int, error) {
		pgid, ok := t.groups[pid]
		if !ok {
			return 0, fmt.Errorf("process %d is not in the process table", pid)
		}
		return pgid, nil
	}
	return descendantsOf(root, t.parents), groupOf
}

// descendantsOf returns every descendant of root in a pid -> parent pid table,
// nearest first. The kernel's table is a tree except at its very top
// (kernel_task is its own parent on macOS), and each pid is still visited only
// once however the table loops.
func descendantsOf(root int, parents map[int]int) []int {
	children := make(map[int][]int, len(parents))
	for pid, ppid := range parents {
		children[ppid] = append(children[ppid], pid)
	}
	var descendants []int
	seen := map[int]bool{root: true}
	for queue := []int{root}; len(queue) > 0; queue = queue[1:] {
		for _, child := range children[queue[0]] {
			if seen[child] {
				continue
			}
			seen[child] = true
			descendants = append(descendants, child)
			queue = append(queue, child)
		}
	}
	return descendants
}
