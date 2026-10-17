package utils

import "time"

// StopEnd is how a stop's wait for an app's exit ended (WaitExitByEvents).
type StopEnd int

const (
	// StopExited: the app exited, on its exit event, or every process it
	// was watched by is gone.
	StopExited StopEnd = iota
	// StopStalled: it made no progress on its way out for StopStallBudget
	// of keploy's runnable time. A kill is due, and is said.
	StopStalled
	// StopPressed: the user pressed Ctrl+C at the stop's offer to end it
	// at once. A kill is due, and is said as the user's.
	StopPressed
)

// StopOffer is how long after the run's interrupt a stop that waits for an
// app's exit offers to kill it at the user's next Ctrl+C (StopNowNotice), as
// the stop of the daemons an app leaves behind does. A test sets it shorter.
var StopOffer = 5 * time.Second
