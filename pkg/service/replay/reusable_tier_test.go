package replay

import (
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

func ptMock(name string) *models.Mock {
	m := &models.Mock{Name: name, Kind: models.HTTP}
	m.TestModeInfo.Lifetime = models.LifetimePerTest
	return m
}

func sessionMock(name string) *models.Mock {
	m := &models.Mock{Name: name, Kind: models.HTTP}
	m.TestModeInfo.Lifetime = models.LifetimeSession
	return m
}

func connMock(name string) *models.Mock {
	m := &models.Mock{Name: name, Kind: models.HTTP}
	m.TestModeInfo.Lifetime = models.LifetimeConnection
	return m
}

func configMetaMock(name string) *models.Mock {
	// Lifetime left at the per-test zero value to prove the metadata fallback
	// (a mock whose lifetime wasn't derived but whose recorder type is config).
	return &models.Mock{
		Name: name, Kind: models.HTTP,
		Spec: models.MockSpec{Metadata: map[string]string{"type": "config"}},
	}
}

func TestIsReusableTierMock(t *testing.T) {
	cases := []struct {
		name string
		m    *models.Mock
		want bool
	}{
		{"per-test is NOT reusable", ptMock("a"), false},
		{"session is reusable", sessionMock("b"), true},
		{"connection is reusable", connMock("c"), true},
		{"config metadata fallback is reusable", configMetaMock("d"), true},
		{"per-test with no metadata", &models.Mock{Name: "e", Kind: models.HTTP}, false},
	}
	for _, tc := range cases {
		if got := isReusableTierMock(tc.m); got != tc.want {
			t.Errorf("%s: isReusableTierMock=%v want %v", tc.name, got, tc.want)
		}
	}
}

func TestIsReusableTierState(t *testing.T) {
	cases := []struct {
		name string
		s    models.MockState
		want bool
	}{
		{"per-test (zero lifetime, no type)", models.MockState{Name: "a"}, false},
		{"session lifetime", models.MockState{Name: "b", Lifetime: models.LifetimeSession}, true},
		{"connection lifetime", models.MockState{Name: "c", Lifetime: models.LifetimeConnection}, true},
		{"config type fallback", models.MockState{Name: "d", Type: "config"}, true},
		{"connection type fallback", models.MockState{Name: "e", Type: "connection"}, true},
	}
	for _, tc := range cases {
		if got := isReusableTierState(tc.s); got != tc.want {
			t.Errorf("%s: isReusableTierState=%v want %v", tc.name, got, tc.want)
		}
	}
}

// End-to-end of the assertion intent: a test whose mapping lists a per-test
// mock AND a reusable session mock, but whose replay only re-consumed the
// per-test mock, must NOT be flagged as a mock-set mismatch — the session
// mock is excluded from both sides of the subset check.
func TestSubsetExcludesReusableTier(t *testing.T) {
	// Loaded mocks tell us the tier of each name (MockEntry carries none).
	reusable := map[string]bool{}
	for _, m := range []*models.Mock{ptMock("pt-1"), sessionMock("sess-1")} {
		if isReusableTierMock(m) {
			reusable[m.Name] = true
		}
	}

	// Mapping (expected) lists both; filter to per-test only.
	expected := []models.MockEntry{{Name: "pt-1", Kind: "Http"}, {Name: "sess-1", Kind: "Http"}}
	var filteredExpected []string
	for _, e := range expected {
		if reusable[e.Name] {
			continue
		}
		filteredExpected = append(filteredExpected, e.Name)
	}

	// Consumed during replay: only the per-test mock came back (session reused
	// elsewhere / not re-reported for this test).
	consumed := []models.MockState{{Name: "pt-1", Lifetime: models.LifetimePerTest}}
	var filteredConsumed []string
	for _, s := range consumed {
		if isReusableTierState(s) {
			continue
		}
		filteredConsumed = append(filteredConsumed, s.Name)
	}

	if mismatch := !isMockSubset(filteredConsumed, filteredExpected); mismatch {
		t.Errorf("expected NO mock-set mismatch once the reusable session mock is excluded; got mismatch (expected=%v consumed=%v)", filteredExpected, filteredConsumed)
	}

	// Control: a genuinely missing per-test mock must still flag a mismatch.
	expected2 := []string{"pt-1", "pt-2"} // pt-2 never consumed
	if mismatch := !isMockSubset(filteredConsumed, expected2); !mismatch {
		t.Errorf("expected a mismatch when a per-test mock is genuinely missing")
	}
}

// isMockSubsetWithConfig (streaming path) must ignore an extra consumed mock
// of ANY reusable/startup tier (session/connection/config) or DNS — not just
// config — so a reused mock can't falsely fail the streaming assertion.
func TestIsMockSubsetWithConfig_ReusableTiers(t *testing.T) {
	expected := []string{"pt-1"}
	cases := []struct {
		name     string
		consumed []models.MockState
		want     bool
	}{
		{"extra session mock ignored", []models.MockState{
			{Name: "pt-1", Lifetime: models.LifetimePerTest},
			{Name: "sess-1", Lifetime: models.LifetimeSession},
		}, true},
		{"extra connection mock ignored", []models.MockState{
			{Name: "pt-1"}, {Name: "conn-1", Lifetime: models.LifetimeConnection},
		}, true},
		{"extra config mock ignored (legacy)", []models.MockState{
			{Name: "pt-1"}, {Name: "cfg-1", Type: "config"},
		}, true},
		{"extra DNS mock ignored", []models.MockState{
			{Name: "pt-1"}, {Name: "dns-1", Kind: models.DNS},
		}, true},
		{"extra per-test mock still fails", []models.MockState{
			{Name: "pt-1"}, {Name: "pt-extra", Lifetime: models.LifetimePerTest},
		}, false},
	}
	for _, tc := range cases {
		if got := isMockSubsetWithConfig(tc.consumed, expected); got != tc.want {
			t.Errorf("%s: isMockSubsetWithConfig=%v want %v", tc.name, got, tc.want)
		}
	}
}
