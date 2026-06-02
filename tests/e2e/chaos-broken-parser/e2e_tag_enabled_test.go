//go:build e2e

// This file flips the `e2eTagEnabled` sentinel to true when the `e2e`
// build tag is set, so `go test -tags e2e ./tests/e2e/chaos-broken-parser/...`
// runs the docker-backed chaos test. The companion file
// e2e_tag_disabled_test.go keeps it false for the default build so
// plain `go test ./...` skips this test.
package chaos_test

const e2eTagEnabled = true
