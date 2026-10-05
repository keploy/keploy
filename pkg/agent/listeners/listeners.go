package listeners

import "sync"

var (
	mu     sync.Mutex
	owners = map[uint16]uint32{}
)

func Note(pid uint32, port uint16) {
	mu.Lock()
	owners[port] = pid
	mu.Unlock()
}

func Owner(port uint16) (uint32, bool) {
	mu.Lock()
	defer mu.Unlock()
	pid, ok := owners[port]
	return pid, ok
}
