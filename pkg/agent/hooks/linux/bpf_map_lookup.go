//go:build linux

package linux

import (
	"reflect"
	"sync"

	"github.com/cilium/ebpf"
	"go.keploy.io/server/v3/pkg/agent"
)

var (
	bpfMapTagIndexOnce sync.Once
	bpfMapTagIndex     map[string]int
)

func initBPFMapTagIndex() {
	bpfMapTagIndex = make(map[string]int)
	t := reflect.TypeOf(bpfMaps{})
	for i := 0; i < t.NumField(); i++ {
		tag := t.Field(i).Tag.Get("ebpf")
		if tag != "" {
			bpfMapTagIndex[tag] = i
		}
	}
}

// lookupMap resolves an eBPF map by its bpffs name (the `ebpf` struct tag).
// Returns nil if the name is not found.
func (o *bpfObjects) lookupMap(name string) agent.Pinnable {
	bpfMapTagIndexOnce.Do(initBPFMapTagIndex)
	idx, ok := bpfMapTagIndex[name]
	if !ok {
		return nil
	}

	v := reflect.ValueOf(&o.bpfMaps).Elem()
	m, _ := v.Field(idx).Interface().(*ebpf.Map)
	return m
}
