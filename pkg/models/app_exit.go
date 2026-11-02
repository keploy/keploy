package models

import "context"

type appExitKey struct{}

// WithAppExit returns ctx carrying told: the application's runner calls it,
// once, as soon as it learns that the application exited on its own (an exit
// keploy did not ask for), before its run returns. Under docker compose that
// is the app's container's die: compose's up returns only once it has stopped
// every other service of the stack, each within its own stop grace, and until
// then keploy took the app for running, began tests and re-sends against it,
// and, when the set ended first, lost the exit to its own teardown. The run's
// return still says the exit as before; told lets the caller act on it at
// once. told must not block.
func WithAppExit(ctx context.Context, told func(AppError)) context.Context {
	return context.WithValue(ctx, appExitKey{}, told)
}

// AppExitOf is the listener ctx carries (WithAppExit), nil when it carries
// none.
func AppExitOf(ctx context.Context) func(AppError) {
	if ctx == nil {
		return nil
	}
	told, _ := ctx.Value(appExitKey{}).(func(AppError))
	return told
}
