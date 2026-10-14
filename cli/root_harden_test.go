package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// TestHardenUnknownSubcommands verifies that the tree walk makes command GROUPS
// reject an unknown verb (instead of printing help and exiting 0), while leaving
// no-verb help, valid subcommands, and groups that already have their own RunE
// untouched. This is what stops `keploy mock bogus` exiting 0 (design §P0b).
func TestHardenUnknownSubcommands(t *testing.T) {
	newTree := func() (root *cobra.Command, leafRan *bool) {
		ran := false
		root = &cobra.Command{Use: "keploy"}

		group := &cobra.Command{Use: "mock"} // a group with subcommands, no RunE
		group.AddCommand(&cobra.Command{
			Use:  "record",
			RunE: func(*cobra.Command, []string) error { ran = true; return nil },
		})
		root.AddCommand(group)

		// A group that already owns a RunE must be left alone.
		withRun := &cobra.Command{Use: "cfg", RunE: func(*cobra.Command, []string) error { return nil }}
		withRun.AddCommand(&cobra.Command{Use: "sub", RunE: func(*cobra.Command, []string) error { return nil }})
		root.AddCommand(withRun)

		HardenUnknownSubcommands(root)
		return root, &ran
	}

	exec := func(t *testing.T, root *cobra.Command, args ...string) error {
		t.Helper()
		root.SetArgs(args)
		root.SetOut(&bytes.Buffer{})
		root.SetErr(&bytes.Buffer{})
		return root.Execute()
	}

	t.Run("unknown verb errors", func(t *testing.T) {
		root, _ := newTree()
		err := exec(t, root, "mock", "bogus")
		if err == nil || !strings.HasPrefix(err.Error(), "unknown command") {
			t.Fatalf("unknown verb: got err=%v, want an 'unknown command' error", err)
		}
	})

	t.Run("no verb shows help without error", func(t *testing.T) {
		root, _ := newTree()
		if err := exec(t, root, "mock"); err != nil {
			t.Fatalf("no verb: got err=%v, want nil (help)", err)
		}
	})

	t.Run("valid subcommand still runs", func(t *testing.T) {
		root, ran := newTree()
		if err := exec(t, root, "mock", "record"); err != nil {
			t.Fatalf("valid sub: got err=%v", err)
		}
		if !*ran {
			t.Fatalf("valid sub: the leaf's RunE did not run (parent RunE shadowed it)")
		}
	})

	t.Run("group with its own RunE is left alone", func(t *testing.T) {
		root, _ := newTree()
		// cfg owns a RunE, so harden must not have swapped in the reject wrapper:
		// an extra arg runs cfg's own RunE (nil) rather than an unknown-command error.
		if err := exec(t, root, "cfg", "whatever"); err != nil {
			t.Fatalf("group with own RunE: got err=%v, want nil (its own RunE handles args)", err)
		}
	})
}
