package user

import (
	"context"
	"errors"
	"testing"

	"go.uber.org/zap"
)

// withMachineID makes the OS machine id raw (or err) for one test.
func withMachineID(t *testing.T, raw string, err error) {
	t.Helper()
	old := machineID
	machineID = func() (string, error) { return raw, err }
	t.Cleanup(func() { machineID = old })
}

// The installation id is the keyed hash of the OS machine id, never the id:
// the expected value is HMAC-SHA256("keploy") keyed by the id, computed
// apart (Python's hmac), which is what machineid.ProtectedID("keploy") gives.
func TestGetInstallationIDSendsAKeyedHashOfTheMachineID(t *testing.T) {
	const raw = "3f2c9a1b7d4e4c0f9a6b2d8e1c5f7a90"
	const want = "31b6af4fa06650c85cf7e197dd0398d0e1dc51a027362a36d87dec51c5aa0ee6"
	for _, tc := range []struct {
		name     string
		inDocker string
		raw      string
		err      error
		envID    string
		want     string
	}{
		{name: "a machine id is sent hashed", raw: raw, want: want},
		{name: "a container keploy started sends the host's id as given", inDocker: "true", raw: "container-id", envID: want, want: want},
		{name: "KEPLOY_INDOCKER other than true reads the machine", inDocker: "1", raw: raw, envID: "host-id", want: want},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("KEPLOY_INDOCKER", tc.inDocker)
			t.Setenv("INSTALLATION_ID", tc.envID)
			withMachineID(t, tc.raw, tc.err)
			got, err := New(zap.NewNop(), nil).GetInstallationID(context.Background())
			if err != nil {
				t.Fatalf("GetInstallationID: %v", err)
			}
			if got != tc.want {
				t.Fatalf("GetInstallationID = %q, want %q", got, tc.want)
			}
		})
	}
}

// When an environment has no OS machine id (containers, CI) or runs in Docker
// without INSTALLATION_ID, GetInstallationID falls back to a persisted random
// 64-hex installation id instead of returning "" (which becomes unattributed)
// and never sends the hash of an empty string.
func TestGetInstallationIDPersistedFallback(t *testing.T) {
	emptyHash := installationID("")
	for _, tc := range []struct {
		name     string
		inDocker string
		raw      string
		err      error
	}{
		{name: "empty machine id falls back to persisted id", raw: ""},
		{name: "unreadable machine id falls back to persisted id", raw: "ignored", err: errors.New("no machine id")},
		{name: "container with no INSTALLATION_ID falls back to persisted id", inDocker: "true", raw: "container-id"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("USERPROFILE", home)
			t.Setenv("HOMEDRIVE", "")
			t.Setenv("HOMEPATH", "")
			t.Setenv("KEPLOY_INDOCKER", tc.inDocker)
			t.Setenv("INSTALLATION_ID", "")
			withMachineID(t, tc.raw, tc.err)

			db := New(zap.NewNop(), nil)
			first, err := db.GetInstallationID(context.Background())
			if err != nil {
				t.Fatalf("first GetInstallationID: %v", err)
			}
			if !isValidFallbackID(first) {
				t.Fatalf("fallback installation id %q is not 64 lowercase hex chars", first)
			}
			if first == emptyHash {
				t.Fatalf("fallback installation id must not be the hash of empty string")
			}

			// A second call in the same HOME returns the same persisted id.
			second, err := db.GetInstallationID(context.Background())
			if err != nil {
				t.Fatalf("second GetInstallationID: %v", err)
			}
			if second != first {
				t.Fatalf("persisted fallback id changed across calls: %q vs %q", first, second)
			}

			// A distinct HOME generates a distinct id.
			otherHome := t.TempDir()
			t.Setenv("HOME", otherHome)
			t.Setenv("USERPROFILE", otherHome)
			other, err := db.GetInstallationID(context.Background())
			if err != nil {
				t.Fatalf("other HOME GetInstallationID: %v", err)
			}
			if !isValidFallbackID(other) || other == first {
				t.Fatalf("distinct HOME got %q (first=%q), want distinct 64-hex id", other, first)
			}
		})
	}
}

// Two machines get two ids, and one machine the same id on every run.
func TestInstallationIDNamesEachMachineApart(t *testing.T) {
	a, b := installationID("machine-a"), installationID("machine-b")
	if a == b {
		t.Fatalf("two machine ids gave one installation id %q", a)
	}
	if again := installationID("machine-a"); again != a {
		t.Fatalf("one machine id gave %q, then %q", a, again)
	}
	if len(a) != 64 {
		t.Fatalf("installation id %q is %d characters, want the 64 of a hex SHA-256", a, len(a))
	}
}
