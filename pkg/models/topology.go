package models

// Where a run's calls to local listeners went, under the proven switch
// (design v14 §3.1; release 1 = one run's process tree, release1-cut §5.1
// items 3 and 8). keploy relays a call to a listener the run's own process
// tree owns to the real handler (LIVE), and records and mocks a call to any
// other local listener like any dependency (mocked-local). A summary lists
// both per test, so a test whose calls keploy did not answer from its
// recording, or whose "dependency" may be the app under test, says so.

// LiveRow is a call to a listener another process of the run's own tree
// owns (the app the test command started, a database a fixture runs as a
// child process): keploy relays it to that process, so the call and what
// it returns are live, never recorded, and outside what a replay proves.
// A call a process makes to its own listener (an in-process test server)
// is relayed too, and is not listed. It is also a call to a local
// dependency outside the run's tree that keploy relays to it instead of
// recording or mocking it (Relayed): a Redis, in a keploy that records and
// mocks Redis only in Keploy Enterprise.
type LiveRow struct {
	// Test is the ID of the test open when the call sent its bytes; "" when
	// none was.
	Test string `json:"test,omitempty" yaml:"test,omitempty"`
	// Exe is the listener's program, as its process's executable names it;
	// "" for a container (Container), or when keploy could not tell which
	// process holds the listener.
	Exe string `json:"exe" yaml:"exe"`
	// Container names a container destination by its container key, as a
	// MockedRow does; empty for any other.
	Container string `json:"container,omitempty" yaml:"container,omitempty"`
	// Port is the port the call was made to.
	Port uint16 `json:"port" yaml:"port"`
	// Relayed is, for a dependency outside the run's tree keploy relays to
	// instead of recording or mocking it, what relayed it:
	// "redis_enterprise_only" (a Redis this keploy neither records nor
	// mocks), "parsing_disabled" (a record's parsing kill switch). Empty for
	// a listener of the run's tree.
	Relayed string `json:"relayed,omitempty" yaml:"relayed,omitempty"`
}

// MockedRow is a call keploy records, and a replay answers from the
// recording, to a destination on this machine: a container (reached at a
// published port or at its own address), or an address of this machine no
// process of the run's tree listens at (an app or a database already
// running, or one recorded running and not running now). If it is the app
// under test, the test tests nothing but its recording; a summary says so,
// with the fix.
type MockedRow struct {
	// Test is the ID of the test open when the connection was accepted; ""
	// when none was. A connection reused by later tests is listed for the
	// test that opened it.
	Test string `json:"test,omitempty" yaml:"test,omitempty"`
	// Addr is the address the call was made to.
	Addr string `json:"addr" yaml:"addr"`
	// Container names a container destination by its container key
	// (container:<repository>:<containerPort>@<fingerprint>, containerkey),
	// the name its recording carries: its image's repository, its own port,
	// and its configuration's fingerprint; without @<fingerprint> while
	// keploy has not read its configuration. Empty for any other
	// destination.
	Container string `json:"container,omitempty" yaml:"container,omitempty"`
	// PID and Exe are a host listener's process and its program; zero and
	// "" for a container, for a destination nothing listens at (a recorded
	// dependency that is not running, as in CI), or when keploy could not
	// tell which process holds the listener.
	PID int    `json:"pid,omitempty" yaml:"pid,omitempty"`
	Exe string `json:"exe,omitempty" yaml:"exe,omitempty"`
	// Port is the port the call was made to.
	Port uint16 `json:"port" yaml:"port"`
}
