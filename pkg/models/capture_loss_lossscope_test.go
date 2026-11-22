package models

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// Two reads of losses counted from one base, taken in either order (the ends
// of parallel tests' reads), are as much as the later one: each kind with
// its larger count, sorted by kind.
func TestCaptureLossesMaxKeepsTheLargerCountOfEachKind(t *testing.T) {
	pid := func(n uint64) CaptureLoss { return CaptureLoss{Kind: CaptureLossPIDMap, Count: n} }
	tls := CaptureLoss{Kind: CaptureLossTLSAbort, Count: 1}
	require.Equal(t, CaptureLosses{pid(3), tls}, CaptureLosses{pid(3)}.Max(CaptureLosses{pid(2), tls}))
	require.Equal(t, CaptureLosses{pid(4)}, CaptureLosses{pid(1)}.Max(CaptureLosses{pid(4)}))
	require.Equal(t, CaptureLosses{pid(2)}, CaptureLosses(nil).Max(CaptureLosses{pid(2)}))
	require.Empty(t, CaptureLosses(nil).Max(nil))
}
