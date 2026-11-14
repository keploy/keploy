//go:build linux

package agent

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/models"
)

// A test's harness note (ScopeReq.Note) is keploy's to keep, never to read:
// at record each end's is kept on its own test's window as it was sent, a
// begin is answered none, and an end with a longer note than
// models.MaxScopeNote is refused with the bound, the test still open. At
// replay each begin is answered its own test's recorded note; one longer
// than the bound is not installed.
func TestATestsNoteIsKeptAtItsRecordAndAnsweredAtItsReplay(t *testing.T) {
	ctx := context.Background()
	note := "frames /a:2\n\tdraws 7 \"x\": #y\x00 " + strings.Repeat("é", 100)
	long := strings.Repeat("n", models.MaxScopeNote+1)

	t.Run("record", func(t *testing.T) {
		a, _ := workerAgent(t, models.MODE_RECORD, 4)
		a.SetTestNotes(ctx, map[string]string{"a::TestOne": "installed"})
		w1, w2 := child(t), child(t)
		claimWorker(t, a, w1.Process.Pid, "a", 2)
		claimWorker(t, a, w2.Process.Pid, "b", 2)
		one, serr := a.ProvenBegin(as(w1.Process.Pid), seededBegin("TestOne", "1111222233334444"))
		require.Nil(t, serr)
		require.Equal(t, "", one.Note, "a record answers no note")
		two, serr := a.ProvenBegin(as(w2.Process.Pid), seededBegin("TestTwo", "aaaabbbbccccdddd"))
		require.Nil(t, serr)
		require.Equal(t, "", two.Note)
		_, serr = a.ProvenEnd(as(w1.Process.Pid), models.ScopeReq{Name: "TestOne", Outcome: "passed", Note: note})
		require.Nil(t, serr)
		_, serr = a.ProvenEnd(as(w2.Process.Pid), models.ScopeReq{Name: "TestTwo", Outcome: "passed", Note: long})
		require.NotNil(t, serr, "a note over the bound is refused")
		require.Equal(t, http.StatusBadRequest, serr.Status)
		require.Equal(t, models.ScopeErrBadRequest, serr.Code)
		require.Contains(t, serr.Error(), "the end of TestTwo sent a note of 16385 bytes, over keploy's bound of 16384")
		// The bound is in bytes: a note of two-byte characters one byte over
		// it (8193 characters) is refused too.
		multi := strings.Repeat("é", models.MaxScopeNote/2) + "n"
		_, serr = a.ProvenEnd(as(w2.Process.Pid), models.ScopeReq{Name: "TestTwo", Outcome: "passed", Note: multi})
		require.NotNil(t, serr, "a multi-byte note over the bound in bytes is refused")
		require.Equal(t, http.StatusBadRequest, serr.Status)
		require.Contains(t, serr.Error(), "the end of TestTwo sent a note of 16385 bytes, over keploy's bound of 16384")
		_, serr = a.ProvenEnd(as(w2.Process.Pid), models.ScopeReq{Name: "TestTwo", Outcome: "passed", Note: strings.Repeat("n", models.MaxScopeNote)})
		require.Nil(t, serr, "the refused end left the test open; a note at the bound is kept")
		require.Equal(t, note, windowOf(t, a, "a::TestOne").Note, "byte for byte")
		require.Equal(t, strings.Repeat("n", models.MaxScopeNote), windowOf(t, a, "b::TestTwo").Note)
		require.Equal(t, "1111222233334444", windowOf(t, a, "a::TestOne").Seed, "the note leaves what the begin kept")
		require.Equal(t, "aaaabbbbccccdddd", windowOf(t, a, "b::TestTwo").Seed)
		a.noteScopeNoteDone("a::TestOne", 0, "late")
		require.NotContains(t, a.scopeMeta, scopeKey{name: "a::TestOne"}, "an end of a test that is not open notes nothing")
	})

	t.Run("replay", func(t *testing.T) {
		a, _, _ := instanceAgent(t, models.MODE_TEST)
		a.proven.pool = map[string][]string{"a": {"pa"}, "b": {"pb"}}
		a.SetTestNotes(ctx, map[string]string{"a::TestOne": note, "b::TestTwo": long, "c::TestThree": "three", "d::TestFour": long[1:]})
		w1, w2 := child(t), child(t)
		claimWorker(t, a, w1.Process.Pid, "a", 2)
		claimWorker(t, a, w2.Process.Pid, "b", 2)
		one, serr := a.ProvenBegin(as(w1.Process.Pid), seededBegin("TestOne", "fedcba9876543210"))
		require.Nil(t, serr)
		two, serr := a.ProvenBegin(as(w2.Process.Pid), seededBegin("TestTwo", "fedcba9876543210"))
		require.Nil(t, serr)
		require.Equal(t, note, one.Note, "its own recording's, byte for byte")
		require.Equal(t, "", two.Note, "a note over the bound is not installed")
		require.Equal(t, "three", a.beginNote("c::TestThree"), "every note within the bound is installed")
		require.Equal(t, long[1:], a.beginNote("d::TestFour"), "a note at the bound is installed")
		a.SetTestNotes(ctx, map[string]string{"c::TestThree": "four"})
		require.Equal(t, "", a.beginNote("a::TestOne"), "an install replaces the notes")
		require.Equal(t, "four", a.beginNote("c::TestThree"))
		a.resetScopeState()
		require.Nil(t, a.testNotes, "a new session forgets them")
	})
}
