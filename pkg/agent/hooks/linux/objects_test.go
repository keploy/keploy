//go:build linux

package linux

import (
	"path/filepath"
	"testing"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/asm"
)

// TestNoObjectCallsTracePrintk fails when an object the agent embeds (the
// hooks and unixconn objects copied from keploy/ebpf, both architectures)
// calls bpf_trace_printk or bpf_trace_vprintk. It reads the objects' ELF
// only, so it needs no privileges and runs on any kernel.
//
// A print costs every load and every run. On kernels without upstream
// 4580f4e0ebdf (no 5.10.y or 5.15.y release has it) the verifier takes the
// host's global event_mutex for each print call it verifies
// (trace_set_clr_event), and every perf-event tracepoint, kprobe and uprobe
// attach and detach on the host holds that mutex, the detach of an event's
// last perf reference across an RCU grace period: on CI's 5.10 hosts the
// agent's load queued behind other agents for over a minute. At run time
// each call writes the host's shared trace buffer (on 5.10.y under a global
// spinlock with interrupts off), and sys_enter_socket printed on every
// socket() on the host. keploy/ebpf builds its debug prints in only with
// -DKEPLOY_BPF_DEBUG (keploy_debug, compile.sh BPF_DEBUG=1); an object built
// that way must not be copied here.
func TestNoObjectCallsTracePrintk(t *testing.T) {
	objects, err := filepath.Glob("*_bpfel.o")
	if err != nil {
		t.Fatal(err)
	}
	// The objects the agent embeds: a glob that matched fewer would check
	// fewer and still pass.
	found := map[string]bool{}
	for _, path := range objects {
		found[path] = true
	}
	for _, want := range []string{"bpf_x86_bpfel.o", "bpf_arm64_bpfel.o", "unixconn_x86_bpfel.o", "unixconn_arm64_bpfel.o"} {
		if !found[want] {
			t.Errorf("%s is not here to check (found %v)", want, objects)
		}
	}
	for _, path := range objects {
		spec, err := ebpf.LoadCollectionSpec(path)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if len(spec.Programs) == 0 {
			t.Errorf("%s: no programs: nothing was checked", path)
		}
		for name, prog := range spec.Programs {
			// A program's instructions include the subprograms it calls,
			// so a print in a shared helper is found in each caller.
			for i, ins := range prog.Instructions {
				if !ins.IsBuiltinCall() {
					continue
				}
				switch fn := asm.BuiltinFunc(ins.Constant); fn {
				case asm.FnTracePrintk, asm.FnTraceVprintk:
					t.Errorf("%s: program %s calls %v at instruction %d: copy objects keploy/ebpf built without KEPLOY_BPF_DEBUG",
						path, name, fn, i)
				}
			}
		}
	}
}
