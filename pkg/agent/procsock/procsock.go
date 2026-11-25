// Package procsock reads a process's TCP sockets from its own descriptor
// table, through macOS's proc_info system call (what libproc's proc_pidinfo
// and proc_pidfdinfo wrap): their ends, TCP state and what their send buffers
// hold. It lists the processes, and reads each one's parent and process
// group, through the same call. A process of keploy's own user may read another of that user's
// processes this way where macOS's global TCP table (net.inet.tcp.pcblist64)
// lists it only its own sockets. It reads nothing on other platforms.
package procsock
