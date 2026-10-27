package learn

import (
	"fmt"
	"testing"
	"time"
)

// The learn step looks for where a changed clock read came from in every
// payload served earlier in the set (appearedIn), and a set grows with the
// round. Whether a payload holds the read whole is asked of the payload's own
// clock reads only where the read stands in it as a word: reading the clock
// layouts of every earlier payload for each changed value made each call's
// learn step cost grow with the round (api-server's full round: up to 10.6 s
// of a 30 s profile window). So 100 or 2000 earlier payloads that do not hold
// the value read the same one payload's layouts, once, though the value
// stands in it twice (first inside a read of more precision, then whole),
// and the place found is the same.
func TestAWholeClockReadIsLookedForInOnlyThePayloadsThatHoldIt(t *testing.T) {
	at := time.Date(2026, 10, 14, 10, 0, 42, 0, time.UTC)
	read := clockSpans
	defer func() { clockSpans = read }()
	var scanned []string
	clockSpans = func(text string) [][]int {
		scanned = append(scanned, text)
		return read(text)
	}
	const holds = `{"rows":[["41","2026-10-14 10:00:41.5"],["42","2026-10-14 10:00:41"]]}`
	for _, n := range []int{100, 2000} {
		var raw []Raw
		for i := 0; i < n; i++ {
			raw = append(raw, Raw{rRead: true, mock: fmt.Sprintf("mock-%d", i), at: at.Add(-2 * time.Second), served: true,
				rText: fmt.Sprintf(`{"rows":[["%d","2026-09-01 08:00:%02d"]]}`, i, i%60)})
		}
		raw = append(raw, Raw{rRead: true, mock: "mock-holds", at: at.Add(-time.Second), served: true, rText: holds})
		scanned = nil
		p, ok := appeared(clockedDrifts(), "2026-10-14 10:00:41", "2026-10-14 10:01:42", at, nil, raw)
		if !ok || p.Mock != "mock-holds" {
			t.Fatalf("%d earlier payloads: found %v at %q, want the payload that holds the read whole", n, ok, p.Mock)
		}
		if len(scanned) != 1 || scanned[0] != holds {
			t.Errorf("%d earlier payloads: the clock layouts were read of %d texts, want only the one payload that holds the read, once", n, len(scanned))
		}
	}
}
