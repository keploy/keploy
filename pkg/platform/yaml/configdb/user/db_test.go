package user

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
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

func withTempHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if runtime.GOOS == "windows" {
		t.Setenv("HOMEDRIVE", "")
		t.Setenv("HOMEPATH", "")
		t.Setenv("USERPROFILE", dir)
	} else {
		t.Setenv("HOME", dir)
	}
	processFallbackIDMu.Lock()
	processFallbackID = ""
	processFallbackIDMu.Unlock()
	t.Cleanup(func() {
		processFallbackIDMu.Lock()
		processFallbackID = ""
		processFallbackIDMu.Unlock()
	})
	return dir
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
			withTempHome(t)
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

// When machineid is unavailable or empty, or KEPLOY_INDOCKER=true has no
// INSTALLATION_ID, GetInstallationID persists and reuses a unique random 64-hex
// fallback ID in ~/.keploy/keploy.yaml instead of returning "" (unattributed)
// or the constant hash of "".
func TestGetInstallationIDPersistsFallbackWhenMachineIDUnavailable(t *testing.T) {
	emptyHash := installationID("")
	for _, tc := range []struct {
		name     string
		inDocker string
		raw      string
		err      error
	}{
		{name: "empty machine id", raw: ""},
		{name: "machine id error", raw: "", err: errors.New("no machine id")},
		{name: "container with no INSTALLATION_ID", inDocker: "true", raw: "container-id"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home1 := withTempHome(t)
			t.Setenv("KEPLOY_INDOCKER", tc.inDocker)
			t.Setenv("INSTALLATION_ID", "")
			withMachineID(t, tc.raw, tc.err)

			db := New(zap.NewNop(), nil)
			first, err := db.GetInstallationID(context.Background())
			if err != nil {
				t.Fatalf("first GetInstallationID: %v", err)
			}
			if len(first) != 64 {
				t.Fatalf("first fallback id = %q (len %d), want 64-hex", first, len(first))
			}
			if first == emptyHash {
				t.Fatalf("fallback id must not be HMAC of empty string (%q)", emptyHash)
			}
			// Verify persisted in ~/.keploy/keploy.yaml and stable across calls.
			if _, statErr := os.Stat(filepath.Join(home1, ".keploy", "keploy.yaml")); statErr != nil {
				t.Fatalf("expected persisted keploy.yaml: %v", statErr)
			}
			second, err := db.GetInstallationID(context.Background())
			if err != nil {
				t.Fatalf("second GetInstallationID: %v", err)
			}
			if second != first {
				t.Fatalf("second GetInstallationID = %q, want persisted %q", second, first)
			}

			// A distinct home directory receives a distinct fallback ID.
			withTempHome(t)
			other, err := db.GetInstallationID(context.Background())
			if err != nil {
				t.Fatalf("other home GetInstallationID: %v", err)
			}
			if len(other) != 64 || other == first {
				t.Fatalf("other home id = %q, want distinct 64-hex from %q", other, first)
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

// LookupInstallationID gives the id GetInstallationID gives wherever that
// needs no new one, and writes nothing: where GetInstallationID would mint
// and persist a fallback id, it gives "" and leaves no keploy.yaml; once a
// fallback id is persisted, it gives that one.
func TestLookupInstallationIDNeverMintsOrWrites(t *testing.T) {
	const raw = "3f2c9a1b7d4e4c0f9a6b2d8e1c5f7a90"
	for _, tc := range []struct {
		name          string
		inDocker      string
		raw           string
		err           error
		envID         string
		needsFallback bool
	}{
		{name: "a machine id", raw: raw},
		{name: "a container keploy started, with the host's id", inDocker: "true", raw: "container-id", envID: "host-id"},
		{name: "empty machine id", raw: "", needsFallback: true},
		{name: "machine id error", err: errors.New("no machine id"), needsFallback: true},
		{name: "a container with no INSTALLATION_ID", inDocker: "true", raw: "container-id", needsFallback: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := withTempHome(t)
			t.Setenv("KEPLOY_INDOCKER", tc.inDocker)
			t.Setenv("INSTALLATION_ID", tc.envID)
			withMachineID(t, tc.raw, tc.err)
			db := New(zap.NewNop(), nil)
			keployYAML := filepath.Join(home, ".keploy", "keploy.yaml")

			looked := db.LookupInstallationID(context.Background())
			if _, err := os.Stat(keployYAML); !os.IsNotExist(err) {
				t.Fatalf("LookupInstallationID wrote %s (stat err=%v)", keployYAML, err)
			}
			if tc.needsFallback {
				if looked != "" {
					t.Fatalf("LookupInstallationID = %q with no id to find, want \"\"", looked)
				}
			}
			got, err := db.GetInstallationID(context.Background())
			if err != nil {
				t.Fatalf("GetInstallationID: %v", err)
			}
			if !tc.needsFallback && looked != got {
				t.Fatalf("LookupInstallationID = %q, GetInstallationID = %q: want the same id", looked, got)
			}
			if again := db.LookupInstallationID(context.Background()); again != got {
				t.Fatalf("after GetInstallationID gave %q, LookupInstallationID = %q", got, again)
			}
		})
	}
}
