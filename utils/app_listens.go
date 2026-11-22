package utils

import (
	"context"
	"sync/atomic"
)

type appPIDKey struct{}

// WithAppPID returns ctx with room for the pid of the application command
// ExecuteCommand starts under it (a native command's; a docker command's pid
// is docker's client, not the application). Every context derived from the
// returned one shares that room: the latest start's pid is the one read.
func WithAppPID(ctx context.Context) context.Context {
	if _, ok := ctx.Value(appPIDKey{}).(*atomic.Int64); ok {
		return ctx
	}
	return context.WithValue(ctx, appPIDKey{}, new(atomic.Int64))
}

// NoteAppPID keeps pid as the application's in the room WithAppPID made in
// ctx; a no-op on a ctx without one.
func NoteAppPID(ctx context.Context, pid int) {
	if room, ok := ctx.Value(appPIDKey{}).(*atomic.Int64); ok {
		room.Store(int64(pid))
	}
}

// AppPID is the pid NoteAppPID kept in ctx, 0 when none.
func AppPID(ctx context.Context) int {
	if room, ok := ctx.Value(appPIDKey{}).(*atomic.Int64); ok {
		return int(room.Load())
	}
	return 0
}

// AppListenPorts is the TCP ports the application keploy started (AppPID)
// and its child processes listen on, sorted, each once. Nil when keploy does
// not know the application's pid, when the platform does not say (only
// Linux's /proc does), or when the processes cannot be read.
func AppListenPorts(ctx context.Context) []int {
	pid := AppPID(ctx)
	if pid <= 0 {
		return nil
	}
	return listenPortsOf(pid)
}
