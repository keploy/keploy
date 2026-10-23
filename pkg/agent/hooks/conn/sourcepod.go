package conn

import "context"

// sourcePodCtxKey carries the source pod name through the capture context.
// Reentrancy seam: the enterprise DaemonSet agent sets it per connection
// (WithSourcePod) so Capture can stamp TestCase.SourcePod for per-pod
// attribution. OSS single-app callers never set it, so SourcePod stays empty.
type sourcePodCtxKey struct{}

// WithSourcePod returns a context carrying the source pod name. A no-op-shaped
// empty pod is allowed (stamps an empty SourcePod, same as not setting it).
func WithSourcePod(ctx context.Context, pod string) context.Context {
	return context.WithValue(ctx, sourcePodCtxKey{}, pod)
}

// SourcePodFromContext returns the source pod name carried on ctx, or "" when
// none was set.
func SourcePodFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if pod, ok := ctx.Value(sourcePodCtxKey{}).(string); ok {
		return pod
	}
	return ""
}

// servedByCtxKey carries the process that serves the capture context's
// connection: its TGID in the initial PID namespace. Reentrancy seam: the
// enterprise low-latency capture sets it per connection (WithServedBy) so
// Capture stamps TestCase.ServedBy, by which the record route tells which test
// cases a scoped span of unrecorded traffic can have stranded. Unset, the
// stamp is 0, which every such span takes as stranded.
type servedByCtxKey struct{}

// WithServedBy returns a context carrying tgid, the process that serves the
// connection whose test cases the context's Capture builds.
func WithServedBy(ctx context.Context, tgid uint32) context.Context {
	return context.WithValue(ctx, servedByCtxKey{}, tgid)
}

// ServedByFromContext returns the serving process carried on ctx, 0 when none
// was set.
func ServedByFromContext(ctx context.Context) uint32 {
	if ctx == nil {
		return 0
	}
	if tgid, ok := ctx.Value(servedByCtxKey{}).(uint32); ok {
		return tgid
	}
	return 0
}
