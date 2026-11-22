package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// The agent's residency line says how much of the on-disk store it keeps in
// memory beside how much is in the store's file: diskBytes alone reads 0 for a
// recording that fits the store's resident budget, though every byte of it is
// then in the agent's memory. The line's two counts are the store's own, and
// together they hold every record the stream parked, a big answer kept apart
// from its mock included.
func TestTheResidencyLineSaysWhatTheStoreKeepsInMemory(t *testing.T) {
	t.Setenv("KEPLOY_STRICT_MOCK_WINDOW", "1")
	base := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	a := strictAgent(true)
	core, logs := observer.New(zap.InfoLevel)
	a.logger = zap.New(core)

	big := &models.Mock{Name: "pt-big", Kind: models.HTTP, Spec: models.MockSpec{
		Metadata:         map[string]string{models.MetadataLifetimeKey: models.MetadataLifetimePerTest},
		ReqTimestampMock: base.Add(2 * time.Second), ResTimestampMock: base.Add(2*time.Second + time.Millisecond),
		HTTPReq:  &models.HTTPReq{Method: "GET", URL: "http://api/big"},
		HTTPResp: &models.HTTPResp{StatusCode: 200, Body: strings.Repeat("b", 64<<10)},
	}}
	filtered := []*models.Mock{diskEligibleMock("pt-1", base.Add(time.Second)), big}
	header, dec := readHeaderAndDecoder(t, encodeStreamBody(t, filtered, nil))
	if err := a.StoreMocksStream(context.Background(), header, dec); err != nil {
		t.Fatalf("StoreMocksStream: %v", err)
	}
	st := loadStorage(t, a)
	if st.diskMocks == nil {
		t.Fatal("strict mode: expected an on-disk store")
	}
	t.Cleanup(func() { _ = st.diskMocks.Close() })
	if st.diskMocks.SpilledResponses() != 1 {
		t.Fatalf("the big answer was kept apart from %d mocks, want 1", st.diskMocks.SpilledResponses())
	}

	lines := logs.FilterMessage("agent mock residency: per-test mocks parked on disk (windowed)").All()
	if len(lines) != 1 {
		t.Fatalf("got %d residency lines, want 1", len(lines))
	}
	fields := lines[0].ContextMap()
	resident, ok := fields["residentBytes"].(int64)
	if !ok {
		t.Fatalf("the residency line has no residentBytes: %v", fields)
	}
	onDisk, _ := fields["diskBytes"].(int64)
	if resident != st.diskMocks.ResidentBytes() || onDisk != st.diskMocks.DiskBytes() {
		t.Fatalf("the line says %d resident and %d on disk, the store holds %d and %d", resident, onDisk, st.diskMocks.ResidentBytes(), st.diskMocks.DiskBytes())
	}
	// Both records fit the resident budget, so the store's file holds none of
	// them and its memory holds them all, the 64 KiB answer among them.
	if onDisk != 0 || resident < 64<<10 {
		t.Fatalf("the line says %d resident and %d on disk: want every record, the 64 KiB answer among them, in memory", resident, onDisk)
	}
}
