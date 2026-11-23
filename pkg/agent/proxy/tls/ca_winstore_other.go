//go:build !windows

package tls

// userRootHolds has no Windows store to read here: installWindowsCA runs only
// on Windows, and rows swap it (userRootHoldsFn).
func userRootHolds([]byte) (bool, error) { return false, errNoWindowsStore }

// machineRootHolds: the same, for the machine's store (machineRootHoldsFn).
func machineRootHolds([]byte) (bool, error) { return false, errNoWindowsStore }

// tokenElevated: only Windows tokens are read; rows swap it (tokenElevatedFn).
func tokenElevated() bool { return false }

// rootStoreCerts has no Windows store to read here: the retired CA's sweep
// of the Windows stores does nothing off Windows, and rows swap it
// (rootStoreCertsFn).
func rootStoreCerts(winRootStore) ([][]byte, error) { return nil, errNoWindowsStore }

// rootStoreDelete: the same, for a removal (rootStoreDeleteFn).
func rootStoreDelete(winRootStore, []byte) (bool, error) { return false, errNoWindowsStore }
