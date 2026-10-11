package provider

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"go.keploy.io/server/v3/utils"
)

func TestMockRunsPrintOneVersionLineWithoutATerminal(t *testing.T) {
	for _, tc := range []struct {
		name string
		mock bool
		logo bool
	}{
		{name: "keploy record or test", mock: false, logo: true},
		{name: "mock run", mock: true, logo: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, err := os.Create(filepath.Join(t.TempDir(), "out"))
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			banner(f, tc.mock, true)
			b, _ := os.ReadFile(f.Name())
			if got := string(b); strings.Contains(got, "▓") != tc.logo {
				t.Fatalf("logo printed = %v, want %v: %q", !tc.logo, tc.logo, got)
			}
			if tc.mock && string(b) != "keploy: version "+utils.Version+"\n" {
				t.Fatalf("got %q", b)
			}
		})
	}
}

func TestIsMockRun(t *testing.T) {
	mock := &cobra.Command{Use: "mock"}
	record, replay, status := &cobra.Command{Use: "record"}, &cobra.Command{Use: "replay"}, &cobra.Command{Use: "status"}
	mock.AddCommand(record, replay, status)
	root := &cobra.Command{Use: "keploy"}
	plainRecord, test := &cobra.Command{Use: "record"}, &cobra.Command{Use: "test"}
	root.AddCommand(mock, plainRecord, test)
	for cmd, want := range map[*cobra.Command]bool{record: true, replay: true, status: false, plainRecord: false, test: false, root: false} {
		if got := IsMockRun(cmd); got != want {
			t.Errorf("IsMockRun(%s) = %v, want %v", cmd.CommandPath(), got, want)
		}
	}
}
