//go:build darwin

package agent

// holdsUnknownCounts is the reason macOS adds to holdsUnknown's: a
// connection whose socket's count of what it received keploy cannot read
// (the proxy's xnuCountsUnknown, which names it) leaves its requests unknown
// too, whatever its parser says.
const holdsUnknownCounts = ", or macOS does not give keploy the count of what the connection received"
