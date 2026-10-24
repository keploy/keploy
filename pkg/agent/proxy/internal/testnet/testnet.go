// Package testnet gives the tests of the proxy tree the loopback ports they
// expect refused or free, held for the test's life. go test ./... tests
// packages side by side, and a port a test took from a listener it then
// closed is free at once: a socket of another test, in this binary or in a
// package tested beside it, can take the port and answer the dial the test
// expects refused, or hold the port the test means to listen at again.
//
// On Linux the port is held with a socket that never listens (RefusedAddr).
// On macOS and Windows nothing is held: macOS drops a SYN sent to a bound
// socket that is not listening, by XNU's source, instead of refusing it, so a
// hold cannot give the refusal the tests expect there, and Windows is not
// checked. There the helpers give a port a listener left when it closed, as
// the tests took one before, and another socket can take it meanwhile. CI
// runs these tests on Linux, and those of package incoming on Windows too.
//
// BoundHere counts the sockets of this process at a port, so a test can tell
// a port its code still holds from one another process took once it was
// free; GiveAway hands a listener's socket to another process, for a test of
// that. Both work on Linux only.
//
// The package is for tests only: nothing outside a _test.go file imports it.
package testnet
