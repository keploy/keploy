package proxy

import (
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// The agent leaves out of a test's pool a mock whose response is stamped
// before its request, and says so at the default level for each such mock,
// once, naming it: not for the first alone, which left every later one silent.
func TestTheManagerSaysOfEachMockAnsweredBeforeItWasAskedOnce(t *testing.T) {
	t.Setenv("KEPLOY_PROVEN", "0")
	core, logs := observer.New(zapcore.InfoLevel)
	mm := NewMockManager(nil, nil, zap.New(core))
	defer mm.Close()
	start := time.Date(2026, 10, 17, 11, 19, 51, 0, time.UTC)
	end := start.Add(30 * time.Second)
	inverted := func(name string, at time.Duration) *models.Mock {
		m := newMockForTest(name, start.Add(at), models.LifetimePerTest)
		m.Spec.ResTimestampMock = m.Spec.ReqTimestampMock.Add(-37 * time.Microsecond)
		return m
	}
	first, second := inverted("manager-inverted-first", time.Second), inverted("manager-inverted-second", 2*time.Second)
	valid := newMockForTest("manager-valid", start.Add(3*time.Second), models.LifetimePerTest)
	for test := 0; test < 2; test++ {
		mm.SetMocksWithWindow([]*models.Mock{first, second, valid}, nil, start, end)
		got, err := mm.GetPerTestMocksInWindow()
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0].Name != valid.Name {
			t.Fatalf("test %d's pool is %v; want only %s", test, mockNames(got), valid.Name)
		}
	}
	said := map[string]int{}
	for _, e := range logs.FilterLevelExact(zapcore.WarnLevel).All() {
		said[e.ContextMap()["mock"].(string)]++
	}
	for _, m := range []*models.Mock{first, second} {
		if said[m.Name] != 1 {
			t.Errorf("the agent warned of %s %d times; want once", m.Name, said[m.Name])
		}
	}
	if said[valid.Name] != 0 {
		t.Errorf("the agent warned of %s, which was answered after it was asked", valid.Name)
	}
}

// Under the proven switch the agent keeps such a mock in the test's pool: the
// test owns it by its mapping, whatever its stamps say.
func TestUnderTheSwitchTheManagerKeepsAMockAnsweredBeforeItWasAsked(t *testing.T) {
	t.Setenv("KEPLOY_PROVEN", "1")
	core, logs := observer.New(zapcore.InfoLevel)
	mm := NewMockManager(nil, nil, zap.New(core))
	defer mm.Close()
	start := time.Date(2026, 10, 17, 11, 19, 51, 0, time.UTC)
	m := newMockForTest("manager-inverted-kept", start.Add(time.Second), models.LifetimePerTest)
	m.Spec.ResTimestampMock = m.Spec.ReqTimestampMock.Add(-37 * time.Microsecond)
	mm.SetMocksWithWindow([]*models.Mock{m}, nil, start, start.Add(30*time.Second))
	got, err := mm.GetPerTestMocksInWindow()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name != m.Name {
		t.Fatalf("under the switch the test's pool is %v; want %s", mockNames(got), m.Name)
	}
	if n := logs.FilterLevelExact(zapcore.WarnLevel).Len(); n != 0 {
		t.Errorf("under the switch the agent warned %d times of a mock it keeps", n)
	}
}

// Whether the open test owns a mock (OwnedByOpenTest) agrees with the
// windowed read: one answered before it was asked is in no window with the
// switch off, and in its request's window under the switch, where the read
// keeps it.
func TestAMockAnsweredBeforeItWasAskedIsInItsWindowOnlyUnderTheSwitch(t *testing.T) {
	start := time.Date(2026, 10, 17, 11, 19, 51, 0, time.UTC)
	m := newMockForTest("window-inverted", start.Add(time.Second), models.LifetimePerTest)
	m.Spec.ResTimestampMock = m.Spec.ReqTimestampMock.Add(-37 * time.Microsecond)
	t.Setenv("KEPLOY_PROVEN", "0")
	if inRecordedWindow(m, start, start.Add(time.Minute), time.Time{}, false) {
		t.Error("with the switch off the mock is in the window; the read leaves it out")
	}
	t.Setenv("KEPLOY_PROVEN", "1")
	if !inRecordedWindow(m, start, start.Add(time.Minute), time.Time{}, false) {
		t.Error("under the switch the mock is in no window; the read keeps it")
	}
}
