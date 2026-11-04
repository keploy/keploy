//go:build composelib && !darwin

package docker

import "testing"

// The other half of TestComposeLibraryIsNotLinkedByDefault: a build that ASKED
// for the library must actually have it.
//
// This is the guard for the consumer that needs it — the in-pod compose runner
// runs in a distroless image with no `docker` binary and no shell, so a build
// that silently fell back to the shell-out would not fail here; it would fail
// in a customer's cluster with "docker: not found".
func TestComposeLibraryIsLinkedWithTheTag(t *testing.T) {
	if !ComposeLibrarySupported {
		t.Fatal("built with -tags composelib but ComposeLibrarySupported is false")
	}
}
