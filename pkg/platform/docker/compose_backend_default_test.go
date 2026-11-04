//go:build !composelib || darwin

package docker

import "testing"

// The DEFAULT build must not link the compose library.
//
// It is reachable only through Config.InMemoryCompose, which cannot be set from
// a config file, a flag or an env var, and nothing in this repo sets it — so in
// an OSS build the library is unreachable code that still ships, in the binary
// and in the agent image injected into every recorded application pod.
// Measured on keploy's own binary (see compose_backend_unsupported.go): the tag
// adds 9.0 MiB with compose v5.5.1.
//
// If this fails, something made the library unconditional again and every OSS
// binary just grew by that much.
func TestComposeLibraryIsNotLinkedByDefault(t *testing.T) {
	if ComposeLibrarySupported {
		t.Fatal("ComposeLibrarySupported is true without the composelib tag; the library is being linked into builds that cannot reach it")
	}
}

// And the fallback has to be usable, not just absent: callers ask
// newComposeBackend and must get a refusal they can branch on, never a nil
// backend with a nil error.
func TestUnsupportedBackendRefusesRatherThanReturningNil(t *testing.T) {
	svc, err := newComposeBackend(nil, false)
	if err == nil {
		t.Fatal("want an error when the library is not linked")
	}
	if svc != nil {
		t.Fatal("want a nil service alongside the error")
	}
}
