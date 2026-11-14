//go:build !windows

package tls

import "errors"

var errNoWindowsStore = errors.New("there is no Windows certificate store on this platform")

// userRootHolds has no Windows store to read here: installWindowsCA runs only
// on Windows, and rows swap it (userRootHoldsFn).
func userRootHolds([]byte) (bool, error) { return false, errNoWindowsStore }

// machineRootHolds: the same, for the machine's store (machineRootHoldsFn).
func machineRootHolds([]byte) (bool, error) { return false, errNoWindowsStore }

// tokenElevated: only Windows tokens are read; rows swap it (tokenElevatedFn).
func tokenElevated() bool { return false }
