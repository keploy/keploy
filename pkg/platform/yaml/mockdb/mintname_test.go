// A mock's name IS its identity: mappings.yaml joins tests to mocks on it, and
// nine call sites key on it. These tests pin what that identity is now made of.
//
// The safety property comes first: `keploy record` / `keploy test` -- integration
// testing, a different product on this same storage layer -- never set Owner, and
// must keep drawing from one run-wide mock-N sequence, byte for byte.
package mockdb

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

func mintAll(t *testing.T, ys *MockYaml, owners ...string) []string {
	t.Helper()
	out := make([]string, 0, len(owners))
	for _, owner := range owners {
		mk := ownerTestMock(owner)
		if err := ys.InsertMock(context.Background(), mk, "set-0"); err != nil {
			t.Fatalf("InsertMock(owner=%q): %v", owner, err)
		}
		out = append(out, mk.Name)
	}
	return out
}

func equalNames(t *testing.T, got, want []string, msg string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: want %v, got %v", msg, want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%s: want %v, got %v", msg, want, got)
		}
	}
}

// P1. An unowned recording is named exactly as it was before owners existed.
func TestUnownedMintIsUnchanged(t *testing.T) {
	ys := New(zap.NewNop(), t.TempDir(), "")
	equalNames(t, mintAll(t, ys, "", "", "", ""),
		[]string{"mock-0", "mock-1", "mock-2", "mock-3"},
		"an unowned run must keep the original arrival-index sequence")
}

// ResetCounterID is what a re-record calls; every sequence restarts.
func TestResetCounterIDRestartsEverySequence(t *testing.T) {
	ys := New(zap.NewNop(), t.TempDir(), "")
	first := mintAll(t, ys, "", "alpha", "alpha")
	ys.ResetCounterID()
	second := mintAll(t, ys, "", "alpha", "alpha")
	equalNames(t, second, first, "a re-record must reissue the same names")
}

// The central property. An owner's ordinals count within that owner, so mocks
// belonging to OTHER owners -- however many, in whatever order -- cannot move
// them. Under the old arrival-index scheme every name below would shift.
func TestOwnerOrdinalsAreIndependentOfOtherOwners(t *testing.T) {
	ys := New(zap.NewNop(), t.TempDir(), "")
	alpha, beta := ownerHash("test-alpha"), ownerHash("test-beta")

	// Run 1: alpha and beta interleaved, plus an unowned boot-time capture.
	equalNames(t, mintAll(t, ys, "", "test-alpha", "test-beta", "test-alpha", "test-beta", "test-alpha"),
		[]string{"mock-0", alpha + "-0", beta + "-0", alpha + "-1", beta + "-1", alpha + "-2"},
		"run 1")

	// Run 2: beta is gone entirely (think --grep-invert), and the unowned
	// capture happens later. alpha's names must not move at all.
	ys.ResetCounterID()
	equalNames(t, mintAll(t, ys, "test-alpha", "test-alpha", "", "test-alpha"),
		[]string{alpha + "-0", alpha + "-1", "mock-0", alpha + "-2"},
		"run 2: removing another owner must not rebind alpha")

	// Run 3: a brand-new owner runs FIRST. Still no movement.
	ys.ResetCounterID()
	gamma := ownerHash("test-gamma")
	equalNames(t, mintAll(t, ys, "test-gamma", "test-alpha", "test-gamma", "test-alpha", "test-alpha"),
		[]string{gamma + "-0", alpha + "-0", gamma + "-1", alpha + "-1", alpha + "-2"},
		"run 3: adding an owner ahead of alpha must not rebind alpha")
}

// The hash is a pure function of the owner string, so the same owner names its
// mocks identically in any process, on any machine, in any run.
func TestOwnerHashIsStableAndBoundedInLength(t *testing.T) {
	const owner = "llm-keys.spec.ts > LLM Key Management > opens the dialog"
	h := ownerHash(owner)
	if len(h) != ownerHashLen {
		t.Fatalf("owner hash must be %d chars, got %d (%q)", ownerHashLen, len(h), h)
	}
	if h != ownerHash(owner) {
		t.Fatal("ownerHash must be deterministic")
	}
	if h == ownerHash(owner+" ") {
		t.Fatal("ownerHash must distinguish distinct owners")
	}
}

// A truncated hash can in principle collide. Every owner string is in hand at
// mint time, so the probabilistic bound is replaced with a deterministic check:
// a second owner landing on an occupied hash is a hard error, not two recordings
// quietly sharing one name sequence. Forced here by seeding the table, since a
// real 48-bit collision cannot be constructed.
func TestCollidingOwnersAreRejectedNotMerged(t *testing.T) {
	ys := New(zap.NewNop(), t.TempDir(), "")
	if err := ys.InsertMock(context.Background(), ownerTestMock("real-owner"), "set-0"); err != nil {
		t.Fatalf("InsertMock: %v", err)
	}
	// Pretend a different owner already occupies "real-owner"'s 12 hex characters.
	ys.nameMu.Lock()
	ys.hashToOwner[ownerHash("real-owner")] = "some-other-owner"
	ys.nameMu.Unlock()

	err := ys.InsertMock(context.Background(), ownerTestMock("real-owner"), "set-0")
	if err == nil {
		t.Fatal("a name collision between two owners must be an error, not a silent merge")
	}
	if !strings.Contains(err.Error(), "both hash to") {
		t.Fatalf("the error must name the collision, got: %v", err)
	}
}

