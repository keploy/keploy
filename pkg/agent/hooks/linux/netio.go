//go:build linux

package linux

import (
	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/asm"
	"github.com/cilium/ebpf/btf"
	"github.com/cilium/ebpf/link"
	"go.uber.org/zap"
)

// NetioBytesPinPath is where keploy_netio_bytes (LIBBPF_PIN_BY_NAME) auto-pins,
// given CollectionOptions.Maps.PinPath = "/sys/fs/bpf". The userspace drain opens
// it here by path (see pkg/agent/proxy). Kept in sync with the map name in
// keploy_ebpf.c.
const (
	bpffsRoot         = "/sys/fs/bpf"
	NetioBytesPinPath = bpffsRoot + "/keploy_netio_bytes"
)

// stubProgram replaces a program's body with `return 0`, so LoadAndAssign accepts
// the spec without attaching it to a kernel function whose BTF signature it
// doesn't match. Used to drop the tcp_recvmsg fexit wrapper that doesn't match
// the running kernel's arg count (see selectNetioRecvmsgVariant).
func stubProgram(spec *ebpf.CollectionSpec, name string) {
	if p, ok := spec.Programs[name]; ok {
		p.Instructions = asm.Instructions{
			asm.Mov.Imm(asm.R0, 0),
			asm.Return(),
		}
	}
}

// tcpRecvmsgArgs returns the running kernel's tcp_recvmsg parameter count from
// BTF. tcp_recvmsg lost its `nonblock` param in v5.19 (commit ec095263a965):
// 6 params on < 5.19, 5 on >= 5.19. Defaults to 5 (modern) when BTF is
// unavailable or the type can't be read.
func tcpRecvmsgArgs(logger *zap.Logger) int {
	spec, err := btf.LoadKernelSpec()
	if err != nil {
		logger.Warn("netio: kernel BTF unavailable — assuming 5-arg tcp_recvmsg", zap.Error(err))
		return 5
	}
	var fn *btf.Func
	if err := spec.TypeByName("tcp_recvmsg", &fn); err != nil {
		logger.Warn("netio: tcp_recvmsg not in BTF — assuming 5-arg", zap.Error(err))
		return 5
	}
	proto, ok := fn.Type.(*btf.FuncProto)
	if !ok {
		return 5
	}
	return len(proto.Params)
}

// selectNetioRecvmsgVariant stubs whichever tcp_recvmsg fexit wrapper does NOT
// match the kernel's arg count, so LoadAndAssign accepts the spec. Returns the
// name of the wrapper that remains live (to be attached), or "" if neither fits.
func selectNetioRecvmsgVariant(spec *ebpf.CollectionSpec, logger *zap.Logger) string {
	switch tcpRecvmsgArgs(logger) {
	case 6: // < 5.19: keep the _v6 (nonblock) wrapper
		stubProgram(spec, "keploy_netio_tcp_recvmsg")
		return "keploy_netio_tcp_recvmsg_v6"
	default: // >= 5.19: keep the 5-arg wrapper
		stubProgram(spec, "keploy_netio_tcp_recvmsg_v6")
		return "keploy_netio_tcp_recvmsg"
	}
}

// attachNetio attaches the app network-I/O counter programs: fexit/tcp_sendmsg
// (egress), the selected fexit/tcp_recvmsg variant (ingress), and the
// tp_btf/cgroup_rmdir eviction hook. Best-effort — network metering is optional,
// so any failure (older kernel, no BTF, verifier) is logged and the userspace
// TCP_INFO path remains the fallback. recvName is the live recvmsg wrapper from
// selectNetioRecvmsgVariant.
func (h *Hooks) attachNetio(objs *bpfObjects, recvName string, logger *zap.Logger) {
	if l, err := link.AttachTracing(link.TracingOptions{Program: objs.KeployNetioTcpSendmsg}); err != nil {
		logger.Warn("netio: attach fexit/tcp_sendmsg failed — app egress metering disabled (TCP_INFO fallback stays active)", zap.Error(err))
	} else {
		h.netioSend = l
	}

	recvProg := objs.KeployNetioTcpRecvmsg
	if recvName == "keploy_netio_tcp_recvmsg_v6" {
		recvProg = objs.KeployNetioTcpRecvmsgV6
	}
	if l, err := link.AttachTracing(link.TracingOptions{Program: recvProg}); err != nil {
		logger.Warn("netio: attach fexit/tcp_recvmsg failed — app ingress metering disabled", zap.Error(err))
	} else {
		h.netioRecv = l
	}

	if l, err := link.AttachTracing(link.TracingOptions{Program: objs.KeployNetioCgroupRmdir}); err != nil {
		logger.Warn("netio: attach tp_btf/cgroup_rmdir failed — dead-cgroup eviction disabled (map may grow on churn)", zap.Error(err))
	} else {
		h.netioRmdir = l
	}
}
