package models

// ComposeAppStart is what keploy read of the application's container of a
// docker compose stack as it started it again for a test-set, under the
// proven switch (StartComposeApp): whether the application that runs now is
// a new one, whose capture losses start afresh, or the one before, which
// carries what its processes lost into the test-set.
type ComposeAppStart int

const (
	// ComposeAppUnread: the docker daemon gave keploy no answer of the
	// container before keploy started it, for keploy's stall bound of
	// runnable time (or the start failed). keploy cannot tell whether its
	// stop at an earlier test-set's end ended the application, so it takes
	// the application for the one before: no new application is assumed
	// where none may have started.
	ComposeAppUnread ComposeAppStart = iota
	// ComposeAppStartedAnew: the daemon said the container was not running,
	// so the application keploy started is a new one.
	ComposeAppStartedAnew
	// ComposeAppFoundRunning: the daemon said the container was running, so
	// the start was the daemon's no-op and the application is the one before
	// (a stop that did not end it, or a start keploy was told failed that
	// the daemon carried out).
	ComposeAppFoundRunning
)

// Anew says the application keploy started is a new one.
func (s ComposeAppStart) Anew() bool { return s == ComposeAppStartedAnew }
