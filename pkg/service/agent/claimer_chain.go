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
