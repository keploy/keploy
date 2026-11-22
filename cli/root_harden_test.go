package cli

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"go.keploy.io/server/v3/utils"
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

// An unknown verb names where a command of that name, or a near one, lives
// under the group, at any depth, whether or not flags follow it: with flags,
// cobra parsed the group's own flags first and said "unknown flag", which sent
// a coding agent looking for a flag when the command was what it had wrong
// (`keploy cloud request-changes --branch x`, which is `keploy cloud review
// request-changes`). A subcommand's own bad flag is still its parent's to say.
func TestAnUnknownVerbNamesWhereItLives(t *testing.T) {
	newTree := func() *cobra.Command {
		root := &cobra.Command{Use: "keploy"}
		root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return errors.New("root: " + err.Error()) })
		cloud := &cobra.Command{Use: "cloud"}
		cloud.PersistentFlags().String("app", "", "")
		review := &cobra.Command{Use: "review"}
		for _, n := range []string{"approve", "request-changes", "request"} {
			leaf := &cobra.Command{Use: n, RunE: func(*cobra.Command, []string) error { return nil }}
			leaf.Flags().String("branch", "", "")
			review.AddCommand(leaf)
		}
		hidden := &cobra.Command{Use: "request-changes-old", Hidden: true, RunE: func(*cobra.Command, []string) error { return nil }}
		review.AddCommand(hidden)
		// A hidden group's commands are not offered.
		legacy := &cobra.Command{Use: "legacy", Hidden: true}
		legacy.AddCommand(&cobra.Command{Use: "request-changes", RunE: func(*cobra.Command, []string) error { return nil }})
		cloud.AddCommand(review, legacy, &cobra.Command{Use: "restore-test", RunE: func(*cobra.Command, []string) error { return nil }})
		root.AddCommand(cloud)
		HardenUnknownSubcommands(root)
		TagUsageErrors(root)
		return root
	}
	run := func(args ...string) error {
		root := newTree()
		root.SetArgs(args)
		root.SetOut(&bytes.Buffer{})
		root.SetErr(&bytes.Buffer{})
		return root.Execute()
	}
	want := "unknown command \"request-changes\" for \"keploy cloud\"\n\nDid you mean this?\n\tkeploy cloud review request-changes"
	if err := run("cloud", "request-changes"); err == nil || err.Error() != want || !utils.IsUsageError(err) {
		t.Errorf("bare verb: got %v, want the usage error %q", err, want)
	}
	// With flags, the refusal comes through the flag-error funcs, which tag it.
	for _, args := range [][]string{
		{"cloud", "request-changes", "--branch", "b1", "-m", "x"},
		{"cloud", "--app", "a.b", "request-changes", "--branch", "b1"},
	} {
		if err := run(args...); err == nil || err.Error() != want || !errors.Is(err, utils.ErrUsage) {
			t.Errorf("%v: got %v, want the tagged usage error %q", args, err, want)
		}
	}
	// A near name, one level down.
	if err := run("cloud", "aprove", "--branch", "b1"); err == nil || !strings.HasSuffix(err.Error(), "Did you mean this?\n\tkeploy cloud review approve") {
		t.Errorf("near name: %v", err)
	}
	// Nothing close: the refusal alone.
	if err := run("cloud", "zzzzzz", "--branch", "b1"); err == nil || err.Error() != `unknown command "zzzzzz" for "keploy cloud"` {
		t.Errorf("nothing close: %v", err)
	}
	// A subcommand's own unknown flag is still its parent's to say, and so is
	// a group's own bad flag with no verb before it.
	if err := run("cloud", "review", "approve", "--nope"); err == nil || err.Error() != "root: unknown flag: --nope" || !errors.Is(err, utils.ErrUsage) {
		t.Errorf("leaf flag: %v", err)
	}
	if err := run("cloud", "review", "approve", "b1", "--nope"); err == nil || err.Error() != "root: unknown flag: --nope" {
		t.Errorf("leaf argument then a bad flag: %v", err)
	}
	if err := run("cloud", "--nope"); err == nil || err.Error() != "root: unknown flag: --nope" {
		t.Errorf("group flag: %v", err)
	}
	// The verb that exists still runs.
	if err := run("cloud", "review", "request-changes", "--branch", "b1"); err != nil {
		t.Errorf("valid: %v", err)
	}
}

// The hints are bounded.
func TestUnknownSubcommandHintsAreBounded(t *testing.T) {
	g := &cobra.Command{Use: "g"}
	for i := 0; i < maxSubcommandHints+3; i++ {
		g.AddCommand(&cobra.Command{Use: fmt.Sprintf("ab%d", i), RunE: func(*cobra.Command, []string) error { return nil }})
	}
	if n := strings.Count(UnknownSubcommand(g, "ab").Error(), "\n\tg ab"); n != maxSubcommandHints {
		t.Errorf("%d hints, want %d", n, maxSubcommandHints)
	}
}

