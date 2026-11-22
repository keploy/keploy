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
		{name: "a machine with an empty id sends none, not the hash of nothing", raw: "", want: ""},
		{name: "a machine whose id cannot be read sends none", raw: raw, err: errors.New("no machine id"), want: ""},
		{name: "a container keploy started sends the host's id as given", inDocker: "true", raw: "container-id", envID: want, want: want},
		{name: "a container given no id sends none", inDocker: "true", raw: "container-id", want: ""},
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
