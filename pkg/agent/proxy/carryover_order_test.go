package proxy

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// TestCarryOverPoolHoldsTiedMocksInRecordedNumberOrder: mocks of one request
// time (the reads of one write) are held in the order mockdb numbered them
// as it wrote them, mock-2 before mock-10, whatever order they are filed in,
// and names with no number after them, by name.
func TestCarryOverPoolHoldsTiedMocksInRecordedNumberOrder(t *testing.T) {
	at := time.Unix(1_700_000_000, 0)
	const n = 12
	var filed, want []string
	var cands []*models.Mock
	for i := n - 1; i >= 0; i-- {
		name := fmt.Sprintf("mock-%d", i)
		cands = append(cands, &models.Mock{Name: name, Kind: models.HTTP, Spec: models.MockSpec{ReqTimestampMock: at}})
		filed = append(filed, name)
	}
	// Names with no number, filed out of order: by name, after the
	// numbered ones.
	for _, name := range []string{"custom-b", "custom-a"} {
		cands = append(cands, &models.Mock{Name: name, Kind: models.HTTP, Spec: models.MockSpec{ReqTimestampMock: at}})
		filed = append(filed, name)
	}
	for i := 0; i < n; i++ {
		want = append(want, fmt.Sprintf("mock-%d", i))
	}
	want = append(want, "custom-a", "custom-b")
	p := newCarryOverPool()
	// Two filings, so both the sort of one filing and the insert into what
	// the pool holds are read.
	half := len(cands) / 2
	p.file(cands[:half], nil)
	p.file(cands[half:], nil)
	var got []string
	for _, mk := range p.snapshot("") {
		got = append(got, mk.Name)
	}
	if g, w := strings.Join(got, ","), strings.Join(want, ","); g != w {
		t.Fatalf("filed %s, held %s; want %s", strings.Join(filed, ","), g, w)
	}
}