// Minting is concurrent: several parser goroutines insert at once. Every name
// must be unique and every owner's sequence dense from 0.
func TestConcurrentMintIsUniqueAndDense(t *testing.T) {
	ys := New(zap.NewNop(), t.TempDir(), "")
	const perOwner = 50
	owners := []string{"", "a", "b"}
	got := make(chan string, len(owners)*perOwner)
	done := make(chan struct{})
	for _, owner := range owners {
		go func(owner string) {
			defer func() { done <- struct{}{} }()
			for i := 0; i < perOwner; i++ {
				name, err := ys.mintName(owner)
				if err != nil {
					t.Error(err)
					return
				}
				got <- name
			}
		}(owner)
	}
	for range owners {
		<-done
	}
	close(got)

	seen := map[string]bool{}
	for name := range got {
		if seen[name] {
			t.Fatalf("duplicate mock name minted: %q", name)
		}
		seen[name] = true
	}
	for _, owner := range owners {
		prefix := unownedPrefix
		if owner != "" {
			prefix = ownerHash(owner)
		}
		for i := 0; i < perOwner; i++ {
			if !seen[prefix+"-"+strconv.Itoa(i)] {
				t.Fatalf("owner %q is missing ordinal %d", owner, i)
			}
		}
	}
}

// The gate is Owner, nothing else: a mock that reaches InsertMock with no owner
// takes the legacy branch even in a session that has owned mocks in it.
func TestUnownedAndOwnedCoexistInOneSession(t *testing.T) {
	ys := New(zap.NewNop(), t.TempDir(), "")
	mk := ownerTestMock("")
	if err := ys.InsertMock(context.Background(), ownerTestMock("owned"), "set-0"); err != nil {
		t.Fatal(err)
	}
	if err := ys.InsertMock(context.Background(), mk, "set-0"); err != nil {
		t.Fatal(err)
	}
	if mk.Name != "mock-0" {
		t.Fatalf("an unowned mock must be mock-0 regardless of owned mocks in the session, got %q", mk.Name)
	}
}

// `keploy mock replay --on-miss record` appends to a set that is already on
// disk, in a process whose counters are empty. Without seeding, every appended
// mock reuses a recorded name; with a single int64 seed, only one of the
// sequences could be continued. Each must continue independently.
func TestSeedCountersContinuesEverySequenceIndependently(t *testing.T) {
	alpha, beta := ownerHash("alpha"), ownerHash("beta")
	onDisk := []*models.Mock{
		{Name: "mock-0"},
		{Name: "mock-1"},
		{Name: alpha + "-0", Owner: "alpha"},
		{Name: alpha + "-1", Owner: "alpha"},
		{Name: alpha + "-2", Owner: "alpha"},
		{Name: beta + "-0", Owner: "beta"},
		nil,
		{Name: "not-a-minted-name"},
	}

	ys := New(zap.NewNop(), t.TempDir(), "")
	ys.SeedCounters(onDisk)
	equalNames(t, mintAll(t, ys, "", "alpha", "beta", "alpha"),
		[]string{"mock-2", alpha + "-3", beta + "-1", alpha + "-4"},
		"each sequence must continue past what is on disk")
}

// Seeding also claims each prefix for the owner that wrote it, so the collision
// check still holds across an append.
func TestSeedCountersClaimsPrefixesForTheirOwners(t *testing.T) {
	ys := New(zap.NewNop(), t.TempDir(), "")
	ys.SeedCounters([]*models.Mock{{Name: ownerHash("alpha") + "-0", Owner: "alpha"}})

	ys.nameMu.Lock()
	got := ys.hashToOwner[ownerHash("alpha")]
	ys.nameMu.Unlock()
	if got != "alpha" {
		t.Fatalf("seeding must claim the prefix for its owner, got %q", got)
	}
}

// An out-of-order or sparse set still seeds from the highest ordinal present.
func TestSeedCountersUsesTheHighestOrdinal(t *testing.T) {
	ys := New(zap.NewNop(), t.TempDir(), "")
	ys.SeedCounters([]*models.Mock{{Name: "mock-9"}, {Name: "mock-3"}, {Name: "mock-7"}})
	equalNames(t, mintAll(t, ys, ""), []string{"mock-10"}, "seed from the highest, not the last")
}

func TestSplitMockName(t *testing.T) {
	for _, tc := range []struct {
		name   string
		prefix string
		n      int64
		ok     bool
	}{
		{"mock-0", "mock", 0, true},
		{"mock-123", "mock", 123, true},
		{"3f2a1b9c4d5e-7", "3f2a1b9c4d5e", 7, true},
		{"mock-", "", 0, false},
		{"mock", "", 0, false},
		{"-4", "", 0, false},
		{"mock-x", "", 0, false},
		{"mock-99999999999999999999", "", 0, false},
		{"", "", 0, false},
	} {
		prefix, n, ok := splitMockName(tc.name)
		if ok != tc.ok || prefix != tc.prefix || n != tc.n {
			t.Errorf("splitMockName(%q) = (%q, %d, %v), want (%q, %d, %v)",
				tc.name, prefix, n, ok, tc.prefix, tc.n, tc.ok)
		}
	}
}
