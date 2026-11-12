//go:build linux

package agent

import (
	"context"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// Without the proven switch, a staging points the proxy's reads of the set's
// recordings of a kind at the on-disk store: a parser holding a call while
// the set still holds an unserved recording of it finds one a later test
// owns, which the staged window does not hold.
func TestAStagingWithoutTheSwitchReadsALaterTestsRecordingFromDisk(t *testing.T) {
	t.Setenv(provenswitch.Env, "0")
	t.Setenv("KEPLOY_STRICT_MOCK_WINDOW", "1")
	t.Cleanup(models.RegisterAnswerPlaced(models.Mongo, func(*models.Mock) bool { return false }))
	base := time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC)
	a := strictAgent(true)
	p := &diskServingProxy{servingProxy: newServingProxy()}
	t.Cleanup(p.mm.Close)
	a.Proxy = p
	filtered := []*models.Mock{diskEligibleMock("pt-1", base.Add(time.Second)), diskEligibleMock("pt-2", base.Add(10*time.Second))}
	header, dec := readHeaderAndDecoder(t, encodeStreamBody(t, filtered, nil))
	if err := a.StoreMocksStream(context.Background(), header, dec); err != nil {
		t.Fatalf("StoreMocksStream: %v", err)
	}
	st := loadStorage(t, a)
	if st.diskMocks == nil {
		t.Fatal("strict mode: expected an on-disk store")
	}
	t.Cleanup(func() { _ = st.diskMocks.Close() })
	params := models.MockFilterParams{AfterTime: base, BeforeTime: base.Add(2 * time.Second), StrictMockWindow: true}
	if err := a.stageMocks(context.Background(), params, nil, nil); err != nil {
		t.Fatalf("stage the first test's window: %v", err)
	}
	got, err := p.mm.HasUnservedRecording(models.Mongo, func(m *models.Mock) bool { return m.Name == "pt-2" })
	if err != nil || !got {
		t.Fatalf("the later test's recording on disk was found %v (%v), want found", got, err)
	}
}
