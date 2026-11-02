package replay

import (
	"context"
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

// keptAppExit is an exit of its own of the application keploy runs once for
// the whole run (a compose stack kept across sets, --keep-app-alive), as its
// runner tells it at the exit (models.WithAppExit): the set running then
// halts on it at once, and every later set finds it told. Under docker
// compose the runner tells it at the app's container's die, while compose's
// up still waits out the rest of the stack: until then keploy took the app
// for running.
type keptAppExit struct {
	once sync.Once
	// told is closed once the exit is told; exit is then set.
	told chan struct{}
	exit models.AppError
}

func newKeptAppExit() *keptAppExit {
	return &keptAppExit{told: make(chan struct{})}
}

// tell keeps the exit e, once: the first word is the exit's.
func (k *keptAppExit) tell(e models.AppError) {
	k.once.Do(func() {
		k.exit = e
		close(k.told)
	})
}

// listenedTo is ctx carrying k's tell (models.WithAppExit), with k: the
// context the run's application runs under.
func listenedTo(ctx context.Context) (context.Context, *keptAppExit) {
	k := newKeptAppExit()
	return models.WithAppExit(ctx, k.tell), k
}
