//go:build linux

package proxy

import (
	"context"
	"time"

	"github.com/cilium/ebpf"
	"go.uber.org/zap"
)

// netioBytesPinPath mirrors the hooks loader's auto-pin location for
// keploy_netio_bytes (LIBBPF_PIN_BY_NAME under /sys/fs/bpf). Kept as a literal to
// avoid a proxy→hooks import edge.
const netioBytesPinPath = "/sys/fs/bpf/keploy_netio_bytes"

const netioDrainInterval = 15 * time.Second

type netioVal struct{ Rx, Tx uint64 }

// StartKernelNetioDrain drains the kernel keploy_netio_bytes counter (per-cgroup
// app rx/tx, tallied in-kernel at tcp_sendmsg/tcp_recvmsg) and folds per-interval
// deltas into networkIOSink, which carries them into the usage-metering footprint.
// The map is opened lazily by its bpffs pin, so this stays inert until the netio
// programs are loaded (or forever, on a kernel without BTF — where no network I/O
// is metered, since the userspace TCP_INFO fallback has been removed).
func StartKernelNetioDrain(ctx context.Context, logger *zap.Logger) {
	go func() {
		t := time.NewTicker(netioDrainInterval)
		defer t.Stop()
		var m *ebpf.Map
		last := map[uint64]netioVal{}
		defer func() {
			if m != nil {
				_ = m.Close()
			}
		}()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if m == nil {
					opened, err := ebpf.LoadPinnedMap(netioBytesPinPath, nil)
					if err != nil {
						continue // map not pinned yet / netio disabled: stay inert
					}
					m = opened
				}
				rxDelta, txDelta, ok := drainKernelNetio(m, last)
				if !ok {
					// Stale handle (map re-pinned). Drop and re-open next tick.
					_ = m.Close()
					m = nil
					last = map[uint64]netioVal{}
					continue
				}
				if rxDelta == 0 && txDelta == 0 {
					continue
				}
				if sinkP := networkIOSink.Load(); sinkP != nil {
					(*sinkP)(rxDelta, txDelta)
				}
			}
		}
	}()
}

// drainKernelNetio sums each cgroup's per-CPU rx/tx and returns the total NEW
// bytes since the last drain (forward-delta, reset-safe), and ok=false on a
// stale-handle iteration error.
func drainKernelNetio(m *ebpf.Map, last map[uint64]netioVal) (rxDelta, txDelta uint64, ok bool) {
	var (
		key    uint64
		perCPU []netioVal
	)
	cur := make(map[uint64]netioVal)
	it := m.Iterate()
	for it.Next(&key, &perCPU) {
		var v netioVal
		for _, c := range perCPU {
			v.Rx += c.Rx
			v.Tx += c.Tx
		}
		cur[key] = v
		p := last[key]
		rxDelta += forwardDeltaU64(v.Rx, p.Rx)
		txDelta += forwardDeltaU64(v.Tx, p.Tx)
	}
	if err := it.Err(); err != nil {
		return 0, 0, false
	}
	// Adopt the fresh snapshot; cgroups that vanished were already counted.
	for k := range last {
		if _, present := cur[k]; !present {
			delete(last, k)
		}
	}
	for k, v := range cur {
		last[k] = v
	}
	return rxDelta, txDelta, true
}

// forwardDeltaU64 returns cur-prev, or the full cur when the counter went
// backwards (a freed+recycled cgroup slot), so a reset never subtracts.
func forwardDeltaU64(cur, prev uint64) uint64 {
	if cur >= prev {
		return cur - prev
	}
	return cur
}
