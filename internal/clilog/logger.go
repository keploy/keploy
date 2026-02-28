package clilog

import (
	"context"
	"log/slog"
	"os"
	"runtime"
)

type ctxKey struct{}

var key ctxKey

func New(level slog.Level, attrs ...slog.Attr) *slog.Logger {
	h := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: level,
	})
	l := slog.New(h)
	if len(attrs) > 0 {
		l = l.With(attrs...)
	}
	return l
}

func WithContext(ctx context.Context, l *slog.Logger) context.Context {
	return context.WithValue(ctx, key, l)
}

func FromContext(ctx context.Context) *slog.Logger {
	if v, ok := ctx.Value(key).(*slog.Logger); ok {
		return v
	}
	return slog.Default()
}

func CommandLogger(base *slog.Logger, command string, version string) *slog.Logger {
	return base.With(
		slog.String("command", command),
		slog.String("op_system", runtime.GOOS),
		slog.String("version", version),
	)
}