// A group's own flag-error func still says the flag errors that are not an
// unknown verb, for it and the commands under it.
func TestHardeningKeepsAGroupsOwnFlagErrorFunc(t *testing.T) {
	root := &cobra.Command{Use: "keploy"}
	group := &cobra.Command{Use: "ui"}
	group.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return errors.New("ui: " + err.Error()) })
	group.AddCommand(&cobra.Command{Use: "open", RunE: func(*cobra.Command, []string) error { return nil }})
	root.AddCommand(group)
	HardenUnknownSubcommands(root)
	for _, args := range [][]string{{"ui", "--nope"}, {"ui", "open", "--nope"}} {
		root.SetArgs(args)
		root.SetOut(&bytes.Buffer{})
		root.SetErr(&bytes.Buffer{})
		if err := root.Execute(); err == nil || err.Error() != "ui: unknown flag: --nope" {
			t.Errorf("%v: %v", args, err)
		}
	}
}

// An exact name is never cut by the cap, however many near names come first;
// with suggestions disabled there are none; the walk leaves each command's
// suggestion distance as it found it.
func TestUnknownSubcommandHintsPutExactNamesFirst(t *testing.T) {
	root := &cobra.Command{Use: "keploy"}
	for i := 0; i < maxSubcommandHints+2; i++ {
		root.AddCommand(&cobra.Command{Use: fmt.Sprintf("approve%d", i), RunE: func(*cobra.Command, []string) error { return nil }})
	}
	deep := &cobra.Command{Use: "review"}
	deep.AddCommand(&cobra.Command{Use: "approve", RunE: func(*cobra.Command, []string) error { return nil }})
	root.AddCommand(deep)
	err := UnknownSubcommand(root, "approve").Error()
	if !strings.Contains(err, "Did you mean this?\n\tkeploy review approve\n") || strings.Count(err, "\n\tkeploy ") != maxSubcommandHints {
		t.Errorf("exact name not first, or hints not capped: %q", err)
	}
	if root.SuggestionsMinimumDistance != 0 || deep.SuggestionsMinimumDistance != 0 {
		t.Errorf("distance left changed: %d %d", root.SuggestionsMinimumDistance, deep.SuggestionsMinimumDistance)
	}
	deep.DisableSuggestions = true
	if err := UnknownSubcommand(root, "approve").Error(); strings.Contains(err, "review approve") {
		t.Errorf("a command with suggestions disabled is suggested from: %q", err)
	}
	root.DisableSuggestions = true
	if err := UnknownSubcommand(root, "approve").Error(); err != `unknown command "approve" for "keploy"` {
		t.Errorf("suggestions disabled: %q", err)
	}
	// The root's setting holds for a group asked directly, too.
	deep.DisableSuggestions = false
	if err := UnknownSubcommand(deep, "aprove").Error(); err != `unknown command "aprove" for "keploy review"` {
		t.Errorf("suggestions disabled on the root, asked of a group: %q", err)
	}
}

// cobra refuses an unknown top-level command before any of root's funcs run;
// main turns its refusal into one that names where the command lives.
func TestHintUnknownCommand(t *testing.T) {
	root := &cobra.Command{Use: "keploy"}
	contract := &cobra.Command{Use: "contract"}
	contract.AddCommand(&cobra.Command{Use: "generate", RunE: func(*cobra.Command, []string) error { return nil }})
	root.AddCommand(contract, &cobra.Command{Use: "record", RunE: func(*cobra.Command, []string) error { return nil }})
	root.SetArgs([]string{"generate"})
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	_, raw := root.ExecuteC()
	err := HintUnknownCommand(root, raw)
	if err == nil || err.Error() != "unknown command \"generate\" for \"keploy\"\n\nDid you mean this?\n\tkeploy contract generate" || !utils.IsUsageError(err) {
		t.Errorf("got %v (from %v)", err, raw)
	}
	other := errors.New("boom")
	if HintUnknownCommand(root, other) != other || HintUnknownCommand(root, nil) != nil {
		t.Error("an error that is not cobra's refusal is changed")
	}
	// cobra quotes the verb with %q; it is read back as typed.
	root.SetArgs([]string{"gen\terate"})
	_, raw = root.ExecuteC()
	if err := HintUnknownCommand(root, raw); err == nil || !strings.HasPrefix(err.Error(), `unknown command "gen\terate" for "keploy"`) {
		t.Errorf("quoted verb: %v (from %v)", err, raw)
	}
	sub := errors.New(`unknown command "x" for "keploy contract"`)
	if HintUnknownCommand(root, sub) != sub {
		t.Error("a group's refusal is rewritten as the root's")
	}
}
