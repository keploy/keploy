//go:build linux

package linux

import (
	"reflect"

	"github.com/cilium/ebpf"
	agentSvc "go.keploy.io/server/v3/pkg/service/agent"
)

// lookupMap resolves an eBPF map by its bpffs name (the `ebpf` struct tag).
// Returns nil if the name is not found.
func (o *bpfObjects) lookupMap(name string) agentSvc.Pinnable {
	v := reflect.ValueOf(&o.bpfMaps).Elem()
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		if t.Field(i).Tag.Get("ebpf") == name {
			m, _ := v.Field(i).Interface().(*ebpf.Map)
			return m
		}
	}
	return nil
}
