package app

import (
	"context"
	"errors"
	"testing"

	"github.com/docker/docker/api/types/container"
	"go.keploy.io/server/v3/pkg/platform/docker"
	"go.uber.org/zap"
)

// nameLister is a docker client that answers ContainerList from canned data.
type nameLister struct {
	docker.Client
	result []container.Summary
	err    error
	// filters records the name filter each call was made with.
	filters []string
}

func (n *nameLister) ContainerList(_ context.Context, options container.ListOptions) ([]container.Summary, error) {
	n.filters = append(n.filters, options.Filters.Get("name")...)
	return n.result, n.err
}

// TestContainerNameFreeDetectsAFreeName is the healthy path.
//
// This check used to shell out to `docker ps -aq --filter ...` and read "stdout
// is empty" as the verdict, which made ANY byte on stderr — a malformed
// ~/.docker/config.json, a credential-helper warning, a deprecation notice —
// mean "this name is taken", for every name, permanently. The Engine API
// returns a typed list, so there is no stream to confuse; these tests pin the
// verdict itself, which is what the callers depend on.
//
// The cost of getting it wrong is not cosmetic: ensureContainerNameFree polls
// its full 90s budget before every docker-run start, and isDockerRunNameConflict
// treats every exit-125 as a name conflict — retrying a genuinely broken run
// (bad image, unsatisfiable mount) with a 90s removal between attempts, so the
// real error takes minutes to appear.
func TestContainerNameFreeDetectsAFreeName(t *testing.T) {
	lister := &nameLister{}
	a := &App{logger: zap.NewNop(), docker: lister}

	if !a.containerNameFree("keploy-v3") {
		t.Fatal("an unused container name read as taken; every docker-run start then waits out the " +
			"full 90s name-free budget, and every exit-125 is retried as a name conflict")
	}
}

// TestContainerNameFreeStillDetectsATakenName keeps the guard honest — without
// it, hardcoding `return true` would satisfy the test above.
func TestContainerNameFreeStillDetectsATakenName(t *testing.T) {
	lister := &nameLister{result: []container.Summary{{ID: "3f9a1c2b4d5e"}}}
	a := &App{logger: zap.NewNop(), docker: lister}

	if a.containerNameFree("keploy-v3") {
		t.Fatal("a name held by a real container reported as free; the next `docker run --name` " +
			"would hit a conflict")
	}
}

// TestContainerNameFreeTreatsAQueryErrorAsInUse pins the conservative default.
// Reading an unreachable daemon as "free" would send the caller straight into a
// create that then fails on the very conflict this check exists to avoid.
func TestContainerNameFreeTreatsAQueryErrorAsInUse(t *testing.T) {
	lister := &nameLister{err: errors.New("daemon unreachable")}
	a := &App{logger: zap.NewNop(), docker: lister}

	if a.containerNameFree("keploy-v3") {
		t.Fatal("a failed availability query reported the name as free")
	}
}

// TestContainerNameFreeAnchorsTheNameFilter pins the regex anchoring. The
// daemon's name filter is a substring regex: unanchored, "keploy-v3" also
// matches "keploy-v3-old", and an unrelated container would make the name read
// as permanently taken.
func TestContainerNameFreeAnchorsTheNameFilter(t *testing.T) {
	lister := &nameLister{}
	a := &App{logger: zap.NewNop(), docker: lister}

	a.containerNameFree("keploy-v3")

	if len(lister.filters) != 1 || lister.filters[0] != "^/keploy-v3$" {
		t.Fatalf("name filter = %v; want [^/keploy-v3$] — an unanchored filter matches any container "+
			"whose name merely contains this one", lister.filters)
	}
}
