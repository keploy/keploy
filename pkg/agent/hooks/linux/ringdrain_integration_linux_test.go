//go:build linux && pidmap_integration

package linux

import (
	"context"
	"errors"
	"runtime"
	"sync/atomic"
	"testing"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/asm"
	"github.com/cilium/ebpf/ringbuf"
	"github.com/cilium/ebpf/rlimit"
)

// ringProducer is a real BPF ring and a program that writes one 8-byte
// report to it per run (BPF_PROG_TEST_RUN): the kernel's side of a watch.
func ringProducer(t *testing.T) (*ringbuf.Reader, func()) {
	t.Helper()
	if err := rlimit.RemoveMemlock(); err != nil {
		t.Fatal(err)
	}
	rb, err := ebpf.NewMap(&ebpf.MapSpec{Type: ebpf.RingBuf, MaxEntries: 4096})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rb.Close() })
	prog, err := ebpf.NewProgram(&ebpf.ProgramSpec{
		Type:    ebpf.SocketFilter,
		License: "GPL",
		Instructions: asm.Instructions{
			asm.Mov.Imm(asm.R1, 42),
			asm.StoreMem(asm.RFP, -8, asm.R1, asm.DWord),
			asm.LoadMapPtr(asm.R1, rb.FD()),
			asm.Mov.Reg(asm.R2, asm.RFP),
			asm.Add.Imm(asm.R2, -8),
			asm.Mov.Imm(asm.R3, 8),
			asm.Mov.Imm(asm.R4, 0),
			asm.FnRingbufOutput.Call(),
			asm.Mov.Imm(asm.R0, 0),
			asm.Return(),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = prog.Close() })
	rd, err := ringbuf.NewReader(rb)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rd.Close() })
	produce := func() {
		t.Helper()
		if _, err := prog.Run(&ebpf.RunOptions{Data: make([]byte, 64)}); err != nil {
			t.Fatal(err)
		}
	}
	return rd, produce
}

// A drain whose ctx ended leaves its flush behind. The watch gets through
// it (ErrFlushed) but has not said so yet (a preempted goroutine) when the
// application makes a call and the next read's drain begins: that drain must
// not take the old flush's answer for its own. It waits the old answer out,
// then flushes, and returns only once the watch got through its own flush,
// the call's report handled. On a real BPF ring.
func TestAnAbandonedDrainsFlushIsNotTheNextDrainsAnswer(t *testing.T) {
	if !inOwnPIDNamespace(t) {
		return
	}
	rd, produce := ringProducer(t)
	d := newReportDrain()
	var flushes atomic.Int32
	flush := func() error { flushes.Add(1); return rd.Flush() }

	// 1. A drain its caller gave up on (ctx): its flush stays behind.
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.wait(ctx, flush) }()
	waitBlocked(t, d, done)
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("abandoned drain = %v", err)
	}

	// 2. The watch's read: that flush, the (empty) ring to its end,
	//    ErrFlushed; it has not called flushed() yet.
	if _, err := rd.Read(); !errors.Is(err, ringbuf.ErrFlushed) {
		t.Fatalf("watch's read = %v, want ErrFlushed", err)
	}

	// 3. The application's call: its report is in the ring.
	produce()
	if rd.AvailableBytes() == 0 {
		t.Fatal("no report in the ring")
	}

	// 4. The next read's drain waits for the old flush's answer before it
	//    flushes its own.
	done = make(chan error, 1)
	go func() { done <- d.wait(context.Background(), flush) }()
	waitBlocked(t, d, done)
	if n := flushes.Load(); n != 1 {
		t.Fatalf("%d flushes before the abandoned one was answered, want 1: the next drain flushed on top of it, and the old answer would end it", n)
	}

	// 5. The watch says it got through the old flush: no answer to the
	//    new drain, which now flushes its own.
	d.flushed()
	for flushes.Load() != 2 {
		select {
		case err := <-done:
			t.Fatalf("FALSE GREEN: the drain returned (%v) on the old flush's answer, the call's report unread (%d bytes)", err, rd.AvailableBytes())
		default:
		}
		runtime.Gosched()
	}

	// 6. The watch reads on: the call's report, then the new flush's
	//    ErrFlushed; only then does the drain return.
	if _, err := rd.Read(); err != nil {
		t.Fatalf("the call's report: %v", err)
	}
	if _, err := rd.Read(); !errors.Is(err, ringbuf.ErrFlushed) {
		t.Fatalf("watch's read = %v, want the new flush's ErrFlushed", err)
	}
	select {
	case err := <-done:
		t.Fatalf("the drain returned (%v) before the watch said it got through its flush", err)
	default:
	}
	d.flushed()
	if err := <-done; err != nil {
		t.Fatalf("the drain = %v", err)
	}
}
