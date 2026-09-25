package record

import "go.keploy.io/server/v3/pkg/models"

// appStopError is what Start returns when the recording ended because of the
// user's application: its message is the stop reason Start logs, word for word,
// and the models.AppError the application ended with stays reachable beneath
// it.
//
// It replaces fmt.Errorf("%s", stopReason), which kept the words and dropped
// everything a caller acts on. `keploy record` exits with the application's
// own code when the application exited (cli/record.go), and that needs the
// AppError's type and ExitCode; a caller reading the raw process status needs
// the *exec.ExitError inside it.
type appStopError struct {
	reason string
	app    models.AppError
}

func (e *appStopError) Error() string { return e.reason }

// Unwrap exposes the AppError, and the error it carries: AppError has no
// Unwrap of its own, so without the second entry the application's
// *exec.ExitError -- and a Keploy-side tag such as
// utils.ErrEnvironmentUnsupported that utils.ExitCodeFor reads -- would stop
// at the AppError.
func (e *appStopError) Unwrap() []error {
	if e.app.Err == nil {
		return []error{e.app}
	}
	return []error{e.app, e.app.Err}
}

// As serves a *models.AppError target. AppError's Error() has a value
// receiver, so both AppError and *AppError are errors, and a caller may ask
// errors.As for either; the value in the chain satisfies only the first.
func (e *appStopError) As(target any) bool {
	p, ok := target.(**models.AppError)
	if !ok {
		return false
	}
	app := e.app
	*p = &app
	return true
}
