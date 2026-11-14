package utils

import (
	"errors"
	"testing"
)

// PortTakenError tags an error as ErrPortTaken and keeps its message; a start
// the agent fails with it says so in its report (BootProgress.PortTaken), and
// one it fails with anything else does not.
func TestAStartFailedOnATakenPortSaysSoInItsReport(t *testing.T) {
	if PortTakenError(nil) != nil {
		t.Fatal("PortTakenError(nil) is not nil")
	}
	inner := errors.New("the proxy's port 16789 is taken by pid 1 (x)")
	err := PortTakenError(inner)
	if !errors.Is(err, ErrPortTaken) || !errors.Is(err, inner) || err.Error() != inner.Error() {
		t.Fatalf("PortTakenError = %v; want ErrPortTaken wrapping %q, saying it as it is", err, inner)
	}
	for _, c := range []struct {
		err  error
		want bool
	}{{err, true}, {inner, false}} {
		resetBoot(t)
		FailBoot(c.err)
		if p := snapshotBoot(0, false); !p.Failed || p.PortTaken != c.want || p.Failure != c.err.Error() {
			t.Fatalf("report after FailBoot(%v) = %+v; want failed, PortTaken %v", c.err, p, c.want)
		}
	}
}

// randomOffset draws over the whole band: every offset of a small one comes
// up, the top one too, and none past it. A band drawn narrower makes the
// draws of processes started together meet more often.
func TestRandomOffsetCoversTheBand(t *testing.T) {
	const span = 8
	seen := map[uint32]int{}
	for i := 0; i < 4000; i++ {
		off, ok := randomOffset(span)
		if !ok || off >= span {
			t.Fatalf("randomOffset(%d) = %d, %v; want an offset below %d", span, off, ok, span)
		}
		seen[off]++
	}
	for off := uint32(0); off < span; off++ {
		if seen[off] == 0 {
			t.Fatalf("randomOffset(%d) never drew %d in 4000 draws: %v", span, off, seen)
		}
	}
}
