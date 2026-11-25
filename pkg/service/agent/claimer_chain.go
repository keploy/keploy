package agent

// descendsByCreation reports whether pid is ancestor or one of its
// descendants. It follows parent, and takes a parent only while it is a live
// process created no later than its child (created is a live process's
// creation time). That is how Windows is read: a process keeps the pid of a
// parent that has exited, and a later process can take that pid, but it was
// created after the child it would be read as the parent of. Equal times
// are taken: the system stamps a process's creation at its clock's tick.
// It is in no build of its own, so that it is tested on every system.
func descendsByCreation(pid, ancestor int, parent func(pid int) (int, bool), created func(pid int) (uint64, bool)) bool {
	if pid == ancestor {
		return true
	}
	born, alive := created(pid)
	if !alive {
		return false
	}
	for i := 0; i < 64; i++ {
		ppid, found := parent(pid)
		if !found || ppid <= 0 {
			return false
		}
		pborn, alive := created(ppid)
		if !alive || pborn > born {
			return false
		}
		if ppid == ancestor {
			return true
		}
		pid, born = ppid, pborn
	}
	return false
}

// directChild is child's creation time when it is a live process whose
// parent is parent, itself a live process created no later than it; false
// otherwise. parent and created are read as descendsByCreation reads them.
// The child's time is read first, so a child that exits while it is checked
// is answered with its own time, which the pid's next process does not have:
// the claim it makes is of a process that has exited. On Windows a process
// keeps the pid of a parent that has exited, and a later process can take
// that pid: one created after the child is not taken for its parent, but one
// created in the child's own clock tick is, as descendsByCreation takes equal
// times.
func directChild(child, parent int, parentOf func(pid int) (int, bool), created func(pid int) (uint64, bool)) (uint64, bool) {
	born, alive := created(child)
	if !alive {
		return 0, false
	}
	ppid, found := parentOf(child)
	if !found || ppid != parent {
		return 0, false
	}
	pborn, alive := created(parent)
	if !alive || pborn > born {
		return 0, false
	}
	return born, true
}
