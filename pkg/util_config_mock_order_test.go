package pkg

import (
	"context"
	"fmt"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// configMockAt builds a reusable config-tier mock with DISTINCT request and
// response timestamps, so a sort keyed on the wrong one is detectable.
func configMockAt(name string, base time.Time, reqOffset, resOffset int) *models.Mock {
	return &models.Mock{
		Name:    name,
		Version: "api.keploy.io/v1beta1",
		Kind:    "Couchbase",
		Spec: models.MockSpec{
			Metadata:         map[string]string{"type": "config"},
			ReqTimestampMock: base.Add(time.Duration(reqOffset) * time.Second),
			ResTimestampMock: base.Add(time.Duration(resOffset) * time.Second),
		},
	}
}

func configMock(name string, base time.Time, offsetSec int) *models.Mock {
	// Response deliberately ordered OPPOSITE to request, so sorting by
	// ResTimestampMock produces a different order and cannot pass silently.
	return configMockAt(name, base, offsetSec, 1000-offsetSec)
}

// Mapping membership must not reorder the reusable pool: it records which tests
// consumed a mock, not when it was recorded. Downstream reads this pool as a
// sequence (slice order -> SortOrder -> RB-tree -> in-order walk), so inverting
// it hands an order-sensitive replayer its recorded sequence backwards.
func TestFilterConfigMocksMapping_PreservesRecordedOrder(t *testing.T) {
	base := time.Unix(1700000000, 0).UTC()
	pool := []*models.Mock{
		configMock("cfg-rev1", base, 1),
		configMock("cfg-rev2", base, 2),
		configMock("cfg-rev3", base, 3),
		configMock("cfg-rev4", base, 4),
	}
	mapping := []string{"cfg-rev3", "cfg-rev4"}

	got := FilterConfigMocksMapping(context.Background(), zap.NewNop(), pool, mapping)

	want := []string{"cfg-rev1", "cfg-rev2", "cfg-rev3", "cfg-rev4"}
	if names := mockNames(got); len(names) != len(want) {
		t.Fatalf("config pool = %v, want %v", names, want)
	}
	for i, w := range want {
		if got[i].Name != w {
			t.Fatalf("config pool = %v, want %v", mockNames(got), want)
		}
	}
}

// Ties are structural, not hypothetical: an encoder that drains several frames
// from one TCP read stamps them all with that chunk's timestamp, so a coalesced
// bootstrap burst arrives all-equal. Under equal timestamps the pool must keep
// its RECORDED order — a partition-then-stable-sort leaves ties in mapped-first
// order, which is the inversion this guards against.
func TestFilterConfigMocksMapping_EqualTimestampsKeepRecordedOrder(t *testing.T) {
	base := time.Unix(1700000000, 0).UTC()
	pool := []*models.Mock{
		configMockAt("hello", base, 5, 5),
		configMockAt("sasl-auth", base, 5, 5),
		configMockAt("select-bucket", base, 5, 5),
		configMockAt("cluster-config", base, 5, 5),
	}
	// The LAST-recorded frame is the one a test consumed.
	mapping := []string{"cluster-config"}

	got := FilterConfigMocksMapping(context.Background(), zap.NewNop(), pool, mapping)

	want := []string{"hello", "sasl-auth", "select-bucket", "cluster-config"}
	for i, w := range want {
		if got[i].Name != w {
			t.Fatalf("equal-timestamp pool reordered: got %v, want %v", mockNames(got), want)
		}
	}
}

// Legacy recordings carry no timestamps at all, so the whole pool is one tie.
func TestFilterConfigMocksMapping_ZeroTimestampsKeepRecordedOrder(t *testing.T) {
	mk := func(name string) *models.Mock {
		return &models.Mock{
			Name:    name,
			Version: "api.keploy.io/v1beta1",
			Spec:    models.MockSpec{Metadata: map[string]string{"type": "config"}},
		}
	}
	pool := []*models.Mock{mk("hello"), mk("sasl-auth"), mk("cfg")}

	got := FilterConfigMocksMapping(context.Background(), zap.NewNop(), pool, []string{"cfg"})

	want := []string{"hello", "sasl-auth", "cfg"}
	for i, w := range want {
		if got[i].Name != w {
			t.Fatalf("zero-timestamp pool reordered: got %v, want %v", mockNames(got), want)
		}
	}
}

// Mapping membership decides the IsFiltered tag and nothing else — no mock is
// dropped, because a reusable mock no single test owns must stay in the pool.
func TestFilterConfigMocksMapping_TagsMembershipAndKeepsEveryMock(t *testing.T) {
	base := time.Unix(1700000000, 0).UTC()
	pool := []*models.Mock{
		configMock("hello", base, 1),
		configMock("sasl-auth", base, 2),
		configMock("select-bucket", base, 3),
	}

	got := FilterConfigMocksMapping(context.Background(), zap.NewNop(), pool, []string{"sasl-auth"})

	if len(got) != 3 {
		t.Fatalf("config pool dropped mocks: %v", mockNames(got))
	}
	want := map[string]bool{"hello": false, "sasl-auth": true, "select-bucket": false}
	for _, m := range got {
		if m.TestModeInfo.IsFiltered != want[m.Name] {
			t.Fatalf("%s: IsFiltered = %v, want %v", m.Name, m.TestModeInfo.IsFiltered, want[m.Name])
		}
	}
}

// The pool must be a deep copy: callers mutate TestModeInfo (SortOrder is
// stamped downstream), and writing through to the shared mock set is a data
// race against every other consumer of the same mocks.
func TestFilterConfigMocksMapping_DeepCopiesSoCallersCannotMutateInput(t *testing.T) {
	base := time.Unix(1700000000, 0).UTC()
	in := []*models.Mock{configMock("hello", base, 1)}

	got := FilterConfigMocksMapping(context.Background(), zap.NewNop(), in, []string{"hello"})

	if got[0] == in[0] {
		t.Fatal("config pool returned the input mock by pointer; callers would mutate shared state")
	}
	got[0].TestModeInfo.SortOrder = 99
	got[0].Name = "mutated"
	if in[0].TestModeInfo.SortOrder != 0 || in[0].Name != "hello" {
		t.Fatalf("mutating the returned pool wrote through to the input: %+v", in[0].TestModeInfo)
	}
	// A shallow copy shares Spec's reference fields, which is the data race the
	// DeepCopy exists to prevent — pointer inequality alone does not catch it.
	got[0].Spec.Metadata["type"] = "mutated"
	if in[0].Spec.Metadata["type"] != "config" {
		t.Fatalf("the copy shares Spec state with the input: metadata = %v", in[0].Spec.Metadata)
	}
}

// The pool as loaded is not guaranteed to be in recorded order, so the sort is
// what establishes it. An input already in order cannot show that.
func TestFilterConfigMocksMapping_SortsAnOutOfOrderPool(t *testing.T) {
	base := time.Unix(1700000000, 0).UTC()
	pool := []*models.Mock{
		configMock("cfg-rev3", base, 3),
		configMock("cfg-rev1", base, 1),
		configMock("cfg-rev4", base, 4),
		configMock("cfg-rev2", base, 2),
	}

	got := FilterConfigMocksMapping(context.Background(), zap.NewNop(), pool, []string{"cfg-rev1"})

	want := []string{"cfg-rev1", "cfg-rev2", "cfg-rev3", "cfg-rev4"}
	for i, w := range want {
		if got[i].Name != w {
			t.Fatalf("out-of-order pool was not sorted into recorded order: got %v, want %v", mockNames(got), want)
		}
	}
}

// A nil entry in the pool must be skipped, not dereferenced or propagated —
// a nil reaching the RB-tree stamp downstream panics.
func TestFilterConfigMocksMapping_SkipsNilMocks(t *testing.T) {
	base := time.Unix(1700000000, 0).UTC()
	pool := []*models.Mock{
		configMock("hello", base, 1),
		nil,
		configMock("cfg", base, 2),
	}

	got := FilterConfigMocksMapping(context.Background(), zap.NewNop(), pool, nil)

	if len(got) != 2 {
		t.Fatalf("nil mock not skipped: got %d entries %v", len(got), mockNames(got))
	}
	for _, m := range got {
		if m == nil {
			t.Fatal("nil mock propagated into the config pool")
		}
	}
}

// Tie order must survive a pool whose chunks arrive INTERLEAVED, which is what
// the loader actually hands over — mocks are not pre-grouped by timestamp. The
// sort has to do two things at once: order the chunks, and keep each chunk's
// frames in the order they were recorded. An unstable sort silently scrambles
// the second (an already-grouped input would hide it, because pdqsort
// short-circuits on sorted input).
func TestFilterConfigMocksMapping_InterleavedChunkTiesKeepRecordedOrder(t *testing.T) {
	base := time.Unix(1700000000, 0).UTC()
	const chunks, perChunk = 8, 8

	// Input round-robins across chunks: c0f0, c1f0, ... c7f0, c0f1, c1f1, ...
	var pool []*models.Mock
	var mapping []string
	for f := 0; f < perChunk; f++ {
		for c := 0; c < chunks; c++ {
			name := fmt.Sprintf("chunk%d-frame%d", c, f)
			pool = append(pool, configMockAt(name, base, c, c))
			if f%2 == 0 {
				mapping = append(mapping, name)
			}
		}
	}

	// Expected: chunks in timestamp order, frames within a chunk in recorded order.
	var want []string
	for c := 0; c < chunks; c++ {
		for f := 0; f < perChunk; f++ {
			want = append(want, fmt.Sprintf("chunk%d-frame%d", c, f))
		}
	}

	got := FilterConfigMocksMapping(context.Background(), zap.NewNop(), pool, mapping)

	if len(got) != len(want) {
		t.Fatalf("pool size changed: got %d want %d", len(got), len(want))
	}
	for i, w := range want {
		if got[i].Name != w {
			t.Fatalf("interleaved chunk ties reordered at index %d: got %s, want %s\n got=%v", i, got[i].Name, w, mockNames(got))
		}
	}
}

// The pool must not be partitioned by ANY attribute — mapping membership was
// the defect found, but partitioning on Lifetime or Kind would reorder a
// recorded sequence exactly the same way. This pins the general rule: recorded
// order in, recorded order out, whatever the mocks differ by.
func TestFilterConfigMocksMapping_DoesNotPartitionByLifetimeOrKind(t *testing.T) {
	base := time.Unix(1700000000, 0).UTC()
	lifetimes := []models.Lifetime{models.LifetimePerTest, models.LifetimeConnection, models.LifetimeSession}
	kinds := []string{"Couchbase", "MySQL", "Postgres"}

	var pool []*models.Mock
	var want []string
	var mapping []string
	for i := 0; i < 18; i++ {
		name := fmt.Sprintf("mock-%02d", i)
		// Interleave lifetime and kind so any partition on either reorders.
		m := configMockAt(name, base, i/3, i/3)
		m.TestModeInfo.Lifetime = lifetimes[i%3]
		m.Kind = models.Kind(kinds[(i+1)%3])
		pool = append(pool, m)
		want = append(want, name)
		if i%2 == 0 {
			mapping = append(mapping, name)
		}
	}

	got := FilterConfigMocksMapping(context.Background(), zap.NewNop(), pool, mapping)

	if len(got) != len(want) {
		t.Fatalf("pool size changed: got %d want %d", len(got), len(want))
	}
	for i, w := range want {
		if got[i].Name != w {
			t.Fatalf("pool was partitioned by lifetime or kind: index %d is %s, want %s\n got=%v",
				i, got[i].Name, w, mockNames(got))
		}
	}
}

// A mock this keploy version does not recognise must still reach the pool, and
// must be reported once at Debug — the pool is not the place to silently drop
// something the user recorded.
func TestFilterConfigMocksMapping_ReportsNonKeployMocks(t *testing.T) {
	base := time.Unix(1700000000, 0).UTC()
	foreign := configMock("foreign", base, 2)
	foreign.Version = "not-keploy/v1"
	pool := []*models.Mock{configMock("hello", base, 1), foreign}

	core, logs := observer.New(zap.DebugLevel)
	got := FilterConfigMocksMapping(context.Background(), zap.New(core), pool, nil)

	if len(got) != 2 {
		t.Fatalf("a non-keploy mock was dropped from the pool: %v", mockNames(got))
	}
	if n := logs.FilterMessageSnippet("not recorded by keploy").Len(); n != 1 {
		t.Fatalf("non-keploy mocks reported %d times, want exactly 1", n)
	}
}
