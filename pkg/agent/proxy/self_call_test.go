package proxy

import (
	"os"

	"go.uber.org/zap"
)

// selfCallProxy is a replay proxy whose application is this test process.
// It builds on every OS: the tree-listener tests that use it are Linux-only
// (self_call_linux_test.go, topology_linux_test.go), but
// recorded_ports_store_test.go builds everywhere and uses it too.
func selfCallProxy() *Proxy {
	return &Proxy{mockMode: true, appPID: uint32(os.Getpid()), logger: zap.NewNop()}
}
