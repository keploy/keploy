package proxy

import (
	"fmt"
	"os"
	"strings"
	"sync/atomic"

	"go.keploy.io/server/v3/pkg/models"
)

// exactViewVerify turns on the exact view's verify mode: each read the view
// keeps materialised (an index it built once instead of on every call) is
// recomputed the way the view computed it before it kept it, and any
// difference panics (verifyExactRead). A stale index would hand a parser a
// mock the view no longer shows, or hide one it shows: a wrong answer, or a
// false green. On in this package's rows (TestMain) and wherever
// KEPLOY_EXACT_VIEW_VERIFY=1 (CI's lanes); off in a user's run, where the
// recomputation is the cost the index saves.
var exactViewVerify atomic.Bool

func init() { exactViewVerify.Store(os.Getenv("KEPLOY_EXACT_VIEW_VERIFY") == "1") }

// verifyExactRead panics when got, a materialised read of the exact view, is
// not want, the same read recomputed: the same mocks, by identity, in the
// same order.
func verifyExactRead(what string, want, got []*models.Mock) {
	same := len(want) == len(got)
	for i := 0; same && i < len(want); i++ {
		same = want[i] == got[i]
	}
	if same {
		return
	}
	panic(fmt.Sprintf("exact view verify: %s: the kept read gives [%s], recomputed it gives [%s]", what, verifyNames(got), verifyNames(want)))
}

func verifyNames(mocks []*models.Mock) string {
	names := make([]string, 0, len(mocks))
	for _, mk := range mocks {
		if mk == nil {
			names = append(names, "<nil>")
			continue
		}
		names = append(names, mk.Name)
	}
	return strings.Join(names, ", ")
}
