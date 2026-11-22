//go:build !darwin

package agent

// holdsUnknownCounts is empty off macOS, where holdsUnknown's reasons are
// its parsers' alone.
const holdsUnknownCounts = ""
