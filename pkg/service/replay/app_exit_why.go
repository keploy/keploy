package replay

import (
	"fmt"
	"sync"

	"go.keploy.io/server/v3/pkg/models"
)

// unansweredTest is the test a set sent its application last that the
// application has not answered yet: the test an exit of the application held,
// which the set's report names (appExitWhy). A set sends its tests one at a
// time, so a test sent replaces the one before it, answered or not.
type unansweredTest struct {
	mu   sync.Mutex
	name string
}

// sent marks the test named sent to the application.
func (u *unansweredTest) sent(name string) {
	u.mu.Lock()
	u.name = name
	u.mu.Unlock()
}

// answered marks the test named answered: an exit of the application from
// now on did not hold it.
func (u *unansweredTest) answered(name string) {
	u.mu.Lock()
	if u.name == name {
		u.name = ""
	}
	u.mu.Unlock()
}

// held is the test sent and not answered, "" when there is none.
func (u *unansweredTest) held() string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.name
}

// appExitWhy says how the application's own exit stopped its set, for the
// set's report: the exit code, where the run knows it, and the test the
// application had not answered when keploy learned of the exit (held). It is
// "" for an end of the run that is no exit of the application's own: keploy's
// stop of it is a cancel (models.ErrCtxCanceled).
func appExitWhy(e models.AppError, held string) string {
	if e == (models.AppError{}) || e.AppErrorType == models.ErrCtxCanceled {
		return ""
	}
	why := "it exited"
	switch {
	case e.ExitCode > 0:
		why += fmt.Sprintf(" with code %d", e.ExitCode)
	case e.AppErrorType == models.ErrAppStopped:
		// The runners say a clean exit so: its code is 0. Another end
		// with no code above 0 is one whose code the run does not know.
		why += " with code 0"
	}
	if held != "" {
		why += " before it answered test " + held
	}
	return why
}
