//go:build !e2e

// This file sets the `e2eTagEnabled` sentinel to false for the
// default build, so plain `go test ./...` skips the docker-backed
// chaos test. Flipping on `-tags e2e` switches to
// e2e_tag_enabled_test.go which sets the sentinel to true.
package chaos_test

const e2eTagEnabled = false
